package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// errGone is ⏎ on a row whose session (or window) no longer exists.
var errGone = errors.New("selection is gone")

// errNeedsLoop is ⏎ to another server from a client no attach loop owns:
// only a loop can move a terminal between servers.
var errNeedsLoop = errors.New("⏎ on another host needs the attach loop (run tower outside tmux)")

// target is a row resolved against a view: the ref to act on and its
// host.
type target struct {
	ref  proto.Ref
	host *proto.Host
	sess *proto.Session
}

// resolve finds the row k names in d, by ids, as it is now: a renamed
// session is still found; a killed one, or one of a server that restarted
// since the row was drawn, is not.
func resolve(d *proto.Dash, k rowKey) (target, error) {
	h := d.View.HostByID(k.Host)
	if h == nil {
		return target{}, errGone
	}
	if k.Session == "" {
		return target{host: h, ref: proto.Ref{Host: h.ID, Name: h.Name, Inst: h.Inst}}, nil
	}
	if h.Inst != k.Inst {
		return target{}, fmt.Errorf("%s restarted since it was listed; pick again", h.Name)
	}
	for si := range h.Sessions {
		s := &h.Sessions[si]
		if s.ID != k.Session {
			continue
		}
		t := target{host: h, sess: s, ref: proto.Ref{Host: h.ID, Name: h.Name, Inst: h.Inst, Session: s.ID, Label: s.Name}}
		if k.Window == "" {
			return t, nil
		}
		for _, w := range s.Windows {
			if w.ID == k.Window {
				t.ref.Window = w.ID
				return t, nil
			}
		}
		return target{}, errGone
	}
	return target{}, errGone
}

// reachable refuses a host that is not up: requests and hand-offs to it
// would fail or hang.
func reachable(h *proto.Host) error {
	if h.Reachable() {
		return nil
	}
	if h.Status == proto.StatusStalled {
		// towerd's own words for it, as the loop and a refused switch say.
		return fmt.Errorf("%s is not responding", h.Name)
	}
	return fmt.Errorf("%s is %s", h.Name, hostStatus(h))
}

// outcome of ⏎.
type outcome int

const (
	outQuit   outcome = iota // done: the dashboard closes
	outChoice                // the loop's picker: return the target
)

// enter is ⏎ on the row k: read the view afresh, re-resolve k in it, and
// act on what is there now. In the loop's picker the target is returned;
// in a popup it is a switch-client on this server or a hand-off to
// another.
func (c *Conn) enter(ctx context.Context, k rowKey) (outcome, proto.Ref, error) {
	config.Mark("dash: enter")
	if k.Session == "" {
		return 0, proto.Ref{}, errors.New("no session there: ^n makes one")
	}
	d, err := c.Towerd.View(ctx, c.viewArgs())
	if err != nil {
		return 0, proto.Ref{}, err
	}
	t, err := resolve(&d, k)
	if err != nil {
		return 0, proto.Ref{}, err
	}
	if err := reachable(t.host); err != nil {
		return 0, proto.Ref{}, err
	}
	if c.Pick {
		return outChoice, t.ref, nil
	}
	if t.ref.Host == d.Self {
		return outQuit, t.ref, c.switchHere(ctx, t.ref)
	}
	if !d.Owned {
		return 0, proto.Ref{}, errNeedsLoop
	}
	return outQuit, t.ref, c.handoff(ctx, t.ref)
}

// switchHere moves the pressing client on this server, as tmux's own
// session picker does; the attach goes on.
func (c *Conn) switchHere(ctx context.Context, r proto.Ref) error {
	cl, err := parseClient(c.Client)
	if err != nil {
		return err
	}
	args := []string{"switch-client", "-c", cl.name, "-t", r.Session}
	if r.Window != "" {
		args = append(args, ";", "select-window", "-t", r.Window)
	}
	_, err = c.Tmux.Run(ctx, args...)
	return err
}

// handoff asks for a switch to another server (protocol.md, Hand-off).
// The switch is committed once towerd has stored it. When the loop ended
// this client itself (Ended), wait for the client to go, so the popup
// never closes first and tmux never redraws under the loop's held frame.
// Otherwise hold the frame on the client's terminal and detach the client
// with exit 42, which the loop takes from there.
func (c *Conn) handoff(ctx context.Context, r proto.Ref) error {
	cl, err := parseClient(c.Client)
	if err != nil {
		return err
	}
	req := proto.Request{Op: proto.OpSwitch, Target: r, Client: c.Client, Nonce: config.NewID()}
	if g, err := strconv.Atoi(os.Getenv("TOWER_TEST_GEN")); err == nil {
		req.Gen = g // test hook: a dashboard claiming another attach
	}
	ack, err := c.act(ctx, req)
	if err != nil {
		return fmt.Errorf("hand-off refused: %v", err)
	}
	if os.Getenv("TOWER_TEST_CRASH") == "after-switch" {
		os.Exit(3) // test hook: the dashboard dies once its switch is stored
	}
	if ack.Ended {
		for t0 := time.Now(); alive(cl.pid) && time.Since(t0) < 3*time.Second; {
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	}
	tty := c.hold(ctx)
	_, err = c.Tmux.Run(ctx, "detach-client", "-t", cl.name, "-E", "exit 42")
	if err != nil && alive(cl.pid) {
		// The client stays: let its terminal draw again.
		if tty != "" {
			writeTTY(tty, syncEnd)
		}
		return err
	}
	// A client already gone (the loop ended it after all) is no failure,
	// and the hold is the next client's to release.
	return nil
}

// hold writes the frame hold to the pressing client's terminal and
// returns its tty, or "" when it does not (TOWER_SYNC=0, or the tty cannot
// be written, as under some sshds). tmux ends each of its own synced
// frames with the end of the mode, so this re-holds after any frame tmux
// drew since the loop's hold.
func (c *Conn) hold(ctx context.Context) string {
	if !config.Flag("TOWER_SYNC", true) || os.Getenv("TOWER_TEST_NOTTY") != "" {
		return ""
	}
	info, err := c.clientInfo(ctx)
	if err != nil || info.tty == "" {
		return ""
	}
	if writeTTY(info.tty, syncBegin) != nil {
		return ""
	}
	return info.tty
}

// done is how an action's outcome reads: "kill on B: done", with towerd's
// note ("already gone") after it.
func done(op, host, note string) string {
	s := op + " on " + host + ": done"
	if note != "" {
		s += " (" + note + ")"
	}
	return s
}

// failed is how an action's error reads.
func failed(op, host string, err error) string {
	return op + " on " + host + ": " + err.Error()
}

// kill kills the session (or window) t names.
func (c *Conn) kill(ctx context.Context, t target) (proto.Ack, error) {
	kind := proto.KindSession
	if t.ref.Window != "" {
		kind = proto.KindWindow
	}
	return c.act(ctx, proto.Request{Op: proto.OpKill, Target: t.ref, Kind: kind})
}

// rename renames the session (or window) t names.
func (c *Conn) rename(ctx context.Context, t target, name string) (proto.Ack, error) {
	kind := proto.KindSession
	if t.ref.Window != "" {
		kind = proto.KindWindow
	}
	return c.act(ctx, proto.Request{Op: proto.OpRename, Target: t.ref, Kind: kind, Name: name})
}

// newSession makes a session on h; an empty name leaves it to tmux.
func (c *Conn) newSession(ctx context.Context, h *proto.Host, name string) (proto.Ack, error) {
	ref := proto.Ref{Host: h.ID, Name: h.Name, Inst: h.Inst}
	return c.act(ctx, proto.Request{Op: proto.OpNew, Target: ref, Kind: proto.KindSession, Name: name})
}

// startingNote is what the dashboard says while a new session starts a
// host's tmux server, which may take seconds with a plugin manager.
func startingNote(h *proto.Host) string {
	if h.NoServer {
		return "starting tmux on " + h.Name + "…"
	}
	return ""
}

// capture asks for the active pane of the session's active window (or of
// the window t names).
func (c *Conn) capture(ctx context.Context, t target) (string, error) {
	ref := t.ref
	if ref.Window == "" && t.sess != nil {
		for _, w := range t.sess.Windows {
			if w.Active {
				ref.Window = w.ID
			}
		}
	}
	a, err := c.act(ctx, proto.Request{Op: proto.OpCapture, Target: ref})
	return a.Text, err
}

// previewHeader is the preview's first line: "B:bravo  ($1)  1:fish
// 2:nvim", from the view alone.
func previewHeader(h *proto.Host, s *proto.Session) string {
	var b strings.Builder
	b.WriteString(h.Name + ":" + s.Name + "  (" + s.ID + ")")
	for _, w := range s.Windows {
		b.WriteString("  " + strconv.Itoa(w.Index) + ":" + w.Name)
	}
	return b.String()
}
