package loop

import (
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
)

// standby is one ssh session to a host, made ahead of a switch there.
type standby struct {
	host, name, key string
	sess            *relay.Session
	env             []string    // the loop's environment, tower's variables left out
	modes           relay.Modes // the terminal's modes it was made with
	ready           bool
}

type hostBackoff struct {
	d     time.Duration
	until time.Time
}

// standbys is the loop's set of standbys, one per host the home offers.
// One goroutine (run) keeps it; take hands one to an attach.
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
}

func newStandbys(l *attachLoop) *standbys {
	return &standbys{l: l, kickC: make(chan struct{}, 1), byHost: map[string]*standby{}, backoff: map[string]*hostBackoff{}}
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
// host gone, down, stalled, reconnected or now the current one) and
// starts those missing.
func (s *standbys) refresh(ctx context.Context) {
	var offers []proto.Offer
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := s.l.call(cctx, proto.CallStandby, proto.LoopArgs{ID: s.l.id}, &offers)
	cancel()
	if err != nil {
		return
	}
	d, _ := s.l.views.get()
	if d == nil {
		return // the hosts' names come with the first view
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for host, sb := range s.byHost {
		if host == s.reserved {
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
		h := d.View.HostByID(o.Host)
		if h == nil {
			continue // not in the view yet
		}
		s.startLocked(o, h.Name)
	}
}

func (s *standbys) startLocked(o proto.Offer, name string) {
	rows, cols := s.l.size()
	config.Mark("standby: start " + name)
	env := os.Environ()
	sess, err := relay.Start(o.Argv, env, s.l.orig, rows, cols)
	if err != nil {
		s.backOffLocked(o.Host)
		return
	}
	sb := &standby{host: o.Host, name: name, key: o.Key, sess: sess, env: towerless(env), modes: s.l.orig}
	s.byHost[o.Host] = sb
	go s.watch(sb)
}

// watch waits for the standby's ready marker, then for its end: one that
// dies before it is ready backs its host off; one that dies unused is
// replaced.
func (s *standbys) watch(sb *standby) {
	_, err := sb.sess.ReadUntil([]byte(relay.MarkerReady), standbyReady)
	s.mu.Lock()
	if s.byHost[sb.host] != sb {
		s.mu.Unlock()
		return
	}
	if err != nil {
		delete(s.byHost, sb.host)
		s.backOffLocked(sb.host)
		s.mu.Unlock()
		sb.sess.Kill()
		sb.sess.Close()
		s.kick()
		return
	}
	sb.ready = true
	delete(s.backoff, sb.host)
	s.mu.Unlock()
	config.Mark("standby: ready " + sb.name)
	<-sb.sess.Done()
	s.mu.Lock()
	mine := s.byHost[sb.host] == sb
	if mine {
		delete(s.byHost, sb.host)
	}
	s.mu.Unlock()
	if mine {
		sb.sess.Close()
		s.kick()
	}
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

func (s *standbys) dropLocked(sb *standby) {
	delete(s.byHost, sb.host)
	config.Mark("standby: drop " + sb.name)
	go func() {
		sb.sess.Kill()
		sb.sess.Close()
	}()
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

// take hands the host's standby to an attach: it must be ready, have the
// key the home gave the attach, and have been made for this terminal (the
// same environment and modes). A standby made for another terminal is
// dropped.
func (s *standbys) take(host, key string) *relay.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.byHost[host]
	why := ""
	switch {
	case sb == nil:
		return nil
	case !sb.ready:
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
		if sb.ready {
			s.dropLocked(sb)
		}
		return nil
	}
	delete(s.byHost, host)
	return sb.sess
}

// closeAll ends every standby: the loop is exiting.
func (s *standbys) closeAll() {
	s.mu.Lock()
	s.closed = true
	all := make([]*standby, 0, len(s.byHost))
	for _, sb := range s.byHost {
		all = append(all, sb)
	}
	s.byHost = map[string]*standby{}
	s.mu.Unlock()
	for _, sb := range all {
		sb.sess.Kill()
	}
	for _, sb := range all {
		sb.sess.Close()
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
