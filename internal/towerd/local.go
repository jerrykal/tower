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
	d.mu.Lock()
	d.snap = s
	regsChanged := d.regs.bind(s)
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
		ID: d.id, Name: d.name, Status: proto.StatusLocal, OS: d.osName, Tmux: d.tmuxVer,
		Version: d.version, MKey: d.env.MKey, Inst: s.Inst, NoServer: s.NoServer,
		Sessions: sessionsAt(s, now),
	}
}

// stateFor is the state message for one home: the sessions, and the
// clients of that home's loops. Call with mu held.
func (d *Daemon) stateFor(home string) *proto.State {
	s := d.snap
	if s == nil {
		s = &snapshot{At: time.Now()}
	}
	st := &proto.State{Inst: s.Inst, NoServer: s.NoServer, Sessions: sessionsAt(s, time.Now())}
	for _, c := range d.regs.forHome(home) {
		st.Clients = append(st.Clients, c)
	}
	st.Pad = testPad()
	return st
}
