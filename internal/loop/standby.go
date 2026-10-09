package loop

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/relay"
)

// Standby sessions (protocol.md, "Standby sessions").
const (
	standbyRefresh  = 2 * time.Second  // offers asked again at least this often
	standbyReady    = 20 * time.Second // not ready by then counts as dead
	standbyBackoff  = time.Second      // a host whose standby died before ready waits this, doubling
	standbyBackoffs = time.Minute
	// recycleReady is how long a session past its ended marker has for
	// its shim to say it is ready again.
	recycleReady = time.Second
)

// state is where a standby is. Each standby goes forward through them
// once: a session handed back by an attach is a new standby.
type state int

const (
	starting state = iota // its shim has not said it is ready
	draining              // an attach's session: its client is being detached back into a standby
	ready
	inUse // an attach has it, and hands it back (recycle) or lets it go (release)
)

// standby is one ssh session to a host, made ahead of a switch there, or
// an attach's session kept for the next, its client detached back into a
// standby.
type standby struct {
	host, name, key string
	sess            *relay.Session
	env             []string    // the loop's environment, tower's variables left out
	modes           relay.Modes // the terminal's modes it was made with
	// again: its client can be detached back into a standby.
	// Set before settled closes.
	again   bool
	state   state
	since   time.Time     // when it entered its state
	settled chan struct{} // closed once it is ready or given up
	takenC  chan struct{} // closed when an attach takes it
	gone    chan struct{} // closed once its watch has closed it
}

func newStandby(host, name, key string, sess *relay.Session, env []string, modes relay.Modes, st state) *standby {
	return &standby{host: host, name: name, key: key, sess: sess, env: env, modes: modes, state: st, since: time.Now(),
		settled: make(chan struct{}), takenC: make(chan struct{}), gone: make(chan struct{})}
}

func (st state) String() string {
	return [...]string{"starting", "draining", "ready", "in use"}[st]
}

// hostStats is what a host's sessions have done in the loop's life.
type hostStats struct {
	opened, reused, givenUp int
	why                     string // the last give-up's reason
}

type hostBackoff struct {
	d     time.Duration
	until time.Time
}

// standbys is the loop's set of standbys, one per host the home offers.
// One goroutine (run) keeps it; take hands one to an attach. A standby's
// watch is the only thing that closes its session, until an attach takes
// it; the attach then does. A standby the attach will hand back stays in
// the set, in use, so no other session is opened for its host meanwhile.
type standbys struct {
	l       *attachLoop
	kickC   chan struct{}
	mu      sync.Mutex
	byHost  map[string]*standby // towerd id → standby
	backoff map[string]*hostBackoff
	closed  bool
	// reserved is the host an attach is being prepared for: its standby
	// stays, though the home stops offering one there once prepared.
	reserved string
	stats    map[string]*hostStats // towerd id → its sessions so far
	// spawn starts a standby's session (relay.Start; tests replace it).
	spawn func(argv, env []string, modes relay.Modes, rows, cols int) (*relay.Session, error)
}

func newStandbys(l *attachLoop) *standbys {
	return &standbys{l: l, kickC: make(chan struct{}, 1), byHost: map[string]*standby{}, backoff: map[string]*hostBackoff{}, stats: map[string]*hostStats{}, spawn: relay.Start}
}

func (s *standbys) statsLocked(host string) *hostStats {
	st := s.stats[host]
	if st == nil {
		st = &hostStats{}
		s.stats[host] = st
	}
	return st
}

// opened counts a session the loop opened to host for an attach.
func (s *standbys) opened(host string) {
	s.mu.Lock()
	s.statsLocked(host).opened++
	s.mu.Unlock()
}

// status is the set per host, for the home's tower status.
func (s *standbys) status() []proto.StandbyStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	hosts := make([]string, 0, len(s.stats))
	for h := range s.stats {
		hosts = append(hosts, h)
	}
	for h := range s.byHost {
		if s.stats[h] == nil {
			hosts = append(hosts, h)
		}
	}
	slices.Sort(hosts)
	out := make([]proto.StandbyStatus, 0, len(hosts))
	for _, h := range hosts {
		st := proto.StandbyStatus{Host: h}
		if c := s.stats[h]; c != nil {
			st.Opened, st.Reused, st.GivenUp, st.Why = c.opened, c.reused, c.givenUp, c.why
		}
		if sb := s.byHost[h]; sb != nil {
			st.State, st.Again, st.Ms = sb.state.String(), sb.again, time.Since(sb.since).Milliseconds()
		}
		out = append(out, st)
	}
	return out
}

// kick asks for a refresh soon: a standby was used or died, or the
// loop's current host changed.
func (s *standbys) kick() {
	select {
	case s.kickC <- struct{}{}:
	default:
	}
}

// run refreshes the set on every change of the home's view, every 2s and
// when kicked, until ctx ends.
func (s *standbys) run(ctx context.Context) {
	t := time.NewTicker(standbyRefresh)
	defer t.Stop()
	for {
		_, changed := s.l.views.get()
		s.refresh(ctx)
		s.heartbeat()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kickC:
		case <-changed:
		}
	}
}

// refresh drops the standbys whose key the home no longer offers (their
// host gone, down, stalled or reconnected) and starts those missing.
func (s *standbys) refresh(ctx context.Context) {
	var offers []proto.Offer
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := s.l.call(cctx, proto.CallStandby, proto.LoopArgs{ID: s.l.id, Standbys: s.status()}, &offers)
	cancel()
	if err != nil {
		return
	}
	d, _ := s.l.views.get()
	if d == nil {
		return // the hosts' names come with the first view
	}
	s.apply(offers, &d.View)
}

// apply drops the standbys whose key is no longer offered, but for one
// in use, and starts those missing. Sessions start (a pty, ssh's fork and exec) outside the lock, so a
// hand-off taking a standby never waits on them.
func (s *standbys) apply(offers []proto.Offer, v *proto.View) {
	type start struct {
		o    proto.Offer
		name string
	}
	var starts []start
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	for host, sb := range s.byHost {
		if host == s.reserved || sb.state == inUse {
			continue
		}
		if !slices.ContainsFunc(offers, func(o proto.Offer) bool { return o.Host == host && o.Key == sb.key }) {
			s.dropLocked(sb)
		}
	}
	for _, o := range offers {
		if s.byHost[o.Host] != nil {
			continue
		}
		if b := s.backoff[o.Host]; b != nil && time.Now().Before(b.until) {
			continue
		}
		h := v.HostByID(o.Host)
		if h == nil {
			continue // not in the view yet
		}
		starts = append(starts, start{o, h.Name})
	}
	s.mu.Unlock()
	rows, cols := s.l.size()
	for _, st := range starts {
		config.Mark("standby: start " + st.name)
		env := os.Environ()
		sess, err := s.spawn(st.o.Argv, env, s.l.orig, rows, cols)
		s.mu.Lock()
		switch {
		case err != nil:
			s.backOffLocked(st.o.Host)
			s.mu.Unlock()
		case s.closed || s.byHost[st.o.Host] != nil:
			s.mu.Unlock()
			go func() {
				sess.Kill()
				sess.Close()
			}()
		default:
			sb := newStandby(st.o.Host, st.name, st.o.Key, sess, towerless(env), s.l.orig, starting)
			s.byHost[st.o.Host] = sb
			s.statsLocked(st.o.Host).opened++
			s.mu.Unlock()
			go s.watch(sb, "", 0)
		}
	}
}

// watch gets the standby ready, then waits for its end: a new one that
// dies before it is ready backs its host off; one that dies unused, or a
// recycled one not ready in time, is replaced.
//
// A recycled session (nonce set) first drains its last client's output
// up to the ended marker carrying nonce, within wait.
func (s *standbys) watch(sb *standby, nonce string, wait time.Duration) {
	err := sb.getReady(nonce, wait)
	s.mu.Lock()
	mine := s.byHost[sb.host] == sb
	switch {
	case mine && err != nil:
		delete(s.byHost, sb.host)
		if nonce == "" {
			s.backOffLocked(sb.host)
		} else {
			st := s.statsLocked(sb.host)
			st.givenUp++
			st.why = err.Error()
		}
	case mine:
		sb.state, sb.since = ready, time.Now()
		delete(s.backoff, sb.host)
		if nonce != "" {
			s.statsLocked(sb.host).reused++
		}
	}
	close(sb.settled)
	s.mu.Unlock()
	if !mine || err != nil {
		if mine {
			config.Mark("standby: given up " + sb.name + ": " + err.Error())
			s.kick()
		}
		s.closeSession(sb)
		return
	}
	config.Mark("standby: ready " + sb.name)
	select {
	case <-sb.sess.Done():
	case <-sb.takenC:
		return
	}
	s.mu.Lock()
	if sb.state == inUse {
		s.mu.Unlock()
		return
	}
	mine = s.byHost[sb.host] == sb
	if mine {
		delete(s.byHost, sb.host)
	}
	s.mu.Unlock()
	s.closeSession(sb)
	if mine {
		s.kick()
	}
}

// getReady waits for the standby's ready marker, draining a recycled
// session's last client first (nonce set).
func (sb *standby) getReady(nonce string, wait time.Duration) error {
	timeout := standbyReady
	if nonce != "" {
		timeout = recycleReady
		if err := sb.sess.Drain(nonce, wait); err != nil {
			return err
		}
	}
	before, err := sb.sess.ReadUntil([]byte(relay.MarkerReady), timeout)
	if err != nil {
		return err
	}
	sb.again = bytes.Contains(before, []byte(relay.MarkerAgain))
	return nil
}

func (s *standbys) closeSession(sb *standby) {
	sb.sess.Kill()
	sb.sess.Close()
	close(sb.gone)
}

func (s *standbys) backOffLocked(host string) {
	b := s.backoff[host]
	if b == nil {
		b = &hostBackoff{}
		s.backoff[host] = b
	}
	if b.d == 0 {
		b.d = standbyBackoff
	} else {
		b.d = min(2*b.d, standbyBackoffs)
	}
	b.until = time.Now().Add(b.d)
}

// dropLocked takes the standby out of the set and ends its session; its
// watch closes it.
func (s *standbys) dropLocked(sb *standby) {
	delete(s.byHost, sb.host)
	config.Mark("standby: drop " + sb.name)
	sb.sess.Kill()
}

// reserve keeps host's standby for the attach being prepared ("": none).
func (s *standbys) reserve(host string) {
	s.mu.Lock()
	s.reserved = host
	s.mu.Unlock()
	if host == "" {
		s.kick()
	}
}

// heartbeat sends every ready standby an empty line: a shim that stops
// hearing them exits (shim.go, standbySilence). It holds s.mu, as take
// does, so no heartbeat follows a standby's go line.
func (s *standbys) heartbeat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sb := range s.byHost {
		if sb.state == ready {
			sb.sess.Send(nil)
		}
	}
}

// take hands the host's standby to an attach: it must be ready, have the
// key the home gave the attach, and have been made for this terminal (the
// same environment and modes). A standby made for another terminal is
// dropped. A session still being recycled is waited for, up to wait.
// With reuse (the attach has a nonce), a standby whose client can be
// detached back into one stays in the set, in use, so no refresh starts
// another for its host.
func (s *standbys) take(host, key string, wait time.Duration, reuse bool) *standby {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.byHost[host]
	if sb != nil && sb.state == draining && sb.key == key && wait > 0 {
		config.Mark("standby: wait for " + sb.name)
		s.mu.Unlock()
		select {
		case <-sb.settled:
		case <-time.After(wait):
		}
		s.mu.Lock()
		sb = s.byHost[host]
	}
	why := ""
	switch {
	case sb == nil:
		return nil
	case sb.state == inUse:
		return nil
	case sb.state != ready:
		why = "not ready"
	case sb.key != key:
		why = "made for another link"
	case !slices.Equal(sb.env, towerless(os.Environ())):
		why = "another environment"
	case !sameModes(s.l.tty, sb.modes):
		why = "other terminal modes"
	}
	if why != "" {
		config.Mark("standby: not used " + sb.name + ": " + why)
		if sb.state == ready {
			s.dropLocked(sb)
		}
		return nil
	}
	sb.state, sb.since = inUse, time.Now()
	close(sb.takenC)
	if !reuse || !sb.again {
		delete(s.byHost, host)
	}
	return sb
}

// release lets an attach's standby go: the attach ends its session, and
// its host gets a new standby.
func (s *standbys) release(sb *standby) {
	s.mu.Lock()
	if s.byHost[sb.host] == sb {
		delete(s.byHost, sb.host)
	}
	s.mu.Unlock()
	s.kick()
}

// recycle takes the running attach's session back, its relay released
// and its client about to be detached back into a standby, as a new
// standby for its host. A session that cannot be kept (the set is
// closing, or the attach's standby is no longer its host's) is ended.
func (s *standbys) recycle(sb *standby, nonce string, wait time.Duration) {
	s.mu.Lock()
	if s.closed || s.byHost[sb.host] != sb {
		s.mu.Unlock()
		config.Mark("standby: not kept " + sb.name)
		go func() {
			sb.sess.Kill()
			sb.sess.Close()
		}()
		s.kick()
		return
	}
	nb := newStandby(sb.host, sb.name, sb.key, sb.sess, sb.env, sb.modes, draining)
	s.byHost[sb.host] = nb
	s.mu.Unlock()
	config.Mark("standby: recycle " + sb.name)
	go s.watch(nb, nonce, wait)
}

// closeAll ends every standby: the loop is exiting. Their watches close
// them; it waits for that, a little. One in use is its attach's to end.
func (s *standbys) closeAll() {
	s.mu.Lock()
	s.closed = true
	all := make([]*standby, 0, len(s.byHost))
	for _, sb := range s.byHost {
		if sb.state != inUse {
			all = append(all, sb)
		}
	}
	s.byHost = map[string]*standby{}
	s.mu.Unlock()
	for _, sb := range all {
		sb.sess.Kill()
	}
	deadline := time.After(2 * time.Second)
	for _, sb := range all {
		select {
		case <-sb.gone:
		case <-deadline:
			return
		}
	}
}

// towerless is an environment with tower's own variables left out: they
// say how tower runs, not what the terminal is.
func towerless(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "TOWER_") {
			out = append(out, kv)
		}
	}
	slices.Sort(out)
	return out
}
