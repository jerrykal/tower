// Package client is how every tower process but towerd itself reaches
// towerd: one call per connection, and Ensure, which makes sure a current
// towerd answers, starting, upgrading or replacing it as needed.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/version"
)

// ErrNoTowerd means nothing listens on the towerd socket.
var ErrNoTowerd = errors.New("towerd is not running")

// Client calls the towerd of one Env.
type Client struct {
	Env     *config.Env
	Version string
}

// New is a client of e's towerd at this build's version.
func New(e *config.Env) *Client { return &Client{Env: e, Version: version.Version} }

// Dial connects to the towerd socket.
func (c *Client) Dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Env.Socket())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoTowerd, err)
	}
	return conn, nil
}

// Call makes one call: op with args, decoding the answer into result
// (which may be nil). Closing ctx abandons the call, which tells towerd
// to stop waiting on the caller's behalf.
func (c *Client) Call(ctx context.Context, op string, args, result any) error {
	conn, err := c.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return c.CallOn(ctx, conn, op, args, result)
}

// CallOn makes a call on an open connection.
func (c *Client) CallOn(ctx context.Context, conn net.Conn, op string, args, result any) error {
	call := proto.Call{Op: op, Version: c.Version}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		call.Args = b
	}
	b, err := json.Marshal(call)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return c.ctxErr(ctx, err)
	}
	line, err := bufio.NewReaderSize(conn, 64<<10).ReadBytes('\n')
	if err != nil {
		return c.ctxErr(ctx, err)
	}
	var r proto.Reply
	if err := json.Unmarshal(line, &r); err != nil {
		return fmt.Errorf("towerd: bad reply: %w", err)
	}
	if r.Err != "" {
		return &CallError{Op: op, Msg: r.Err}
	}
	if result != nil && len(r.Result) > 0 {
		return json.Unmarshal(r.Result, result)
	}
	return nil
}

func (c *Client) ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// CallError is towerd's refusal of a call.
type CallError struct {
	Op  string
	Msg string
}

func (e *CallError) Error() string { return e.Msg }

// Ensure returns the status of a towerd at least as new as this build,
// starting one when none answers, asking an older one to make way, and
// replacing one that holds the lock but does not answer.
func (c *Client) Ensure(ctx context.Context) (*proto.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var lastStart time.Time
	var stuckSince time.Time
	var lastErr error
	for {
		st, err := c.status(ctx, time.Second)
		switch {
		case err == nil && proto.Newer(c.Version, st.Version):
			// An older towerd makes way; an older caller never downgrades
			// a newer one (the other branch).
			c.Call(ctx, proto.CallStop, proto.StopArgs{IfOlderThan: c.Version}, nil)
			c.waitGone(ctx)
			stuckSince = time.Time{}
			continue
		case err == nil:
			return st, nil
		case errors.Is(err, ErrNoTowerd):
			stuckSince = time.Time{}
			if time.Since(lastStart) >= 100*time.Millisecond {
				lastStart = time.Now()
				if serr := c.start(); serr != nil {
					return nil, serr
				}
			}
		default:
			// Connected but no answer: a towerd that is stopped or stuck.
			if stuckSince.IsZero() {
				stuckSince = time.Now()
			} else if time.Since(stuckSince) >= time.Second {
				if c.killWedged() {
					stuckSince = time.Time{}
					lastStart = time.Time{}
				}
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("towerd did not start: %v", lastErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (c *Client) status(ctx context.Context, d time.Duration) (*proto.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var st proto.Status
	if err := c.Call(ctx, proto.CallStatus, proto.StatusArgs{}, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// waitGone waits until nothing answers on the socket (or ctx ends).
func (c *Client) waitGone(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := c.Dial(ctx)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
}

// start launches `tower towerd` detached from this process: its own
// session, stdio on /dev/null. The lock decides between starters.
func (c *Client) start() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"towerd"}
	if len(c.Env.Tmux) > 0 {
		args = append(args, "--tmux", strings.Join(c.Env.Tmux, " "))
	}
	cmd := exec.Command(self, args...)
	cmd.Env = append(os.Environ(), "TOWER_MKEY="+c.Env.MKey)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Dir = "/"
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap a starter that lost the lock
	return nil
}

// killWedged SIGKILLs the towerd in towerd.pid if it holds the lock and
// is a towerd: one that accepts connections but answers nothing keeps any
// other from starting.
func (c *Client) killWedged() bool {
	b, err := os.ReadFile(c.Env.State("towerd.pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return false
	}
	if !LockHeld(c.Env.LockPath()) || !IsTowerd(pid) {
		return false
	}
	return syscall.Kill(pid, syscall.SIGKILL) == nil
}

// LockHeld reports whether some process holds the flock on path.
func LockHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// IsTowerd reports whether pid runs `… towerd` (and not a bridge).
func IsTowerd(pid int) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	f := strings.Fields(string(out))
	return len(f) >= 2 && f[1] == "towerd" && !strings.Contains(string(out), "--stdio")
}
