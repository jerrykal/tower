package towerd

import (
	"slices"
	"sync"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// reg is one registration: an attach of some home's loop, made by the
// attach shim before it execs the tmux client, then bound to that client.
type reg struct {
	Pid     int    `json:"pid"`
	Loop    string `json:"loop"`
	Gen     int    `json:"gen"`
	Home    string `json:"home"`
	Inst    string `json:"inst,omitempty"`
	Name    string `json:"name,omitempty"`    // the tmux client, once bound
	Created string `json:"created,omitempty"` // its client_created
	Session string `json:"s,omitempty"`
	Window  string `json:"w,omitempty"`
	At      int64  `json:"at"` // unix ms registered
	// Again is the attach's nonce when its client can be detached back
	// into a standby, by running Resume in its place (detach-client -E).
	Again  string `json:"again,omitempty"`
	Resume string `json:"resume,omitempty"`
	End    bool   `json:"end,omitempty"` // its home ended the attach: detach it once bound
}

func (r *reg) bound() bool { return r.Name != "" }

// unboundTTL is how long a registration may wait for its client: the shim
// execs tmux at once, so one never seen is an attach that failed.
const unboundTTL = 10 * time.Second

// registry is the set of registrations, saved to clients.json on every
// change and restored at start, so a restarted towerd keeps them (V03).
// Its methods are called with Daemon.mu held, except save, which writes a
// copy taken under it (copyLocked): bind edits entries and filters the
// list in place, so encoding the live list without the lock could write
// torn or nil entries.
type registry struct {
	path string
	list []*reg
	seq  uint64 // bumped by copyLocked: the order the copies were taken

	saveMu sync.Mutex
	saved  uint64 // seq of the newest copy written

	// ended are attaches a home asked to end before they registered
	// (nonce → unix ms asked): their client is detached once bound.
	ended map[string]int64
}

// endLater remembers an attach, by its nonce, to end once it registers.
func (r *registry) endLater(key string) {
	now := time.Now().UnixMilli()
	if r.ended == nil {
		r.ended = map[string]int64{}
	}
	for k, at := range r.ended {
		if now-at > unboundTTL.Milliseconds() {
			delete(r.ended, k)
		}
	}
	r.ended[key] = now
}

// takeEnd reports whether the attach was asked to end before it
// registered, and forgets it.
func (r *registry) takeEnd(key string) bool {
	at, ok := r.ended[key]
	delete(r.ended, key)
	return ok && time.Now().UnixMilli()-at <= unboundTTL.Milliseconds()
}

// regCopy is the set as it was at one moment, for save.
type regCopy struct {
	seq  uint64
	list []reg
}

func loadRegistry(path string) *registry {
	r := &registry{path: path}
	config.ReadJSON(path, &r.list)
	// A file from an older build, or one cut short, may hold null
	// entries; they mean nothing.
	r.list = slices.DeleteFunc(r.list, func(g *reg) bool { return g == nil || g.Pid <= 0 })
	return r
}

// copyLocked copies the set; call it with Daemon.mu held.
func (r *registry) copyLocked() regCopy {
	r.seq++
	c := regCopy{seq: r.seq, list: make([]reg, len(r.list))}
	for i, g := range r.list {
		c.list[i] = *g
	}
	return c
}

// save writes a copy taken with copyLocked; call it without Daemon.mu. A
// copy older than one already written is skipped, so saves that race
// never put an older set over a newer one.
func (r *registry) save(c regCopy) {
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	if c.seq <= r.saved {
		return
	}
	r.saved = c.seq
	config.WriteJSON(r.path, c.list)
}

func (r *registry) add(n *reg) {
	// A pid has one registration: a re-registration replaces it.
	r.list = slices.DeleteFunc(r.list, func(o *reg) bool { return o.Pid == n.Pid })
	r.list = append(r.list, n)
}

// bind matches registrations to the snapshot's clients: an unbound one to
// the client with its pid, a bound one followed to where its client is
// now. Registrations whose client is gone, whose server restarted, or
// that never found their client are dropped. It reports whether anything
// changed, and the registrations just bound whose home has ended their
// attach (End): their clients are to be detached.
func (r *registry) bind(s *snapshot) (changed bool, end []reg) {
	now := time.Now().UnixMilli()
	keep := r.list[:0]
	for _, g := range r.list {
		drop := false
		switch {
		case s.NoServer:
			drop = true
		case g.Inst != "" && s.Inst != "" && g.Inst != s.Inst:
			drop = true
		case g.bound():
			c := s.client(func(c *tclient) bool { return c.Name == g.Name && c.Created == g.Created })
			if c == nil {
				drop = true
			} else if c.Session != g.Session || c.Window != g.Window {
				g.Session, g.Window = c.Session, c.Window
				changed = true
			}
		default:
			if c := s.client(func(c *tclient) bool { return c.Pid == g.Pid }); c != nil {
				g.Name, g.Created, g.Session, g.Window = c.Name, c.Created, c.Session, c.Window
				changed = true
				if g.End {
					end = append(end, *g)
				}
			} else if now-g.At > unboundTTL.Milliseconds() {
				drop = true
			}
		}
		if drop {
			changed = true
			continue
		}
		keep = append(keep, g)
	}
	clear(r.list[len(keep):])
	r.list = keep
	return changed, end
}

func (s *snapshot) client(match func(*tclient) bool) *tclient {
	for i := range s.Clients {
		if match(&s.Clients[i]) {
			return &s.Clients[i]
		}
	}
	return nil
}

// byClient finds the registration of a TOWER_CLIENT value,
// "pid:created:name".
func (r *registry) byClient(v string) *reg {
	c, err := proto.ParseClient(v)
	if err != nil {
		return nil
	}
	for _, g := range r.list {
		if g.Pid == c.Pid && (!g.bound() || g.Created == c.Created && g.Name == c.Name) {
			return g
		}
	}
	return nil
}

// count is the number of registrations with a live client (or one on
// its way).
func (r *registry) count() int { return len(r.list) }

// forHome are the bound clients of home's loops, as a state carries them.
func (r *registry) forHome(home string) []proto.Client {
	var out []proto.Client
	for _, g := range r.list {
		if g.Home == home && g.bound() {
			out = append(out, g.client())
		}
	}
	return out
}

func (r *registry) clients() []proto.Client {
	out := make([]proto.Client, 0, len(r.list))
	for _, g := range r.list {
		out = append(out, g.client())
	}
	return out
}

func (g *reg) client() proto.Client {
	return proto.Client{Name: g.Name, Pid: g.Pid, Session: g.Session, Window: g.Window, Loop: g.Loop, Gen: g.Gen, Home: g.Home, Reuse: g.Resume != "", End: g.End}
}

// register stores the shim's registration and schedules re-reads that
// cover a missed client notification.
func (d *Daemon) register(a proto.RegisterArgs) (*proto.Registered, error) {
	if a.Pid <= 0 {
		return nil, errBad("register: no pid")
	}
	g := &reg{Pid: a.Pid, Loop: a.Loop, Gen: a.Gen, Home: a.Home, Inst: a.Inst, At: time.Now().UnixMilli(), Again: a.Again, Resume: a.Resume}
	d.mu.Lock()
	if a.Again != "" && d.regs.takeEnd(a.Again) {
		// Its home ended the attach before it registered: the client is
		// detached once bound.
		g.End = true
		d.logf("client pid %d (loop %s gen %d) ended before it registered: detached once bound", a.Pid, a.Loop, a.Gen)
	}
	d.regs.add(g)
	d.bump()
	regs := d.regs.copyLocked()
	d.mu.Unlock()
	d.regs.save(regs)
	for _, after := range []time.Duration{150 * time.Millisecond, 600 * time.Millisecond, 1500 * time.Millisecond} {
		time.AfterFunc(after, d.w.kick)
	}
	return &proto.Registered{MKey: d.env.MKey, TmuxBin: d.tm.Bin}, nil
}

// clientReg finds the registration of a TOWER_CLIENT value, re-reading
// once when the client has registered but is not bound yet.
func (d *Daemon) clientReg(v string) *reg {
	if v == "" {
		return nil
	}
	d.mu.Lock()
	g := d.regs.byClient(v)
	var cp reg
	if g != nil {
		cp = *g
	}
	d.mu.Unlock()
	if g == nil {
		return nil
	}
	if !cp.bound() {
		d.w.refresh()
		d.mu.Lock()
		g = d.regs.byClient(v)
		if g != nil {
			cp = *g
		}
		d.mu.Unlock()
		if g == nil {
			return nil
		}
	}
	return &cp
}
