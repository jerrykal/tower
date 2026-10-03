package towerd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/stream"
	"github.com/jerrykal/tower/internal/tmux"
)

// answerTTL is how long the towerd that ran a request remembers its
// answer, for a repeat of the same id.
const answerTTL = 10 * time.Minute

// newServerWait is how long a new session may take on a host with no
// server: the server reads the user's config first, which can take
// seconds with a plugin manager.
const newServerWait = 15 * time.Second

// answers remembers the answer to every request this towerd ran, by id,
// and makes a repeat that arrives while the first runs wait for it.
type answers struct {
	mu      sync.Mutex
	done    map[string]answer
	running map[string]chan struct{}
}

type answer struct {
	ack proto.Ack
	at  time.Time
}

func newAnswers() *answers {
	return &answers{done: map[string]answer{}, running: map[string]chan struct{}{}}
}

func (a *answers) run(id string, fn func() *proto.Ack) *proto.Ack {
	if id == "" {
		return fn()
	}
	a.mu.Lock()
	if e, ok := a.done[id]; ok {
		a.mu.Unlock()
		ack := e.ack
		return &ack
	}
	if ch, ok := a.running[id]; ok {
		a.mu.Unlock()
		<-ch
		a.mu.Lock()
		e := a.done[id]
		a.mu.Unlock()
		ack := e.ack
		return &ack
	}
	ch := make(chan struct{})
	a.running[id] = ch
	a.mu.Unlock()
	ack := fn()
	ack.ID = id
	a.mu.Lock()
	a.done[id] = answer{ack: *ack, at: time.Now()}
	delete(a.running, id)
	a.mu.Unlock()
	close(ch)
	return ack
}

func (a *answers) expire() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, e := range a.done {
		if time.Since(e.at) > answerTTL {
			delete(a.done, id)
		}
	}
}

// mutates reports whether op changes the session list, so its answer
// waits for a re-read (read your writes).
func mutates(op string) bool {
	switch op {
	case proto.OpKill, proto.OpRename, proto.OpNew, proto.OpDup:
		return true
	}
	return false
}

// runLocal runs req on this machine's server: once per request id, never
// after its deadline (in this machine's clock).
func (d *Daemon) runLocal(req *proto.Request) *proto.Ack {
	return d.answers.run(req.ID, func() *proto.Ack {
		start := time.Now()
		ack := d.perform(req)
		d.logf("act: %s %s on %s: ok=%v %s%s (%v)", req.Op, req.Kind, req.Target.String(), ack.OK, ack.Err, ack.Note, time.Since(start).Round(time.Microsecond))
		return ack
	})
}

func (d *Daemon) perform(req *proto.Request) *proto.Ack {
	ack := &proto.Ack{ID: req.ID}
	if req.Deadline != 0 && stream.Now() > req.Deadline {
		ack.Err = "the request's time ran out before it could run"
		return ack
	}
	left := 5 * time.Second
	if req.Deadline != 0 {
		left = time.Duration(req.Deadline-stream.Now()) * time.Millisecond
	}
	t := req.Target
	snap := d.snapshotNow()
	if t.Inst != "" && snap.Inst != "" && t.Inst != snap.Inst && req.Op != proto.OpNew {
		if req.Op == proto.OpHas || req.Op == proto.OpKill {
			ack.OK, ack.Gone, ack.Note = true, true, "already gone"
			return ack
		}
		ack.Err = fmt.Sprintf("%s restarted since it was listed: %s is gone", d.name, t.String())
		return ack
	}
	ctl := d.w.control()
	if ctl == nil && req.Op != proto.OpNew && req.Op != proto.OpHas {
		if req.Op == proto.OpKill {
			ack.OK, ack.Note = true, "already gone"
			return ack
		}
		ack.Err = "no tmux server on " + d.name
		return ack
	}
	do := func(line string) (tmux.Reply, error) {
		r, err := ctl.DoTimeout(line, max(left, 100*time.Millisecond))
		if err == nil && r.Err {
			err = errors.New(strings.TrimSpace(r.Text()))
		}
		return r, err
	}
	var err error
	switch req.Op {
	case proto.OpKill:
		line := "kill-session -t " + tmux.Quote(t.Session)
		if req.Kind == proto.KindWindow {
			line = "kill-window -t " + tmux.Quote(t.Window)
		}
		if _, err = do(line); err != nil && gone(err) {
			err, ack.Note = nil, "already gone"
		}
	case proto.OpRename:
		if req.Name == "" {
			err = errors.New("a name is needed")
			break
		}
		// "--": a name may start with "-".
		line := "rename-session -t " + tmux.Quote(t.Session) + " -- " + tmux.Arg(req.Name)
		if req.Kind == proto.KindWindow {
			line = "rename-window -t " + tmux.Quote(t.Window) + " -- " + tmux.Arg(req.Name)
		}
		_, err = do(line)
	case proto.OpNew:
		ack.Ref, err = d.newLocal(req, ctl, left)
		if err == nil && ctl == nil {
			ack.Note = "started tmux on " + d.name
		}
	case proto.OpDup:
		line := "new-session -d -P -F " + tmux.Quote("#{session_id}\t#{session_name}") + " -t " + tmux.Quote(t.Session)
		if req.Name != "" {
			line += " -s " + tmux.Arg(req.Name)
		}
		var r tmux.Reply
		if r, err = do(line); err == nil {
			ack.Ref = d.madeRef(r.Text(), false)
		}
	case proto.OpCapture:
		target := t.Pane
		if target == "" {
			target = t.Window
		}
		if target == "" {
			target = t.Session
		}
		var r tmux.Reply
		if r, err = do("capture-pane -e -p -t " + tmux.Quote(target)); err == nil {
			ack.Text = r.Text()
		}
	case proto.OpHas:
		ack.Gone = !d.hasSession(ctl, t.Session, left)
	default:
		err = fmt.Errorf("unknown request %q", req.Op)
	}
	if err != nil {
		ack.Err = err.Error()
		return ack
	}
	ack.OK = true
	if mutates(req.Op) {
		d.w.refresh()
	}
	return ack
}

// gone reports whether a tmux error says the target does not exist.
func gone(err error) bool {
	m := err.Error()
	return strings.Contains(m, "can't find") || strings.Contains(m, "no such") || strings.Contains(m, "not found")
}

// hasSession asks tmux itself, so a detach is answered with what is true
// now: no server means gone.
func (d *Daemon) hasSession(ctl *tmux.Control, id string, left time.Duration) bool {
	if ctl != nil {
		r, err := ctl.DoTimeout("has-session -t "+tmux.Quote(id), max(left, 100*time.Millisecond))
		if err == nil {
			return !r.Err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), max(left, 100*time.Millisecond))
	defer cancel()
	_, err := d.tm.Run(ctx, "-N", "has-session", "-t", id)
	return err == nil
}

// newLocal makes a session (or a window in t.Session). With no server it
// starts one, waiting for the user's config, and waits for the watch to
// take the new server before answering.
func (d *Daemon) newLocal(req *proto.Request, ctl *tmux.Control, left time.Duration) (*proto.Ref, error) {
	dir := req.Dir
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	window := req.Kind == proto.KindWindow
	if ctl != nil {
		var line string
		if window {
			line = "new-window -d -P -F " + tmux.Quote("#{window_id}\t#{window_name}") + " -t " + tmux.Quote(req.Target.Session+":")
			if req.Name != "" {
				line += " -n " + tmux.Arg(req.Name)
			}
		} else {
			line = "new-session -d -P -F " + tmux.Quote("#{session_id}\t#{session_name}")
			if req.Name != "" {
				line += " -s " + tmux.Arg(req.Name)
			}
		}
		if dir != "" {
			line += " -c " + tmux.Quote(dir)
		}
		r, err := ctl.DoTimeout(line, max(left, 100*time.Millisecond))
		if err != nil {
			return nil, err
		}
		if r.Err {
			return nil, errors.New(strings.TrimSpace(r.Text()))
		}
		return d.madeRef(r.Text(), window), nil
	}
	if window {
		return nil, errors.New("no tmux server on " + d.name)
	}
	// No server: start one with the session.
	ctx, cancel := context.WithTimeout(context.Background(), min(newServerWait, max(left, 100*time.Millisecond)))
	defer cancel()
	args := []string{"new-session", "-d", "-P", "-F", "#{session_id}\t#{session_name}"}
	if req.Name != "" {
		args = append(args, "-s", tmux.Literal(req.Name))
	}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	d.logf("act: starting tmux for a new session")
	out, err := d.tm.Run(ctx, args...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("tmux on %s did not start in time", d.name)
		}
		return nil, err
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer wcancel()
	d.w.waitAttached(wctx)
	return d.madeRef(strings.TrimSpace(out), false), nil
}

// madeRef turns "<id>\t<name>" from new-session or new-window into a ref.
func (d *Daemon) madeRef(out string, window bool) *proto.Ref {
	id, name, _ := strings.Cut(strings.TrimSpace(out), "\t")
	r := &proto.Ref{Host: d.id, Name: d.name, Inst: d.snapshotNow().Inst, Label: name}
	if window {
		r.Window = id
	} else {
		r.Session = id
	}
	return r
}

// ackTimeout is a dashboard request's deadline when the caller sets none.
func ackTimeout() time.Duration { return config.Duration("TOWER_ACK_TIMEOUT", 5*time.Second) }

// act is the act call: a request from a dashboard (or tower last) on this
// machine. It runs here, at this machine's home, or is relayed to the
// client's home.
func (d *Daemon) act(ctx context.Context, req *proto.Request) *proto.Ack {
	if req.ID == "" {
		req.ID = config.NewID() + config.NewID()
	}
	if req.Deadline == 0 {
		req.Deadline = stream.Now() + ackTimeout().Milliseconds()
	}
	if req.From == "" {
		req.From = d.id
	}
	if req.Op == proto.OpNew && req.Kind != proto.KindWindow && d.hostNoServer(req.Target.Host) {
		req.Deadline = max(req.Deadline, stream.Now()+newServerWait.Milliseconds())
	}
	if req.Op == proto.OpSwitch {
		return d.switchFrom(ctx, req)
	}
	if req.Target.Host == "" || req.Target.Host == d.id {
		return d.runLocal(req)
	}
	g := d.clientReg(req.Client)
	home := ""
	if g != nil {
		home = g.Home
	}
	if h := d.homeRole(); h != nil && (home == "" || home == d.id) {
		return h.route(ctx, req)
	}
	rec := d.homeFor(home)
	if rec == nil {
		return &proto.Ack{ID: req.ID, Err: "home not connected"}
	}
	return d.relay(ctx, rec, req)
}

// switchFrom handles a dashboard's switch: the pressing client's
// registration names the loop, the attach and the home that decides.
func (d *Daemon) switchFrom(ctx context.Context, req *proto.Request) *proto.Ack {
	g := d.clientReg(req.Client)
	if g == nil {
		return &proto.Ack{ID: req.ID, Err: "⏎ on another host needs the attach loop (run tower outside tmux)"}
	}
	req.Loop = g.Loop
	if req.Gen == 0 {
		req.Gen = g.Gen
	}
	if g.Home == d.id {
		if h := d.homeRole(); h != nil {
			return h.storeSwitch(ctx, req)
		}
	}
	rec := d.homeFor(g.Home)
	if rec == nil {
		return &proto.Ack{ID: req.ID, Err: "home not connected"}
	}
	return d.relay(ctx, rec, req)
}

// hostNoServer reports whether the host with towerd id id is known to
// have no tmux server, from what this towerd holds.
func (d *Daemon) hostNoServer(id string) bool {
	if id == "" || id == d.id {
		return d.snapshotNow().NoServer
	}
	if h := d.homeRole(); h != nil {
		if l := h.linkByID(id); l != nil {
			d.mu.Lock()
			defer d.mu.Unlock()
			return l.nosrv
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.homes {
		if r.view != nil {
			if host := r.view.HostByID(id); host != nil {
				return host.NoServer
			}
		}
	}
	return false
}
