package scenario

import "time"

// Links and faults: what the network and the far side do to an ssh
// name. Scenarios set them through the World; each host's backend puts
// them in place: a container's (container_test.go), or a host's behind
// sshds of its own (sshd_test.go).

// Link is the shape of the link to an ssh name.
type Link struct {
	DelayMs  int // one-way delay, each direction
	JitterMs int // ± on the delay
	BwKBps   int // KB/s per direction; 0 unlimited
}

// linkState is an ssh name's link and faults.
type linkState struct {
	Link
	target     *Host
	freeze     bool   // half-open: nothing moves, ssh gives up after its alive window
	stall      bool   // the far side stops reading and writing; ssh never gives up
	down       string // refused, hostkey, auth, password, resolve, timeout
	drops      int    // each one closes the live connections
	halfOpenAt int64  // unix ms: connections made before it are half-open from then on
	port       int    // a container's sshd port for this name
	sshd       *sshd  // the name's sshd (the sshd backend)
}

// SSH registers ssh name alias for target, an SSHHost, with an unshaped
// link. A name registered again replaces what it had: its sshd, its port.
func (w *World) SSH(alias string, target *Host) {
	w.T.Helper()
	if target.ctr == nil && !target.overSSH {
		w.T.Fatalf("ssh name %s: %s is no SSHHost", alias, target.Name)
	}
	if w.links == nil {
		w.links = map[string]*linkState{}
	}
	if old := w.links[alias]; old != nil && old.sshd != nil {
		old.sshd.stop()
	}
	w.links[alias] = &linkState{target: target}
	if target.ctr != nil {
		w.ctrName(alias, target)
	} else {
		w.sshdName(alias, target)
	}
	w.apply(alias, linkState{target: target})
}

// Shape changes alias's link.
func (w *World) Shape(alias string, f func(*Link)) {
	w.T.Helper()
	w.change(alias, func(s *linkState) { f(&s.Link) })
}

// Freeze makes alias's link half-open (nothing moves; ssh gives up after
// its alive window, the far side never hears of it), or ends that.
func (w *World) Freeze(alias string, on bool) {
	w.T.Helper()
	w.change(alias, func(s *linkState) { s.freeze = on })
}

// Stall stops the far side of alias's connections reading and writing
// (ssh still answers, so it never gives up), or ends that.
func (w *World) Stall(alias string, on bool) {
	w.T.Helper()
	w.change(alias, func(s *linkState) { s.stall = on })
}

// NetworkChange makes every connection to alias made before at
// half-open from at on (the laptop moved networks): masters made since
// work.
func (w *World) NetworkChange(alias string, at time.Time) {
	w.T.Helper()
	w.change(alias, func(s *linkState) { s.halfOpenAt = at.UnixMilli() })
}

// Down makes new connections to alias fail as ssh does: refused,
// hostkey, auth, password, resolve or timeout. Live ones stay (Drop ends
// them).
func (w *World) Down(alias, how string) {
	w.T.Helper()
	w.change(alias, func(s *linkState) { s.down = how })
}

// Drop closes alias's live connections.
func (w *World) Drop(alias string) {
	w.T.Helper()
	w.change(alias, func(s *linkState) { s.drops++ })
}

// Heal ends alias's faults, keeping its link. Connections a network
// change left half-open stay so.
func (w *World) Heal(alias string) {
	w.T.Helper()
	w.change(alias, func(s *linkState) {
		s.freeze, s.stall, s.down = false, false, ""
	})
}

// Reset heals alias and unshapes its link.
func (w *World) Reset(alias string) {
	w.T.Helper()
	w.change(alias, func(s *linkState) {
		s.Link = Link{}
		s.freeze, s.stall, s.down = false, false, ""
	})
}

func (w *World) change(alias string, f func(*linkState)) {
	w.T.Helper()
	s, ok := w.links[alias]
	if !ok {
		w.T.Fatalf("no ssh name %s", alias)
	}
	prev := *s
	f(s)
	w.apply(alias, prev)
}

// apply puts alias's state in place, prev being what is in place now.
func (w *World) apply(alias string, prev linkState) {
	w.T.Helper()
	s := w.links[alias]
	if s.target.ctr != nil {
		w.ctrApply(alias, prev, s)
		return
	}
	w.sshdApply(alias, prev, s)
}
