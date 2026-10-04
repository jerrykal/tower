package ui

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/hosts"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/stream"
	"github.com/jerrykal/tower/internal/tmux"
)

// Towerd is what the dashboard asks of its own machine's towerd.
type Towerd interface {
	View(ctx context.Context, a proto.ViewArgs) (proto.Dash, error)
	Watch(ctx context.Context, gen uint64) (uint64, error)
	Act(ctx context.Context, r proto.Request) (proto.Ack, error)
	// Retry asks the home to retry every host that is down now.
	Retry(ctx context.Context) error
}

// Tmux runs one tmux command on the dashboard's own server.
type Tmux interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// Calls is the Towerd of a client.Client: one local call each.
type Calls struct{ C *client.Client }

func (t Calls) View(ctx context.Context, a proto.ViewArgs) (proto.Dash, error) {
	var d proto.Dash
	err := t.C.Call(ctx, proto.CallView, a, &d)
	return d, err
}

func (t Calls) Watch(ctx context.Context, gen uint64) (uint64, error) {
	var r proto.WatchResult
	err := t.C.Call(ctx, proto.CallWatch, proto.WatchArgs{Gen: gen}, &r)
	return r.Gen, err
}

func (t Calls) Act(ctx context.Context, r proto.Request) (proto.Ack, error) {
	var a proto.Ack
	err := t.C.Call(ctx, proto.CallAct, r, &a)
	return a, err
}

func (t Calls) Retry(ctx context.Context) error {
	return t.C.Call(ctx, proto.CallNetChange, nil, nil)
}

// Conn is one dashboard's link to the world: its towerd, the tmux server
// it runs on, and who it runs for. The popup, the loop's picker and the
// scripted entry points share it, so they share every code path behind a
// key.
type Conn struct {
	Towerd Towerd
	Tmux   Tmux     // nil in the loop's picker, which never runs tmux
	Client string   // TOWER_CLIENT: pid:created:name of the pressing client
	Loop   string   // the loop's own picker: its loop id
	Pick   bool     // the loop's picker: ⏎ returns the target
	Hosts  HostList // the host list, on the home's machine; nil: not edited here

	mu   sync.Mutex
	info *clientInfo // the client's tty and session, read once
}

// Dial ensures this machine's towerd and returns a Conn for client
// (TOWER_CLIENT, may be empty). It takes towerd's resolved tmux binary, so
// no tmux call goes through a version manager's shim.
func Dial(ctx context.Context, e *config.Env, clientID string) (*Conn, error) {
	c := client.New(e)
	st, err := c.Ensure(ctx)
	if err != nil {
		return nil, err
	}
	tmux.UseBin(st.TmuxBin)
	return &Conn{
		Towerd: Calls{C: c},
		Tmux:   tmux.Server{Bin: tmux.Bin(), Args: e.Tmux},
		Client: clientID,
		Hosts:  hosts.New(e),
	}, nil
}

func (c *Conn) viewArgs() proto.ViewArgs {
	return proto.ViewArgs{Client: c.Client, Loop: c.Loop}
}

// ackTimeout is a dashboard request's deadline (TOWER_ACK_TIMEOUT).
func ackTimeout() time.Duration { return config.Duration("TOWER_ACK_TIMEOUT", 5*time.Second) }

// act sends one request with a fresh id and the dashboard's deadline. The
// call itself may take a little longer than the deadline, so towerd's own
// refusal, which says why, is what the user sees.
func (c *Conn) act(ctx context.Context, r proto.Request) (proto.Ack, error) {
	return c.actWithin(ctx, r, ackTimeout())
}

// actWithin is act with deadline d.
func (c *Conn) actWithin(ctx context.Context, r proto.Request, d time.Duration) (proto.Ack, error) {
	r.ID = config.NewID()
	r.Deadline = stream.Now() + d.Milliseconds() // the clock towerd checks it against
	ctx, cancel := context.WithTimeout(ctx, d+time.Second)
	defer cancel()
	a, err := c.Towerd.Act(ctx, r)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return a, errors.New("no answer in " + d.String())
		}
		return a, err
	}
	if !a.OK {
		if a.Err == "" {
			a.Err = "refused"
		}
		return a, errors.New(a.Err)
	}
	return a, nil
}

// clientInfo is what tmux says of the pressing client.
type clientInfo struct {
	tty     string
	session string // session id
	window  string // window id
}

// clientInfo reads the client's tty and where it is, once. The popup asks
// as it opens, off the first frame's path, so a hand-off never waits for
// it.
func (c *Conn) clientInfo(ctx context.Context) (*clientInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info != nil {
		return c.info, nil
	}
	cl, err := proto.ParseClient(c.Client)
	if err != nil {
		return nil, err
	}
	if c.Tmux == nil {
		return nil, errors.New("no tmux server")
	}
	out, err := c.Tmux.Run(ctx, "display-message", "-p", "-c", cl.Name,
		"#{client_tty}\t#{session_id}\t#{window_id}")
	if err != nil {
		return nil, err
	}
	f, err := tmux.Fields(strings.TrimRight(out, "\n"), 3)
	if err != nil {
		return nil, err
	}
	c.info = &clientInfo{tty: f[0], session: f[1], window: f[2]}
	return c.info, nil
}

// alive reports whether a process with pid exists.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// writeTTY writes seq to a terminal without ever blocking on it.
func writeTTY(tty, seq string) error {
	f, err := os.OpenFile(tty, os.O_WRONLY|os.O_APPEND|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(seq)
	return err
}
