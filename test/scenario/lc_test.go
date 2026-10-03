package scenario

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
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
	terms []*Term
}

func lcName(rtt int) string { return fmt.Sprintf("R%d", rtt) }

// lcSession is the session on the remote of round trip rtt: names no
// picker query for one matches another's.
func lcSession(rtt int) string {
	return map[int]string{0: "kilo", 50: "lima", 150: "mike", 400: "oscar"}[rtt]
}

// newLC makes the world and starts the home; entry, when set, edits each
// remote's hosts.toml entry first.
func newLC(t *testing.T, id string, knobs func(rtt int, k *fakenet.Knobs)) *lcWorld {
	x := makeLC(t, id, knobs)
	x.start(nil)
	return x
}

func makeLC(t *testing.T, id string, knobs func(rtt int, k *fakenet.Knobs)) *lcWorld {
	w := NewWorld(t, id)
	w.Timing(ProductionTimings)
	x := &lcWorld{w: w, knobs: knobs}
	x.a = w.Host("A", []string{"alpha"})
	for _, rtt := range lcRTTs {
		h := w.Host(lcName(rtt), []string{lcSession(rtt)})
		x.hosts = append(x.hosts, h)
		w.Knobs(h.Name, func(k *fakenet.Knobs) { x.shape(rtt, k) })
	}
	return x
}

// start writes hosts.toml and starts the home, then waits for every
// remote to be up.
func (x *lcWorld) start(entry func(rtt int, h *config.Host)) {
	var remotes []config.Host
	for i, rtt := range lcRTTs {
		r := x.hosts[i].Remote()
		if entry != nil {
			entry(rtt, &r)
		}
		remotes = append(remotes, r)
	}
	x.w.Home(x.a, remotes...)
	for _, h := range x.hosts {
		x.w.WaitLink(x.a, h.Name, "up", 20*time.Second)
	}
}

// shape sets a remote's link: a shared master, delay rtt/2, then the
// scenario's own knobs.
func (x *lcWorld) shape(rtt int, k *fakenet.Knobs) {
	k.Mux, k.DelayMs = true, rtt/2
	if x.knobs != nil {
		x.knobs(rtt, k)
	}
}

// attachAll puts a loop on every remote: a terminal each, attached
// through the picker.
func (x *lcWorld) attachAll() {
	x.w.T.Helper()
	for i, rtt := range lcRTTs {
		h, s := x.hosts[i], lcSession(rtt)
		x.terms = append(x.terms, x.w.LoopTo(h.Name, x.a, nil, s, "^"+h.Name+":"+s))
		x.w.Eventually(10*time.Second, h.Name+"'s client", func() bool { return slices.Equal(h.Clients(), []string{s}) })
	}
}

// tracker records, per host, when each event was first seen since its
// last reset: stalled, down (down or connecting, or a new attempt since
// the last poll), notup (anything but up), attempt (a new connect), back
// (up again after notup); and for the loops' clients noclient (not the
// wanted session alone, or every client replaced) and clientback.
type tracker struct {
	x     *lcWorld
	mu    sync.Mutex
	start time.Time
	seen  map[string]time.Duration
	att   map[string]int
	pids  map[string][]int // the clients at the reset
	stop  chan struct{}
}

func (x *lcWorld) track() *tracker {
	tr := &tracker{x: x, stop: make(chan struct{})}
	tr.reset()
	go func() {
		for i := 0; ; i++ {
			select {
			case <-tr.stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			st := x.a.Status()
			if st == nil {
				continue
			}
			var clients map[string][]int
			if i%2 == 0 && len(x.terms) > 0 {
				clients = map[string][]int{}
				for j, h := range x.hosts {
					clients[h.Name] = h.clientPids(lcSession(lcRTTs[j]))
				}
			}
			tr.mu.Lock()
			now := time.Since(tr.start)
			mark := func(name, ev string) {
				if _, ok := tr.seen[name+" "+ev]; !ok {
					tr.seen[name+" "+ev] = now
				}
			}
			for _, l := range st.Detail.Links {
				newAttempt := l.Attempts != tr.att[l.Name]
				tr.att[l.Name] = l.Attempts
				if l.Stalled || l.Status == "stalled" {
					mark(l.Name, "stalled")
				}
				if l.Status == "down" || l.Status == "connecting" || newAttempt {
					mark(l.Name, "down")
				}
				if newAttempt {
					mark(l.Name, "attempt")
				}
				if l.Status != "up" || l.Stalled || newAttempt {
					mark(l.Name, "notup")
				} else if _, ok := tr.seen[l.Name+" notup"]; ok {
					mark(l.Name, "back")
				}
			}
			for name, pids := range clients {
				replaced := !slices.ContainsFunc(pids, func(p int) bool { return slices.Contains(tr.pids[name], p) })
				switch {
				case len(pids) != 1:
					mark(name, "noclient")
				case replaced:
					mark(name, "noclient")
					mark(name, "clientback")
				default:
					if _, ok := tr.seen[name+" noclient"]; ok {
						mark(name, "clientback")
					}
				}
			}
			tr.mu.Unlock()
		}
	}()
	x.w.T.Cleanup(func() { close(tr.stop) })
	return tr
}

// clientPids are the pids of h's clients: those on session, and -1 for
// any client elsewhere.
func (h *Host) clientPids(session string) []int {
	out, _ := h.Tmux("list-clients", "-F", "#{client_control_mode} #{session_name} #{client_pid}")
	var pids []int
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 || f[0] != "0" {
			continue
		}
		pid := -1
		if f[1] == session {
			fmt.Sscan(f[2], &pid)
		}
		pids = append(pids, pid)
	}
	return pids
}

// reset restarts the clock; a host not up now counts as notup at 0.
func (tr *tracker) reset() {
	st := tr.x.a.Status()
	pids := map[string][]int{}
	for i, h := range tr.x.hosts {
		pids[h.Name] = h.clientPids(lcSession(lcRTTs[i]))
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.start = time.Now()
	tr.seen = map[string]time.Duration{}
	tr.att = map[string]int{}
	tr.pids = pids
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

// LC01: a cold start lists every host with its sessions; attaches; a wake
// brings every client back; a remote upgraded by a reload; ssh's options.
func TestLC01(t *testing.T) {
	x := makeLC(t, "lc01", nil)
	w := x.w
	// The home starting: no host is ever up without its sessions.
	empty := map[string]bool{}
	upAt := map[string]time.Duration{}
	done, polled := make(chan struct{}), make(chan struct{})
	start := time.Now()
	go func() {
		defer close(polled)
		for {
			select {
			case <-done:
				return
			case <-time.After(10 * time.Millisecond):
			}
			st := x.a.Status()
			if st == nil {
				continue
			}
			for _, l := range st.Detail.Links {
				if l.Status == "up" {
					if l.Sessions == 0 {
						empty[l.Name] = true
					}
					if _, ok := upAt[l.Name]; !ok {
						upAt[l.Name] = time.Since(start)
					}
				}
			}
		}
	}()
	x.start(func(rtt int, h *config.Host) { h.ObscureKeystrokes = rtt == 400 })
	close(done)
	<-polled
	for _, h := range x.hosts {
		t.Logf("%s up %v after the home started", h.Name, upAt[h.Name].Round(time.Millisecond))
		if empty[h.Name] {
			t.Fatalf("%s was up with no sessions", h.Name)
		}
	}
	// Attach, each from the picker.
	for i, rtt := range lcRTTs {
		h, s := x.hosts[i], lcSession(rtt)
		w.ClearMarks()
		x.terms = append(x.terms, w.LoopTo(h.Name, x.a, nil, s, "^"+h.Name+":"+s))
		w.Eventually(10*time.Second, h.Name+"'s client", func() bool { return slices.Equal(h.Clients(), []string{s}) })
		var prep, att time.Time
		for _, m := range w.ReadMarks() {
			switch {
			case m.What == "prepare" && prep.IsZero():
				prep = m.At
			case m.What == "attach" && att.IsZero():
				att = m.At
			}
		}
		t.Logf("%s: prepare → attach %v", h.Name, att.Sub(prep).Round(time.Millisecond))
	}
	// A wake: every client back.
	tr := x.track()
	time.Sleep(200 * time.Millisecond)
	tr.reset()
	if err := x.a.Call(proto.CallWake, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, h := range x.hosts {
		back := tr.wait(h.Name, "back", 20*time.Second)
		cb := tr.wait(h.Name, "clientback", 20*time.Second)
		t.Logf("%s after a wake: back %v, client back %v", h.Name, back, cb)
		if cb < 0 {
			t.Fatalf("%s's client never came back", h.Name)
		}
	}
	// Upgrade R50 by a reload.
	hosts, _ := config.LoadHosts(x.a.Paths().HostsFile())
	for i := range hosts {
		if hosts[i].Name == "R50" {
			hosts[i].Tower = tower2
		}
	}
	config.SaveHosts(x.a.Paths().HostsFile(), hosts)
	x.a.Call(proto.CallReload, nil, nil)
	time.Sleep(100 * time.Millisecond)
	w.Eventually(20*time.Second, "R50 up at "+Version2, func() bool {
		l := w.Link(x.a, "R50")
		return l.Status == "up" && l.Version == Version2
	})
	// ssh's options.
	okt := map[string]map[string]bool{}
	for _, c := range w.SSHLog() {
		h, _ := c["host"].(string)
		if c["op"] == "check" {
			t.Fatalf("ssh -O check was called: %v", c)
		}
		if okt[h] == nil {
			okt[h] = map[string]bool{}
		}
		v, _ := c["okt"].(string)
		okt[h][v] = true
	}
	for _, rtt := range lcRTTs {
		want := "no"
		if rtt == 400 {
			want = ""
		}
		if got := okt[lcName(rtt)]; len(got) != 1 || !got[want] {
			t.Fatalf("%s: ObscureKeystrokeTiming %v, want %q", lcName(rtt), got, want)
		}
	}
}

// LC02: a clean drop of a link up for the stable period is retried within
// 200ms, and every loop's client is back.
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
		cb := tr.wait(h.Name, "clientback", 30*time.Second)
		t.Logf("%s: dropped seen %v, retried %v later, back %v, client back %v", h.Name, notup, attempt-notup, back, cb)
		if notup < 0 || attempt < 0 || back < 0 {
			t.Fatalf("%s never came back", h.Name)
		}
		// 200ms, plus the tracker's 10ms polls on either side.
		if attempt-notup > 220*time.Millisecond {
			t.Fatalf("%s retried %v after the drop", h.Name, attempt-notup)
		}
		if cb < 0 {
			t.Fatalf("%s's client never came back", h.Name)
		}
	}
}

// LC03: a link half-open after a network change: back in seconds, faster
// with the interface watcher, every loop's client with it.
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
				cb := tr.wait(h.Name, "clientback", 40*time.Second)
				back, _ := tr.get(h.Name, "back")
				stalled, _ := tr.get(h.Name, "stalled")
				down, _ := tr.get(h.Name, "down")
				t.Logf("%s (%s): stalled %v, given up %v, back %v, client back %v", h.Name, mode, stalled, down, back, cb)
				if cb < 0 || cb > limit {
					t.Fatalf("%s's client back after %v (limit %v)", h.Name, cb, limit)
				}
			}
		})
	}
}

// LC04: a 20s blackout: every loop's client back within 10s of the
// network's return.
func TestLC04(t *testing.T) {
	x := newLC(t, "lc04", nil)
	x.attachAll()
	tr := x.track()
	at := time.Now()
	for i, rtt := range lcRTTs {
		x.w.Knobs(x.hosts[i].Name, func(k *fakenet.Knobs) { x.shape(rtt, k); k.Freeze, k.HalfOpenAt = true, at.UnixMilli() })
	}
	tr.reset()
	for _, h := range x.hosts {
		t.Logf("%s down after %v", h.Name, tr.wait(h.Name, "down", 30*time.Second))
	}
	time.Sleep(time.Until(at.Add(20 * time.Second)))
	for i, rtt := range lcRTTs {
		x.w.Knobs(x.hosts[i].Name, func(k *fakenet.Knobs) {
			*k = fakenet.Knobs{Env: k.Env, Drop: k.Drop}
			x.shape(rtt, k)
			k.HalfOpenAt = at.UnixMilli()
		})
	}
	tr.reset()
	for _, h := range x.hosts {
		back := tr.wait(h.Name, "back", 60*time.Second)
		cb := tr.wait(h.Name, "clientback", 60*time.Second)
		t.Logf("%s after the blackout: back %v, client back %v", h.Name, back, cb)
		if cb < 0 || cb > 10*time.Second {
			t.Fatalf("%s's client back after %v", h.Name, cb)
		}
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
		if _, ok := tr.get(h.Name, "noclient"); ok {
			bad = append(bad, h.Name+" lost its client")
		}
		if got := x.w.Link(x.a, h.Name).Attempts; got != attempts[h.Name] {
			bad = append(bad, fmt.Sprintf("%s reconnected (%d → %d)", h.Name, attempts[h.Name], got))
		}
		if !slices.Equal(h.Clients(), []string{lcSession(lcRTTs[i])}) {
			bad = append(bad, h.Name+" has no client")
		}
	}
	t.Logf("%d network changes; seen stalled: %v", n, stalled)
	if len(bad) > 0 {
		t.Fatalf("a slow but live link was dropped: %v", bad)
	}
}

// LC06: a switch to a host whose master went half-open a second ago
// lands within seconds: the attach is given up as the host stalls, and a
// pick after that waits for the new link.
func TestLC06(t *testing.T) {
	x := newLC(t, "lc06", nil)
	var terms []*Term
	for _, h := range x.hosts {
		term := x.w.Loop(h.Name, x.a, nil)
		term.Wait(Prompt, 6*time.Second)
		terms = append(terms, term)
	}
	time.Sleep(3 * time.Second)
	at := time.Now().UnixMilli()
	for i, rtt := range lcRTTs {
		x.w.Knobs(x.hosts[i].Name, func(k *fakenet.Knobs) { x.shape(rtt, k); k.HalfOpenAt = at })
	}
	time.Sleep(time.Second)
	type result struct {
		took  time.Duration
		picks int
		ok    bool
	}
	res := make([]result, len(lcRTTs))
	var wg sync.WaitGroup
	for i, rtt := range lcRTTs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 100 * time.Millisecond)
			h, s, term := x.hosts[i], lcSession(rtt), terms[i]
			start := time.Now()
			term.Pick(s)
			last, picks := time.Now(), 1
			var since time.Time // the client there, continuously
			for time.Since(start) < 40*time.Second {
				if slices.Equal(h.Clients(), []string{s}) {
					if since.IsZero() {
						since = time.Now()
					}
					if time.Since(since) >= time.Second {
						res[i] = result{since.Sub(start), picks, true}
						return
					}
				} else {
					since = time.Time{}
				}
				if time.Since(last) > time.Second {
					// The picker refused the pick, saying why; the
					// reason goes once the host is back, the picker
					// stays.
					if sc := term.Screen(); strings.Contains(sc, Prompt) && (strings.Contains(sc, "not responding") || time.Since(last) > 2*time.Second) {
						term.Keys("C-u") // the picker keeps the query
						term.Pick(s)
						last = time.Now()
						picks++
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			res[i] = result{time.Since(start), picks, false}
		}()
	}
	wg.Wait()
	for i, r := range res {
		t.Logf("%s: attached in %v, %d picks", x.hosts[i].Name, r.took.Round(time.Millisecond), r.picks)
		if !r.ok || r.took > 12*time.Second {
			t.Errorf("%s: attached %v after %v", x.hosts[i].Name, r.ok, r.took)
		}
	}
}
