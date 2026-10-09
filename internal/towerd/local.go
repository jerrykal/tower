package towerd

import (
	"errors"
	"slices"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

func errBad(msg string) error { return errors.New(msg) }

// publish takes a snapshot from the watch: it binds registrations, wakes
// dashboards, and passes the change to each connected home and to the
// home role. changed is false for a re-read that found nothing new.
func (d *Daemon) publish(s *snapshot, changed bool) {
	if changed {
		// The refresher only records them here; it looks at a new one
		// in its own goroutine. Before the pushes below, so the states
		// they build leave out the dirs that now have a session.
		paths := make([]string, 0, len(s.Sessions))
		for _, x := range s.Sessions {
			paths = append(paths, x.Path)
		}
		d.dirs.Sessions(paths)
	}
	d.mu.Lock()
	d.snap = s
	regsChanged, ends := d.regs.bind(s)
	if !d.looked {
		d.looked = true
		close(d.lookedC)
	}
	if changed || regsChanged {
		d.bump()
	}
	var recs []*homeRec
	for _, r := range d.homes {
		if r.conn != nil {
			recs = append(recs, r)
		}
	}
	h := d.home
	var regs regCopy
	if regsChanged {
		regs = d.regs.copyLocked()
	}
	d.mu.Unlock()
	if regsChanged {
		d.regs.save(regs)
	}
	for _, g := range ends {
		go d.detachEnded(g)
	}
	if !changed && !regsChanged {
		return
	}
	for _, r := range recs {
		r.pace.Kick()
	}
	if h != nil {
		h.localChanged()
	}
}

// snapshotNow is the latest snapshot (never nil).
func (d *Daemon) snapshotNow() *snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.snap == nil {
		return &snapshot{At: time.Now()}
	}
	return d.snap
}

// firstLook is closed once the watch has read tmux (or found no server).
func (d *Daemon) firstLook() <-chan struct{} { return d.lookedC }

// sessionsAt are s's sessions with ages moved to at.
// ageRef is the moment comparison keys evaluate ages at. Evaluated at the
// epoch, an age is minus the absolute time it counts from, which stays the
// same from one read to the next while nothing changes; evaluated at
// time.Now it grows by the milliseconds between reads, so every key would
// differ and every unchanged re-read, state and view would go out again.
var ageRef = time.UnixMilli(0)

func sessionsAt(s *snapshot, at time.Time) []proto.Session {
	out := slices.Clone(s.Sessions)
	shift := at.Sub(s.At).Milliseconds()
	for i := range out {
		out[i].Ago += shift
	}
	return out
}

// localHost is this machine as a view lists it. Call with mu held.
func (d *Daemon) localHost(now time.Time) proto.Host {
	s := d.snap
	if s == nil {
		s = &snapshot{At: now}
	}
	return proto.Host{
		ID: d.id, Name: d.label, Status: proto.StatusLocal, OS: d.osName, Tmux: d.tmuxVer,
		Version: d.version, MKey: d.env.MKey, Inst: s.Inst, NoServer: s.NoServer,
		Sessions: d.withGit(sessionsAt(s, now)), Dirs: d.dirs.Dirs(),
	}
}

// stateFor is the state message for one home: the sessions, and the
// clients of that home's loops. Call with mu held.
func (d *Daemon) stateFor(home string) *proto.State { return d.stateAt(home, time.Now()) }

// stateAt is stateFor with ages as of at (ageRef for a comparison key).
func (d *Daemon) stateAt(home string, at time.Time) *proto.State {
	s := d.snap
	if s == nil {
		s = &snapshot{At: at}
	}
	st := &proto.State{Inst: s.Inst, NoServer: s.NoServer, Sessions: d.withGit(sessionsAt(s, at)), Dirs: d.dirs.Dirs()}
	for _, c := range d.regs.forHome(home) {
		st.Clients = append(st.Clients, c)
	}
	st.Pad = testPad()
	return st
}
