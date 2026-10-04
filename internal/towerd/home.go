package towerd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/install"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/stream"
	"github.com/jerrykal/tower/internal/transport"
)

// Timings of the home role.
const (
	loopTTL        = 15 * time.Second // a loop with no beat for this long is gone
	prepareWait    = 8 * time.Second  // prepare waits this long for a host to come up
	heldWait       = 100 * time.Millisecond
	standbyUpFor   = time.Second // a link must be up this long before standbys are offered
	hostsSaveEvery = 5 * time.Second
)

// handoffTTL is how long a stored switch stays valid (TOWER_HANDOFF_TTL).
func handoffTTL() time.Duration { return config.Duration("TOWER_HANDOFF_TTL", 30*time.Second) }

// homeRole links to every host in hosts.toml, merges their states with
// the local one into the view, and owns its loops' current, previous and
// pending switch. Its fields are guarded by Daemon.mu.
type homeRole struct {
	d    *Daemon
	tr   Transport
	inst *install.Installer // this build, for hosts that lack it

	links    map[string]*link // by host name
	order    []string
	loops    map[string]*loopRec
	clientsB map[string][]proto.Client // host id → its last client list
	viewSeq  uint64
	netAt    time.Time
	netGen   uint64
	hostsMod time.Time
	cache    map[string]cachedHost
	last     proto.Ref
	savedAt  time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

// loopRec is one attach loop of this home.
type loopRec struct {
	id        string
	gen       int
	cur, prev proto.Ref
	sw        *pendingSwitch
	waiters   []chan struct{} // wait-switch calls of the current attach
	seen      bool            // the home has seen this attach's client
	picking   bool            // at the picker: attached nowhere
	beat      time.Time
}

// pendingSwitch is a stored switch: committed once stored, taken once by
// the loop's after.
type pendingSwitch struct {
	target  proto.Ref
	nonce   string
	gen     int
	at      time.Time
	held    chan struct{} // closed when the loop holds the frame and will end the client
	waiting bool          // the switch still waits for held
	ended   bool          // the loop ends the old client
	aborted string        // why it can no longer go ahead (its host stalled)
}

// cachedHost is what hosts.json keeps of a host across restarts.
type cachedHost struct {
	ID       string          `json:"id"`
	OS       string          `json:"os,omitempty"`
	Tmux     string          `json:"tmux,omitempty"`
	Version  string          `json:"version,omitempty"`
	MKey     string          `json:"mkey,omitempty"`
	Inst     string          `json:"inst,omitempty"`
	Sessions []proto.Session `json:"sessions,omitempty"`
	Heard    int64           `json:"heard,omitempty"` // unix ms
}

type lastFile struct {
	Target proto.Ref `json:"target"`
}

func newHome(d *Daemon) *homeRole {
	tr := d.o.Transport
	if tr == nil {
		tr = newSSHTransport(d.env.CMDir())
	}
	h := &homeRole{d: d, tr: tr, inst: install.New(d.version, d.self), links: map[string]*link{}, loops: map[string]*loopRec{}, clientsB: map[string][]proto.Client{}, cache: map[string]cachedHost{}}
	h.ctx, h.cancel = context.WithCancel(d.ctx)
	config.ReadJSON(d.env.State("hosts.json"), &h.cache)
	var lf lastFile
	config.ReadJSON(d.env.State("last.json"), &lf)
	h.last = lf.Target
	return h
}

func (h *homeRole) start() {
	h.reload()
	go transport.WatchNet(h.ctx, time.Second, func() { h.netChanged("network interfaces changed") })
	go h.watchWake()
}

func (h *homeRole) stop() {
	h.cancel()
	h.d.mu.Lock()
	var ls []*link
	for _, l := range h.links {
		ls = append(ls, l)
	}
	h.d.mu.Unlock()
	for _, l := range ls {
		l.stop()
	}
	h.save(true)
}

// reload reads hosts.toml and makes the links follow it: new hosts get a
// link, removed ones lose theirs, turned off ones are closed.
func (h *homeRole) reload() {
	path := h.d.env.HostsFile()
	var mod time.Time
	if st, err := os.Stat(path); err == nil {
		mod = st.ModTime()
	}
	hosts, err := config.LoadHosts(path)
	if err != nil {
		// Noted as read, so a file that does not parse is read again only
		// once it changes, not on every dashboard's view.
		h.d.mu.Lock()
		h.hostsMod = mod
		h.d.mu.Unlock()
		h.d.logf("home: %v", err)
		return
	}
	var start, stop []*link
	h.d.mu.Lock()
	h.hostsMod = mod
	seen := map[string]bool{}
	h.order = h.order[:0]
	for _, cfg := range hosts {
		seen[cfg.Name] = true
		h.order = append(h.order, cfg.Name)
		l := h.links[cfg.Name]
		if l != nil && !l.cfg.Same(cfg) {
			stop = append(stop, l)
			l = nil
		}
		if l == nil {
			l = newLink(h, cfg)
			if c, ok := h.cache[cfg.Name]; ok {
				l.fromCache(c)
			}
			h.links[cfg.Name] = l
			if cfg.On() {
				start = append(start, l)
			}
		}
	}
	for name, l := range h.links {
		if !seen[name] {
			stop = append(stop, l)
			delete(h.links, name)
		}
	}
	h.d.bump()
	h.d.mu.Unlock()
	for _, l := range stop {
		l.stop()
	}
	for _, l := range start {
		l.start()
	}
	h.kickViews()
}

// maybeReload reloads hosts.toml when it changed since it was read (a
// dashboard opening).
func (h *homeRole) maybeReload() {
	st, err := os.Stat(h.d.env.HostsFile())
	h.d.mu.Lock()
	changed := err == nil && !st.ModTime().Equal(h.hostsMod) || err != nil && !h.hostsMod.IsZero()
	h.d.mu.Unlock()
	if changed {
		h.reload()
	}
}

// linkByID is the link whose host has towerd id id.
func (h *homeRole) linkByID(id string) *link {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	return h.linkByIDLocked(id)
}

func (h *homeRole) linkByIDLocked(id string) *link {
	if id == "" {
		return nil
	}
	var best *link
	for _, name := range h.order {
		l := h.links[name]
		if l == nil || l.id != id {
			continue
		}
		if best == nil || l.status == proto.StatusUp && best.status != proto.StatusUp {
			best = l
		}
	}
	return best
}

// kickViews asks every link to push the view.
func (h *homeRole) kickViews() {
	h.d.mu.Lock()
	ls := make([]*link, 0, len(h.links))
	for _, l := range h.links {
		ls = append(ls, l)
	}
	h.d.mu.Unlock()
	for _, l := range ls {
		l.pace.Kick()
	}
}

// buildView is the merged view: this machine, every host in the list,
// and the loops. Call with mu held.
func (h *homeRole) buildView(now time.Time) proto.View {
	v := proto.View{Home: h.d.id}
	v.Hosts = append(v.Hosts, h.d.localHost(now))
	for _, name := range h.order {
		if l := h.links[name]; l != nil {
			v.Hosts = append(v.Hosts, l.host(now))
		}
	}
	ids := make([]string, 0, len(h.loops))
	for id := range h.loops {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		l := h.loops[id]
		v.Loops = append(v.Loops, proto.Loop{ID: l.id, Gen: l.gen, Cur: l.cur, Prev: l.prev, Seen: l.seen})
	}
	return v
}

// localChanged follows a new local snapshot: the loops' clients here, and
// every remote's view.
func (h *homeRole) localChanged() {
	h.d.mu.Lock()
	s := h.d.snap
	if s != nil {
		h.applyClients(h.d.id, h.d.name, s.Inst, sessionsAt(s, time.Now()), h.d.regs.forHome(h.d.id))
	}
	h.d.mu.Unlock()
	h.kickViews()
}

// applyClients follows the clients of this home's loops on one host: a
// loop's current target moves with its client (a switch-client there, a
// window change), and the home notes when it first sees an attach's
// client. Call with mu held.
func (h *homeRole) applyClients(hostID, hostName, inst string, sessions []proto.Session, clients []proto.Client) {
	h.clientsB[hostID] = clients
	changed := false
	for _, c := range clients {
		if c.Home != h.d.id {
			continue
		}
		l := h.loops[c.Loop]
		if l == nil || c.Gen != l.gen || l.cur.Host != hostID {
			continue
		}
		label := c.Session
		if i := slices.IndexFunc(sessions, func(s proto.Session) bool { return s.ID == c.Session }); i >= 0 {
			label = sessions[i].Name
		}
		// moved is this client's own news: only a loop whose client moved
		// (or was first seen) makes its target the last one.
		moved := false
		if !l.seen {
			l.seen = true
			moved = true
			config.Mark("home sees the new client")
		}
		switch {
		case c.Session != l.cur.Session:
			l.prev = l.cur
			l.cur = proto.Ref{Host: hostID, Name: hostName, Inst: inst, Session: c.Session, Window: c.Window, Label: label}
			moved = true
		case c.Window != l.cur.Window || label != l.cur.Label:
			l.cur.Window, l.cur.Label = c.Window, label
			moved = true
		}
		if moved {
			h.last = l.cur
			changed = true
		}
	}
	if changed {
		h.d.bump()
		go h.save(false)
	}
}

// replayClients applies every host's last client list again: a loop the
// home relearns from a beat (after a restart) catches up with moves its
// client made meanwhile. Call with mu held.
func (h *homeRole) replayClients() {
	for id, cs := range h.clientsB {
		name, inst := h.d.name, ""
		var sessions []proto.Session
		if id == h.d.id {
			if s := h.d.snap; s != nil {
				inst, sessions = s.Inst, s.Sessions
			}
		} else if l := h.linkByIDLocked(id); l != nil {
			name, inst, sessions = l.cfg.Name, l.inst, l.sessions
		}
		h.applyClients(id, name, inst, sessions, cs)
	}
}

// save writes hosts.json and last.json, at most every few seconds unless
// now.
func (h *homeRole) save(now bool) {
	h.d.mu.Lock()
	if !now && time.Since(h.savedAt) < hostsSaveEvery {
		h.d.mu.Unlock()
		return
	}
	h.savedAt = time.Now()
	for name, l := range h.links {
		if l.id != "" {
			h.cache[name] = l.toCache()
		}
	}
	cache := make(map[string]cachedHost, len(h.cache))
	for k, v := range h.cache {
		cache[k] = v
	}
	last := h.last
	h.d.mu.Unlock()
	config.WriteJSON(h.d.env.State("hosts.json"), cache)
	config.WriteJSON(h.d.env.State("last.json"), lastFile{Target: last})
}

// beat is a loop's heartbeat (and its first call). A loop the home does
// not know (a new one, or after a restart) is learned from the beat.
func (h *homeRole) beat(a proto.LoopBeat) *proto.LoopAck {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	ack := &proto.LoopAck{}
	l := h.loops[a.ID]
	if l == nil {
		l = &loopRec{id: a.ID, gen: a.Gen, cur: a.Cur, prev: a.Prev}
		h.loops[a.ID] = l
		if a.Cur.IsZero() {
			ack.Last = h.last
		}
		h.replayClients()
		h.d.bump()
		h.d.logf("home: loop %s (gen %d, at %s)", a.ID, a.Gen, a.Cur.String())
		go h.kickViews()
	}
	l.beat = time.Now()
	return ack
}

func (h *homeRole) bye(id string) {
	h.d.mu.Lock()
	l := h.loops[id]
	if l != nil {
		delete(h.loops, id)
		for _, w := range l.waiters {
			close(w)
		}
		h.d.bump()
	}
	h.d.mu.Unlock()
	if l != nil {
		h.d.logf("home: loop %s said bye", id)
		h.kickViews()
	}
}

func (h *homeRole) expireLoops() {
	h.d.mu.Lock()
	var gone []string
	for id, l := range h.loops {
		if time.Since(l.beat) > loopTTL {
			gone = append(gone, id)
		}
	}
	h.d.mu.Unlock()
	for _, id := range gone {
		h.bye(id)
	}
}

// hostState is a target host as prepare and switch need it.
type hostState struct {
	name, status, reason, inst, mkey string
	local, nosrv                     bool
	sessions                         []proto.Session
	link                             *link
}

// hostOf finds the host with towerd id id. Call with mu held.
func (h *homeRole) hostOf(id string) *hostState {
	if id == h.d.id {
		hs := &hostState{name: h.d.name, status: proto.StatusLocal, local: true, mkey: h.d.env.MKey}
		if s := h.d.snap; s != nil {
			hs.inst, hs.nosrv, hs.sessions = s.Inst, s.NoServer, s.Sessions
		}
		return hs
	}
	l := h.linkByIDLocked(id)
	if l == nil {
		return nil
	}
	return &hostState{name: l.cfg.Name, status: l.status, reason: l.reason, inst: l.inst, mkey: l.mkey, nosrv: l.nosrv, sessions: l.sessions, link: l}
}

// unreachable says why a host cannot take a switch or a request now, or
// "" when it can.
func (hs *hostState) unreachable() string {
	switch hs.status {
	case proto.StatusLocal, proto.StatusUp:
		return ""
	case proto.StatusStalled:
		return hs.name + " is not responding"
	case proto.StatusOff:
		return hs.name + " is turned off"
	}
	if hs.reason != "" {
		return fmt.Sprintf("%s is %s: %s", hs.name, hs.status, hs.reason)
	}
	return fmt.Sprintf("%s is %s", hs.name, hs.status)
}

// waitHost waits for the host to be up (or local), at most prepareWait;
// a stalled, failed, duplicate or turned off host is refused at once.
func (h *homeRole) waitHost(ctx context.Context, id string) (*hostState, error) {
	deadline := time.Now().Add(prepareWait)
	for {
		h.d.mu.Lock()
		hs := h.hostOf(id)
		ch := h.d.changed
		h.d.mu.Unlock()
		if hs == nil {
			return nil, fmt.Errorf("unknown host %s", id)
		}
		why := hs.unreachable()
		if why == "" {
			return hs, nil
		}
		switch hs.status {
		case proto.StatusStalled, proto.StatusFailed, proto.StatusDup, proto.StatusOff:
			return nil, fmt.Errorf("%s", why)
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, fmt.Errorf("%s", why)
		}
		select {
		case <-ch:
		case <-time.After(min(left, 100*time.Millisecond)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// prepare makes target the loop's next attach and returns how to run it.
func (h *homeRole) prepare(ctx context.Context, a proto.PrepareArgs) (*proto.Prepared, error) {
	config.Mark("prepare")
	t := a.Target
	hs, err := h.waitHost(ctx, t.Host)
	if err != nil {
		return nil, err
	}
	if t.Inst != "" && hs.inst != "" && t.Inst != hs.inst {
		return nil, fmt.Errorf("%s restarted since it was listed: %s is gone", hs.name, t.String())
	}
	si := slices.IndexFunc(hs.sessions, func(s proto.Session) bool { return s.ID == t.Session })
	if si < 0 {
		return nil, fmt.Errorf("session %s no longer exists", t.String())
	}
	s := hs.sessions[si]
	if t.Window != "" && !slices.ContainsFunc(s.Windows, func(w proto.Window) bool { return w.ID == t.Window }) {
		return nil, fmt.Errorf("window %s no longer exists", t.String())
	}
	t.Name, t.Inst, t.Label = hs.name, hs.inst, s.Name

	h.d.mu.Lock()
	l := h.loops[a.Loop]
	if l == nil {
		l = &loopRec{id: a.Loop, beat: time.Now()}
		h.loops[a.Loop] = l
	}
	l.gen++
	if !l.cur.IsZero() && !l.cur.SameSession(t) {
		l.prev = l.cur
	}
	l.cur, l.sw, l.seen, l.picking = t, nil, false, false
	for _, w := range l.waiters {
		close(w)
	}
	l.waiters = nil
	h.last = t
	gen := l.gen
	p := &proto.Prepared{Gen: gen, Target: t, Local: hs.local}
	shimArgs := []string{"attach", "--loop", l.id, "--gen", strconv.Itoa(gen), "--home", h.d.id, "--inst", hs.inst, "--mkey", hs.mkey}
	if hs.local {
		p.Argv = append(append([]string{h.d.self}, shimArgs...), "--tmux", strings.Join(h.d.env.Tmux, " "), t.Session)
		if t.Window != "" {
			p.Argv = append(p.Argv, t.Window)
		}
	} else {
		lk := hs.link
		args := append(shimArgs, "--tmux", lk.cfg.Tmux, t.Session)
		if t.Window != "" {
			args = append(args, t.Window)
		}
		p.Argv = h.tr.AttachArgv(lk.cfg, h.towerCommand(lk.cfg, args...))
		goLine, _ := json.Marshal(proto.GoLine{Loop: l.id, Gen: gen, Home: h.d.id, Inst: hs.inst, MKey: hs.mkey, Session: t.Session, Window: t.Window})
		p.Go = string(goLine)
		p.Key = lk.standbyKey()
		p.Link = lk.gen
		if lk.conn != nil {
			p.RTT = lk.conn.SlowRTT().Milliseconds()
		}
	}
	h.d.bump()
	h.d.mu.Unlock()
	go h.save(false)
	h.kickViews()
	config.Mark("prepared " + hs.name)
	h.d.logf("home: loop %s gen %d → %s", a.Loop, gen, t.String())
	return p, nil
}

// storeSwitch is a dashboard's switch, here or relayed: refused for an
// earlier attach or an unreachable host before anything detaches, else
// stored (committed), the loop woken, and the answer says whether the
// loop ends the old client itself.
func (h *homeRole) storeSwitch(ctx context.Context, req *proto.Request) *proto.Ack {
	ack := &proto.Ack{ID: req.ID}
	h.d.mu.Lock()
	l := h.loops[req.Loop]
	if l == nil {
		h.d.mu.Unlock()
		ack.Err = "no attach loop owns this terminal any more"
		return ack
	}
	if req.Gen != l.gen {
		h.d.mu.Unlock()
		ack.Err = fmt.Sprintf("request from an earlier attach (gen %d, now %d)", req.Gen, l.gen)
		return ack
	}
	hs := h.hostOf(req.Target.Host)
	if hs == nil {
		h.d.mu.Unlock()
		ack.Err = "unknown host " + req.Target.Host
		return ack
	}
	if why := hs.unreachable(); why != "" {
		h.d.mu.Unlock()
		ack.Err = why
		return ack
	}
	t := req.Target
	t.Name = hs.name
	sw := &pendingSwitch{target: t, nonce: req.Nonce, gen: l.gen, at: time.Now(), held: make(chan struct{})}
	l.sw = sw
	eager := config.Flag("TOWER_EAGER", true) && len(l.waiters) > 0
	if eager {
		sw.waiting = true
		for _, w := range l.waiters {
			close(w)
		}
		l.waiters = nil
	}
	h.d.bump()
	h.d.mu.Unlock()
	config.Mark("switch stored")
	h.d.logf("home: switch stored for loop %s gen %d → %s (eager %v)", req.Loop, req.Gen, t.String(), eager)
	ack.OK = true
	if !eager {
		return ack
	}
	// Committed: the wait for the loop's hold goes on even if the
	// dashboard goes away.
	t1 := time.NewTimer(heldWait)
	defer t1.Stop()
	select {
	case <-sw.held:
	case <-t1.C:
	}
	h.d.mu.Lock()
	sw.waiting = false
	ack.Ended = sw.ended
	h.d.mu.Unlock()
	return ack
}

// plant stores a switch as if a dashboard had asked (a test hook).
func (h *homeRole) plant(r *proto.Request) (any, error) {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	l := h.loops[r.Loop]
	if l == nil {
		return nil, fmt.Errorf("no loop %s", r.Loop)
	}
	l.sw = &pendingSwitch{target: r.Target, nonce: r.Nonce, gen: r.Gen, at: time.Now(), held: make(chan struct{})}
	h.d.bump()
	return struct{}{}, nil
}

// waitSwitch blocks while the loop's attach a.Gen runs, until a switch is
// stored for it.
func (h *homeRole) waitSwitch(ctx context.Context, a proto.GenArgs) *proto.SwitchWake {
	h.d.mu.Lock()
	l := h.loops[a.Loop]
	if l == nil || l.gen != a.Gen {
		h.d.mu.Unlock()
		return &proto.SwitchWake{}
	}
	if l.sw != nil && l.sw.gen == a.Gen && l.sw.aborted == "" {
		h.d.mu.Unlock()
		return &proto.SwitchWake{Switch: true}
	}
	ch := make(chan struct{})
	l.waiters = append(l.waiters, ch)
	h.d.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
		h.d.mu.Lock()
		if l := h.loops[a.Loop]; l != nil {
			l.waiters = slices.DeleteFunc(l.waiters, func(c chan struct{}) bool { return c == ch })
		}
		h.d.mu.Unlock()
		return &proto.SwitchWake{}
	}
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	l = h.loops[a.Loop]
	return &proto.SwitchWake{Switch: l != nil && l.sw != nil && l.sw.gen == a.Gen && l.gen == a.Gen}
}

// held is the loop's confirmation that it holds the frame: while the
// switch still waits for it, the loop is told to end the old client.
func (h *homeRole) held(a proto.GenArgs) *proto.HeldReply {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	l := h.loops[a.Loop]
	if l == nil || l.sw == nil || l.sw.gen != a.Gen || !l.sw.waiting || l.sw.ended {
		return &proto.HeldReply{}
	}
	l.sw.ended = true
	close(l.sw.held)
	return &proto.HeldReply{End: true}
}

// after decides what a loop does once an attach ended: the table in
// protocol.md, "After an attach ends".
func (h *homeRole) after(ctx context.Context, a proto.AfterArgs) *proto.Next {
	h.d.mu.Lock()
	l := h.loops[a.Loop]
	if l == nil {
		h.d.mu.Unlock()
		return &proto.Next{Do: proto.NextPicker, Note: "the home does not know this loop"}
	}
	sw := l.sw
	l.sw = nil // read once
	gen, cur := l.gen, l.cur
	h.d.bump()
	h.d.mu.Unlock()
	next := h.decide(ctx, a, gen, cur, sw)
	if next.Do == proto.NextPicker {
		h.d.mu.Lock()
		if l := h.loops[a.Loop]; l != nil && l.gen == gen {
			l.picking = true
		}
		h.d.mu.Unlock()
	}
	config.Mark("after " + next.Do)
	h.d.logf("home: loop %s gen %d exited %d (ended %v): %s %s %s", a.Loop, a.Gen, a.Code, a.Ended, next.Do, next.Target.String(), next.Note)
	return next
}

func (h *homeRole) decide(ctx context.Context, a proto.AfterArgs, gen int, cur proto.Ref, sw *pendingSwitch) *proto.Next {
	if a.Ended || a.Code == 42 {
		if sw != nil && sw.aborted != "" {
			return &proto.Next{Do: proto.NextHandoff, Target: cur, Note: sw.aborted}
		}
		reason := "no request"
		switch {
		case sw == nil:
		case sw.gen != a.Gen || sw.gen != gen:
			reason = fmt.Sprintf("request from an earlier attach (gen %d, now %d)", sw.gen, gen)
		case time.Since(sw.at) > handoffTTL():
			reason = fmt.Sprintf("request is %ds old", int(time.Since(sw.at).Seconds()))
		default:
			return &proto.Next{Do: proto.NextHandoff, Target: sw.target}
		}
		return &proto.Next{Do: proto.NextPicker, Note: "exit 42 without a valid hand-off (" + reason + "): ignored"}
	}
	if a.Code == 43 {
		return &proto.Next{Do: proto.NextPicker, Note: fmt.Sprintf("%s restarted since it was listed; pick again", cur.Name)}
	}
	if a.Code == 255 && cur.Host != h.d.id {
		return &proto.Next{Do: proto.NextReconnect, Target: cur}
	}
	discarded := ""
	if sw != nil {
		discarded = "a hand-off request was pending and has been discarded"
	}
	gone, err := h.sessionGone(ctx, cur)
	if err != nil || !gone {
		return &proto.Next{Do: proto.NextExit, Note: discarded}
	}
	if next, ok := h.moveOn(a.Loop, cur); ok {
		return &proto.Next{Do: proto.NextHandoff, Target: next, Note: fmt.Sprintf("tower: %s ended; now on %s", sessionOf(cur), sessionOf(next))}
	}
	return &proto.Next{Do: proto.NextExit, Note: fmt.Sprintf("tower: %s ended", sessionOf(cur))}
}

// sessionGone asks the session's host directly whether it still exists:
// a detach returns the terminal in one round trip.
func (h *homeRole) sessionGone(ctx context.Context, r proto.Ref) (bool, error) {
	req := &proto.Request{ID: config.NewID() + config.NewID(), Op: proto.OpHas, Target: r, Deadline: stream.Now() + ackTimeout().Milliseconds(), From: h.d.id}
	var ack *proto.Ack
	if r.Host == h.d.id {
		ack = h.d.runLocal(req)
	} else {
		ack = h.route(ctx, req)
	}
	if !ack.OK {
		return false, fmt.Errorf("%s", ack.Err)
	}
	return ack.Gone, nil
}

// moveOn picks where a loop goes when its session ended: its previous
// session if nobody is on it, else the most recently used session nobody
// is on, on any reachable host.
func (h *homeRole) moveOn(loop string, ended proto.Ref) (proto.Ref, bool) {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	v := h.buildView(time.Now())
	free := func(r proto.Ref) bool {
		host := v.HostByID(r.Host)
		if host == nil || !host.Reachable() || host.Inst != r.Inst && r.Inst != "" {
			return false
		}
		i := slices.IndexFunc(host.Sessions, func(s proto.Session) bool { return s.ID == r.Session })
		return i >= 0 && host.Sessions[i].Attached == 0
	}
	if l := h.loops[loop]; l != nil && !l.prev.IsZero() && !l.prev.SameSession(ended) && free(l.prev) {
		p := l.prev
		p.Window = ""
		return p, true
	}
	var best proto.Ref
	bestAgo := int64(-1)
	for _, host := range v.Hosts {
		if !host.Reachable() {
			continue
		}
		for _, s := range host.Sessions {
			if s.Attached > 0 || host.ID == ended.Host && s.ID == ended.Session {
				continue
			}
			if bestAgo < 0 || s.Ago < bestAgo {
				bestAgo = s.Ago
				best = proto.Ref{Host: host.ID, Name: host.Name, Inst: host.Inst, Session: s.ID, Label: s.Name}
			}
		}
	}
	return best, bestAgo >= 0
}

// standby offers a loop one standby session per host it may switch to.
func (h *homeRole) standby(loop string) []proto.Offer {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	cur := ""
	if l := h.loops[loop]; l != nil && !l.picking {
		cur = l.cur.Host
	}
	out := []proto.Offer{}
	for _, name := range h.order {
		l := h.links[name]
		if l == nil || l.status != proto.StatusUp || l.id == "" || l.id == cur || !l.cfg.StandbyOn() || time.Since(l.upAt) < standbyUpFor {
			continue
		}
		args := []string{"attach", "--standby", "--mkey", l.mkey, "--tmux", l.cfg.Tmux}
		o := proto.Offer{Host: l.id, Key: l.standbyKey(), Argv: h.tr.AttachArgv(l.cfg, h.towerCommand(l.cfg, args...))}
		if l.conn != nil {
			o.RTT = l.conn.SlowRTT().Milliseconds()
		}
		out = append(out, o)
	}
	return out
}

// route runs a request for another host over that host's link. A host
// that is not up, or stalled, is refused at once.
func (h *homeRole) route(ctx context.Context, req *proto.Request) *proto.Ack {
	if req.Target.Host == h.d.id || req.Target.Host == "" {
		return h.d.runLocal(req)
	}
	h.d.mu.Lock()
	hs := h.hostOf(req.Target.Host)
	var lk *link
	var conn *stream.Conn
	if hs != nil {
		lk = hs.link
		conn = lk.conn
	}
	h.d.mu.Unlock()
	if hs == nil {
		return &proto.Ack{ID: req.ID, Err: "unknown host " + req.Target.Host}
	}
	if why := hs.unreachable(); why != "" || conn == nil {
		if why == "" {
			why = hs.name + " is not connected"
		}
		return &proto.Ack{ID: req.ID, Err: why}
	}
	if req.Deadline != 0 && req.Deadline-stream.Now() <= conn.Margin().Milliseconds() {
		// The host would get it with its time already up.
		return &proto.Ack{ID: req.ID, Err: "too little time left to reach " + hs.name}
	}
	ack, err := conn.Request(ctx, proto.TExec, req)
	if err != nil {
		switch streamErr(err) {
		case "no answer in time":
			return &proto.Ack{ID: req.ID, Err: hs.name + " did not answer in time"}
		case "not responding":
			return &proto.Ack{ID: req.ID, Err: hs.name + " is not responding"}
		}
		return &proto.Ack{ID: req.ID, Err: hs.name + ": " + streamErr(err)}
	}
	return ack
}

// serveRelay answers a request a remote's dashboard sent: the asking
// host gets the merged view first, then the answer, so the rows read
// after the answer show its result.
func (h *homeRole) serveRelay(l *link, req *proto.Request) {
	ctx, cancel := context.WithTimeout(h.ctx, max(time.Duration(req.Deadline-stream.Now())*time.Millisecond, 10*time.Millisecond))
	defer cancel()
	var ack *proto.Ack
	switch {
	case req.Op == proto.OpSwitch:
		ack = h.storeSwitch(ctx, req)
	default:
		ack = h.route(ctx, req)
	}
	h.d.mu.Lock()
	conn := l.conn
	var m *proto.Msg
	if conn != nil {
		m = l.viewMsgLocked(true)
	}
	h.d.mu.Unlock()
	if conn == nil {
		return
	}
	if m != nil {
		conn.SendNow(m)
	}
	conn.Answer(ack)
}

// netChanged: a stalled host not heard since is given up at once, and so
// is one that stalls later without being heard; a down host retries now.
func (h *homeRole) netChanged(why string) {
	h.d.mu.Lock()
	h.netAt = time.Now()
	h.netGen++
	var ls []*link
	for _, l := range h.links {
		ls = append(ls, l)
	}
	h.d.mu.Unlock()
	h.d.logf("home: %s", why)
	for _, l := range ls {
		l.netChanged()
	}
}

// watchWake notices a wall clock that jumped past the monotonic one: the
// machine slept.
func (h *homeRole) watchWake() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-h.ctx.Done():
			return
		case now := <-t.C:
			wall := now.Round(0).Sub(last.Round(0))
			mono := now.Sub(last)
			last = now
			if wall-mono > 2*time.Second {
				h.wake(fmt.Sprintf("woke after %v", (wall - mono).Round(time.Second)))
			}
		}
	}
}

// wake closes every stream and makes every master exit, in parallel, so
// one wedged master holds up no other host.
func (h *homeRole) wake(why string) {
	h.d.logf("home: wake (%s): resetting every link", why)
	h.d.mu.Lock()
	var ls []*link
	for _, l := range h.links {
		ls = append(ls, l)
	}
	h.d.mu.Unlock()
	for _, l := range ls {
		go l.reset("wake")
	}
}

// detail fills the full status with the links, loops and pending
// switches.
func (h *homeRole) detail(det *proto.Detail) {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	for _, name := range h.order {
		if l := h.links[name]; l != nil {
			det.Links = append(det.Links, l.status_())
		}
	}
	ids := make([]string, 0, len(h.loops))
	for id := range h.loops {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		l := h.loops[id]
		det.Loops = append(det.Loops, proto.LoopStatus{ID: l.id, Gen: l.gen, Cur: l.cur, Prev: l.prev, Host: l.cur.Host, Seen: l.seen})
		if l.sw != nil {
			det.Pending = append(det.Pending, proto.PendingSwitch{Loop: l.id, Gen: l.sw.gen, Target: l.sw.target, Nonce: l.sw.nonce, Age: time.Since(l.sw.at).Milliseconds()})
		}
	}
}

// abortSwitches gives up the stored switches to a host that stalled:
// their loops go back where they were. Call with mu held.
func (h *homeRole) abortSwitches(hostID, why string) {
	for _, l := range h.loops {
		if l.sw != nil && l.sw.target.Host == hostID && l.sw.aborted == "" {
			l.sw.aborted = why
		}
	}
}

// towerCommand is the shell line running tower with args on host c: the
// binary c pins (tower = …), else this build's under the install root,
// by its exact path.
func (h *homeRole) towerCommand(c config.Host, args ...string) string {
	if c.Tower != "" {
		return transport.RemoteCommand(c.Tower, args...)
	}
	return h.inst.Command(args...)
}

// sessionOf names r's session (host:session), without its window: what a
// note about a session ending says.
func sessionOf(r proto.Ref) string {
	r.Window = ""
	return r.String()
}
