package scenario

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/test/scenario/fakenet"
)

// lcRTTs are the connection family's round trips, one remote each.
var lcRTTs = []int{0, 50, 150, 400}

// lcWorld is the home on A and remotes R0, R50, R150 and R400 over a
// shared master each, at production timings, each with a loop attached.
type lcWorld struct {
	w     *World
	a     *Host
	hosts []*Host
	knobs func(rtt int, k *fakenet.Knobs)
}

func lcName(rtt int) string { return fmt.Sprintf("R%d", rtt) }

func newLC(t *testing.T, id string, knobs func(rtt int, k *fakenet.Knobs)) *lcWorld {
	w := NewWorld(t, id)
	w.Timing(ProductionTimings)
	x := &lcWorld{w: w, knobs: knobs}
	x.a = w.Host("A", []string{"alpha"})
	var remotes []config.Host
	for _, rtt := range lcRTTs {
		h := w.Host(lcName(rtt), []string{fmt.Sprintf("s%d", rtt)})
		x.hosts = append(x.hosts, h)
		w.Knobs(h.Name, func(k *fakenet.Knobs) { x.shape(rtt, k) })
		remotes = append(remotes, h.Remote())
	}
	w.Home(x.a, remotes...)
	for _, h := range x.hosts {
		w.WaitLink(x.a, h.Name, "up", 20*time.Second)
	}
	return x
}

// shape sets a remote's link: a shared master, delay rtt/2, then the
// scenario's own knobs.
func (x *lcWorld) shape(rtt int, k *fakenet.Knobs) {
	k.Mux, k.DelayMs = true, rtt/2
	if x.knobs != nil {
		x.knobs(rtt, k)
	}
}

// attachAll puts a loop's client on every remote.
func (x *lcWorld) attachAll() {
	for i, rtt := range lcRTTs {
		h := x.hosts[i]
		s := fmt.Sprintf("s%d", rtt)
		x.w.FakeLoop(x.a).Attach(x.w.Ref(x.a, h.Name, s), h)
		x.w.Eventually(10*time.Second, h.Name+"'s client", func() bool { return slices.Equal(h.Clients(), []string{s}) })
	}
}

// tracker records, per host, when each link event was first seen since
// its last reset: stalled, down (down or connecting, or a new attempt
// since the last poll), notup (anything but up), attempt (a new connect),
// back (up again after notup).
type tracker struct {
	x     *lcWorld
	mu    sync.Mutex
	start time.Time
	seen  map[string]time.Duration
	att   map[string]int
	stop  chan struct{}
}

func (x *lcWorld) track() *tracker {
	tr := &tracker{x: x, stop: make(chan struct{})}
	tr.reset()
	go func() {
		for {
			select {
			case <-tr.stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			st := x.a.Status()
			if st == nil {
				continue
			}
			tr.mu.Lock()
			now := time.Since(tr.start)
			for _, l := range st.Detail.Links {
				mark := func(ev string) {
					if _, ok := tr.seen[l.Name+" "+ev]; !ok {
						tr.seen[l.Name+" "+ev] = now
					}
				}
				newAttempt := l.Attempts != tr.att[l.Name]
				tr.att[l.Name] = l.Attempts
				if l.Stalled || l.Status == "stalled" {
					mark("stalled")
				}
				if l.Status == "down" || l.Status == "connecting" || newAttempt {
					mark("down")
				}
				if newAttempt {
					mark("attempt")
				}
				if l.Status != "up" || l.Stalled || newAttempt {
					mark("notup")
				} else if _, ok := tr.seen[l.Name+" notup"]; ok {
					mark("back")
				}
			}
			tr.mu.Unlock()
		}
	}()
	x.w.T.Cleanup(func() { close(tr.stop) })
	return tr
}

// reset restarts the clock; a host not up now counts as notup at 0.
func (tr *tracker) reset() {
	st := tr.x.a.Status()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.start = time.Now()
	tr.seen = map[string]time.Duration{}
	tr.att = map[string]int{}
	if st == nil {
		return
	}
	for _, l := range st.Detail.Links {
		tr.att[l.Name] = l.Attempts
		if l.Status != "up" {
			tr.seen[l.Name+" notup"] = 0
		}
	}
}

// wait waits for host's event, at most d; -1 when it never came.
func (tr *tracker) wait(host, ev string, d time.Duration) time.Duration {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		v, ok := tr.seen[host+" "+ev]
		tr.mu.Unlock()
		if ok {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	return -1
}

func (tr *tracker) get(host, ev string) (time.Duration, bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	v, ok := tr.seen[host+" "+ev]
	return v, ok
}

// LC02: a clean drop of a link up for the stable period is retried within
// 200ms, and every host is back.
func TestLC02(t *testing.T) {
	x := newLC(t, "lc02", nil)
	x.attachAll()
	tr := x.track()
	// Up for the stable period (TOWER_STABLE, 30s at production timing).
	time.Sleep(31 * time.Second)
	tr.reset()
	for _, h := range x.hosts {
		x.w.Drop(h.Name)
	}
	time.Sleep(300 * time.Millisecond)
	for _, h := range x.hosts {
		notup := tr.wait(h.Name, "notup", 10*time.Second)
		attempt := tr.wait(h.Name, "attempt", 10*time.Second)
		back := tr.wait(h.Name, "back", 30*time.Second)
		t.Logf("%s: dropped seen %v, retried %v later, back %v", h.Name, notup, attempt-notup, back)
		if notup < 0 || attempt < 0 || back < 0 {
			t.Fatalf("%s never came back", h.Name)
		}
		// 200ms, plus the tracker's 10ms polls on either side.
		if attempt-notup > 220*time.Millisecond {
			t.Fatalf("%s retried %v after the drop", h.Name, attempt-notup)
		}
	}
	// loop part: every loop's client back on its host.
}

// LC03: a link half-open after a network change: back in seconds, faster
// with the interface watcher.
func TestLC03(t *testing.T) {
	for _, mode := range []string{"silent", "netchange"} {
		t.Run(mode, func(t *testing.T) {
			x := newLC(t, "lc03-"+mode, nil)
			x.attachAll()
			time.Sleep(time.Duration(rand.IntN(1000)) * time.Millisecond)
			tr := x.track()
			at := time.Now().UnixMilli()
			for i, rtt := range lcRTTs {
				x.w.Knobs(x.hosts[i].Name, func(k *fakenet.Knobs) { x.shape(rtt, k); k.HalfOpenAt = at })
			}
			tr.reset()
			if mode == "netchange" {
				time.Sleep(time.Second)
				if err := x.a.Call(proto.CallNetChange, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			limit := 12 * time.Second
			if mode == "netchange" {
				limit = 8 * time.Second
			}
			for _, h := range x.hosts {
				back := tr.wait(h.Name, "back", 40*time.Second)
				stalled, _ := tr.get(h.Name, "stalled")
				down, _ := tr.get(h.Name, "down")
				t.Logf("%s (%s): stalled %v, given up %v, back %v", h.Name, mode, stalled, down, back)
				if back < 0 || back > limit {
					t.Fatalf("%s back after %v (limit %v)", h.Name, back, limit)
				}
			}
			// loop part: each loop's client back on its host.
		})
	}
}

// LC05: a slow but live link (200–800ms each way at R400) for 30s,
// with network changes every 3s, is never dropped.
func TestLC05(t *testing.T) {
	x := newLC(t, "lc05", func(rtt int, k *fakenet.Knobs) {
		k.DelayMs = rtt/2 + 3*rtt/4
		k.JitterMs = 3 * rtt / 4
	})
	x.attachAll()
	tr := x.track()
	attempts := map[string]int{}
	for _, h := range x.hosts {
		attempts[h.Name] = x.w.Link(x.a, h.Name).Attempts
	}
	tr.reset()
	end := time.Now().Add(30 * time.Second)
	n := 0
	for time.Now().Before(end) {
		time.Sleep(3 * time.Second)
		x.a.Call(proto.CallNetChange, nil, nil)
		n++
	}
	var bad, stalled []string
	for i, h := range x.hosts {
		if _, ok := tr.get(h.Name, "stalled"); ok {
			stalled = append(stalled, h.Name)
		}
		if _, ok := tr.get(h.Name, "down"); ok {
			bad = append(bad, h.Name+" down")
		}
		if got := x.w.Link(x.a, h.Name).Attempts; got != attempts[h.Name] {
			bad = append(bad, fmt.Sprintf("%s reconnected (%d → %d)", h.Name, attempts[h.Name], got))
		}
		if !slices.Equal(h.Clients(), []string{fmt.Sprintf("s%d", lcRTTs[i])}) {
			bad = append(bad, h.Name+" lost its client")
		}
	}
	t.Logf("%d network changes; seen stalled: %v", n, stalled)
	if len(bad) > 0 {
		t.Fatalf("a slow but live link was dropped: %v", bad)
	}
}
