package towerd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/stream"
)

// keptView is how long a remote keeps a disconnected home's last view,
// for the dashboards of that home's loops.
const keptView = time.Hour

// homeRec is one home connected to this towerd (the remote role), keyed
// by home id and the name that home uses for this host.
type homeRec struct {
	key, id, as, name, version string

	conn      *stream.Conn // nil once disconnected
	connected time.Time
	gone      time.Time

	view    *proto.View // the last view the home sent
	viewAt  time.Time
	viewSeq uint64

	pace      *pacer // state pushes, 100ms in a burst
	lastState string
	stateSeq  uint64
}

// silences are how long each side lets a stream be silent before ending
// it: the home gives up after TOWER_SILENCE (15s); the remote waits twice
// that and 5s more, since only the home can reconnect.
func homeSilence() time.Duration { return config.Duration("TOWER_SILENCE", 15*time.Second) }
func remoteSilence() time.Duration {
	return 2*homeSilence() + 5*time.Second
}

// serveStream runs one home's stream on a local connection the bridge
// opened: the home's hello, ours after the first look, then states out
// and views and execs in.
func (d *Daemon) serveStream(c net.Conn, br *bufio.Reader) {
	helloC := make(chan *proto.Hello, 1)
	var rec *homeRec
	recReady := make(chan struct{})
	var conn *stream.Conn
	conn = stream.New(br, c, stream.Options{
		Silence: remoteSilence(),
		Closer:  func() { c.Close() },
		Log:     func(f string, a ...any) { d.logf("home stream: "+f, a...) },
		OnMsg: func(m *proto.Msg) {
			switch m.T {
			case proto.THello:
				if m.Hello != nil {
					select {
					case helloC <- m.Hello:
					default:
					}
				}
			case proto.TView:
				<-recReady
				if rec != nil && m.View != nil {
					d.takeView(rec, conn, m.View)
				}
			case proto.TExec:
				<-recReady
				if rec != nil && m.Req != nil {
					go d.execFor(rec, conn, m.Req)
				}
			case proto.TLook:
				<-recReady
				if rec != nil {
					d.look(rec)
				}
			}
		},
	})
	conn.Start()
	ready := false
	defer func() {
		if !ready {
			close(recReady)
		}
	}()
	var hello *proto.Hello
	select {
	case hello = <-helloC:
	case <-conn.Done():
		return
	case <-time.After(15 * time.Second):
		conn.Close(errors.New("no hello from the home"))
		return
	}
	lo, hi := protoRange()
	reply := &proto.Hello{Min: lo, Max: hi, ID: d.id, Name: d.name, Version: d.version, MKey: d.env.MKey, OS: d.osName}
	p, err := proto.Negotiate(hello.Min, hello.Max, lo, hi)
	if err != nil {
		reply.Err = fmt.Sprintf("%v (towerd %s here, home %s)", err, d.version, hello.Version)
		d.logf("home %s (%s) refused: %s", hello.ID, hello.Name, reply.Err)
		conn.Send(&proto.Msg{T: proto.THello, Hello: reply})
		conn.Drain(2 * time.Second)
		conn.Close(errors.New("incompatible protocol"))
		return
	}
	reply.Proto = p
	// Hello after the first look, so the host never shows up empty.
	if wait := time.Until(d.started.Add(2 * time.Second)); wait > 0 {
		select {
		case <-d.firstLook():
		case <-time.After(wait):
		}
	}
	d.mu.Lock()
	reply.Tmux = d.tmuxVer
	key := hello.ID + "|" + hello.As
	rec = d.homes[key]
	var old *stream.Conn
	if rec == nil {
		rec = &homeRec{key: key, id: hello.ID, as: hello.As}
		r := rec
		rec.pace = newPacer(100*time.Millisecond, 2, func() bool { return d.pushState(r) })
		d.homes[key] = rec
	}
	old = rec.conn
	rec.name, rec.version = hello.Name, hello.Version
	rec.conn, rec.connected = conn, time.Now()
	rec.viewSeq, rec.lastState = 0, ""
	d.bump()
	d.mu.Unlock()
	ready = true
	close(recReady)
	if old != nil {
		// One stream per home and name: the new one replaces a half-open one.
		old.Close(errors.New("replaced by a new stream from the same home"))
	}
	d.logf("home %s (%s, tower %s) connected as %q, protocol %d", hello.ID, hello.Name, hello.Version, hello.As, p)
	conn.Send(&proto.Msg{T: proto.THello, Hello: reply})
	conn.Live()
	rec.pace.Kick()
	if testFuture() {
		conn.Send(&proto.Msg{T: "future-thing"})
	}
	<-conn.Done()
	d.mu.Lock()
	if rec.conn == conn {
		rec.conn = nil
		rec.gone = time.Now()
		d.bump()
	}
	d.mu.Unlock()
	d.logf("home %s (%s) disconnected: %v", hello.ID, hello.Name, conn.Err())
}

// pushState sends rec's home our state if it changed since the last one,
// and reports whether it sent.
func (d *Daemon) pushState(rec *homeRec) bool {
	d.mu.Lock()
	conn := rec.conn
	if conn == nil {
		d.mu.Unlock()
		return false
	}
	key, _ := json.Marshal(d.stateAt(rec.id, ageRef))
	if string(key) == rec.lastState {
		d.mu.Unlock()
		return false
	}
	rec.lastState = string(key)
	st := d.stateFor(rec.id)
	rec.stateSeq++
	st.Seq = rec.stateSeq
	d.mu.Unlock()
	conn.Send(&proto.Msg{T: proto.TState, State: st})
	return true
}

// takeView keeps the newest view a home sent.
func (d *Daemon) takeView(rec *homeRec, conn *stream.Conn, v *proto.View) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rec.conn != conn || v.Seq != 0 && v.Seq < rec.viewSeq {
		return
	}
	v.Pad = ""
	rec.view, rec.viewAt, rec.viewSeq = v, time.Now(), v.Seq
	d.bump()
}

// execFor runs a home's request here; a change goes out as a state ahead
// of the answer, so the home's next view shows it.
func (d *Daemon) execFor(rec *homeRec, conn *stream.Conn, req *proto.Request) {
	ack := d.runLocal(req)
	if mutates(req.Op) && ack.OK {
		d.mu.Lock()
		key, _ := json.Marshal(d.stateAt(rec.id, ageRef))
		rec.lastState = string(key)
		st := d.stateFor(rec.id)
		rec.stateSeq++
		st.Seq = rec.stateSeq
		d.mu.Unlock()
		conn.SendNow(&proto.Msg{T: proto.TState, State: st})
	}
	conn.Answer(ack)
}

// relay sends a request to a home over its stream and waits for the
// answer.
func (d *Daemon) relay(ctx context.Context, rec *homeRec, req *proto.Request) *proto.Ack {
	d.mu.Lock()
	conn := rec.conn
	d.mu.Unlock()
	if conn == nil {
		return &proto.Ack{ID: req.ID, Err: "home not connected"}
	}
	ack, err := conn.Request(ctx, proto.TRelay, req)
	if err != nil {
		return &proto.Ack{ID: req.ID, Err: "home did not answer: " + streamErr(err)}
	}
	return ack
}

func streamErr(err error) string {
	switch {
	case errors.Is(err, stream.ErrDeadline):
		return "no answer in time"
	case errors.Is(err, stream.ErrStalled):
		return "not responding"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "connection lost"
}

// homeFor is the connected home with id, or with no id the one connected
// last.
func (d *Daemon) homeFor(id string) *homeRec {
	d.mu.Lock()
	defer d.mu.Unlock()
	var best *homeRec
	for _, r := range d.homes {
		if r.conn == nil || id != "" && r.id != id {
			continue
		}
		if best == nil || r.connected.After(best.connected) {
			best = r
		}
	}
	return best
}

// keptHome is the record of home id, connected or not, preferring a
// connected one. Call with mu held.
func (d *Daemon) keptHome(id string) *homeRec {
	var best *homeRec
	for _, r := range d.homes {
		if r.id != id {
			continue
		}
		if best == nil || r.conn != nil && best.conn == nil || (r.conn != nil) == (best.conn != nil) && r.viewAt.After(best.viewAt) {
			best = r
		}
	}
	return best
}

// expireHomes forgets homes gone for longer than keptView.
func (d *Daemon) expireHomes() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, r := range d.homes {
		if r.conn == nil && time.Since(r.gone) > keptView {
			r.pace.Stop()
			delete(d.homes, k)
		}
	}
}

// view is what a dashboard on this machine shows: the view of the home
// whose loop owns its client, or for a client no loop owns, this
// machine's home (or the home that connected last).
func (d *Daemon) view(a proto.ViewArgs) *proto.Dash {
	if a.Look {
		d.look(nil)
	}
	g := d.clientReg(a.Client)
	h := d.homeRole()
	if h != nil {
		h.maybeReload()
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	dash := &proto.Dash{Gen: d.gen, Self: d.id}
	var held *homeRec
	switch {
	case a.Loop != "" && d.home != nil:
		dash.View = d.home.buildView(now)
		dash.Loop, dash.Home, dash.Owned = a.Loop, d.id, true
	case g != nil:
		dash.Loop, dash.Home, dash.Owned = g.Loop, g.Home, true
		if g.Home == d.id && d.home != nil {
			dash.View = d.home.buildView(now)
			break
		}
		held = d.keptHome(g.Home)
		if held == nil || held.view == nil {
			dash.View = proto.View{Home: g.Home}
			dash.Note = "home not connected"
		} else if held.conn == nil {
			dash.Note = fmt.Sprintf("home %s not connected: other hosts as of %s ago", homeLabel(held), time.Since(held.viewAt).Round(time.Second))
		}
	default:
		if d.home != nil {
			dash.View = d.home.buildView(now)
			dash.Home = d.id
		} else {
			for _, r := range d.homes {
				if r.conn != nil && r.view != nil && (held == nil || r.connected.After(held.connected)) {
					held = r
				}
			}
		}
		if a.Client != "" {
			dash.Note = "not a tower terminal: ⏎ to another host needs the attach loop"
		}
	}
	if held != nil && held.view != nil {
		dash.View = copyView(held.view, now.Sub(held.viewAt).Milliseconds())
		if dash.Home == "" {
			dash.Home = held.id
		}
	}
	// This machine's own entry is always the live one.
	self := d.localHost(now)
	if i := slices.IndexFunc(dash.View.Hosts, func(x proto.Host) bool { return x.ID == d.id }); i >= 0 {
		self.Name = dash.View.Hosts[i].Name
		dash.View.Hosts[i] = self
	} else {
		dash.View.Hosts = append([]proto.Host{self}, dash.View.Hosts...)
	}
	dash.View.Pad = ""
	return dash
}

func homeLabel(r *homeRec) string {
	if r.name != "" {
		return r.name
	}
	return r.id
}

// copyView copies v with every session's age moved on by shift ms.
func copyView(v *proto.View, shift int64) proto.View {
	out := *v
	out.Hosts = make([]proto.Host, len(v.Hosts))
	for i, h := range v.Hosts {
		h.Sessions = slices.Clone(h.Sessions)
		for j := range h.Sessions {
			h.Sessions[j].Ago += shift
		}
		if h.Seen > 0 {
			h.Seen += shift
		}
		out.Hosts[i] = h
	}
	out.Loops = slices.Clone(v.Loops)
	return out
}

// last is tower last for a client: the previous target of the client's
// loop, as a local switch-client when it is on the client's own server,
// else as a stored switch. Neither: the caller falls back to tmux's own
// switch-client -l.
func (d *Daemon) last(ctx context.Context, client string) *proto.LastResult {
	g := d.clientReg(client)
	if g == nil {
		return &proto.LastResult{Note: "not a tower terminal"}
	}
	d.mu.Lock()
	var lp *proto.Loop
	if g.Home == d.id && d.home != nil {
		if l := d.home.loops[g.Loop]; l != nil {
			lp = &proto.Loop{ID: l.id, Gen: l.gen, Cur: l.cur, Prev: l.prev}
		}
	} else if r := d.keptHome(g.Home); r != nil && r.view != nil {
		lp = r.view.LoopByID(g.Loop)
	}
	inst := ""
	if d.snap != nil {
		inst = d.snap.Inst
	}
	d.mu.Unlock()
	if lp == nil || lp.Prev.IsZero() {
		return &proto.LastResult{Note: "no previous session"}
	}
	prev := lp.Prev
	if prev.Host == d.id && (prev.Inst == "" || prev.Inst == inst) {
		return &proto.LastResult{Local: true, Target: prev}
	}
	ack := d.act(ctx, &proto.Request{Op: proto.OpSwitch, Client: client, Target: prev, Nonce: config.NewID()})
	if !ack.OK {
		return &proto.LastResult{Note: ack.Err, Target: prev}
	}
	return &proto.LastResult{Stored: true, Target: prev}
}
