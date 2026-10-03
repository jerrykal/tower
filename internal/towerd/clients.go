package towerd

import (
	"slices"
	"strconv"
	"strings"
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
}

func (r *reg) bound() bool { return r.Name != "" }

// unboundTTL is how long a registration may wait for its client: the shim
// execs tmux at once, so one never seen is an attach that failed.
const unboundTTL = 10 * time.Second

// registry is the set of registrations, saved to clients.json on every
// change and restored at start, so a restarted towerd keeps them (V03).
// Its methods are called with Daemon.mu held, except save.
type registry struct {
	path string
	list []*reg

	saveMu sync.Mutex
}

func loadRegistry(path string) *registry {
	r := &registry{path: path}
	config.ReadJSON(path, &r.list)
	return r
}

// save writes the set; call it without Daemon.mu, after a change.
func (r *registry) save() {
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	r.write()
}

func (r *registry) write() {
	list := slices.Clone(r.list)
	if list == nil {
		list = []*reg{}
	}
	config.WriteJSON(r.path, list)
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
// changed.
func (r *registry) bind(s *snapshot) bool {
	changed := false
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
	return changed
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
	pid, created, name, ok := parseClient(v)
	if !ok {
		return nil
	}
	for _, g := range r.list {
		if g.Pid == pid && (!g.bound() || g.Created == created && g.Name == name) {
			return g
		}
	}
	return nil
}

// parseClient splits a TOWER_CLIENT value. The name, a tty path or
// client-<pid>, comes last and may hold colons.
func parseClient(v string) (pid int, created, name string, ok bool) {
	f := strings.SplitN(v, ":", 3)
	if len(f) != 3 {
		return 0, "", "", false
	}
	pid, err := strconv.Atoi(f[0])
	if err != nil || pid <= 0 {
		return 0, "", "", false
	}
	return pid, f[1], f[2], true
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
	return proto.Client{Name: g.Name, Pid: g.Pid, Session: g.Session, Window: g.Window, Loop: g.Loop, Gen: g.Gen, Home: g.Home}
}

// register stores the shim's registration and schedules re-reads that
// cover a missed client notification.
func (d *Daemon) register(a proto.RegisterArgs) (*proto.Registered, error) {
	if a.Pid <= 0 {
		return nil, errBad("register: no pid")
	}
	d.mu.Lock()
	d.regs.add(&reg{Pid: a.Pid, Loop: a.Loop, Gen: a.Gen, Home: a.Home, Inst: a.Inst, At: time.Now().UnixMilli()})
	d.bump()
	d.mu.Unlock()
	d.regs.save()
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
