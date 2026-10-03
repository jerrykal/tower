package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/test/scenario/fakenet"
)

// lhTiming is the slow-host family's timing: production keepalive, and
// states and views padded to 30 KB so a stalled pipe fills.
var lhTiming = map[string]string{"TOWER_SILENCE": "15000", "TOWER_TEST_PAD": "30000"}

// slowKnobs is a 500ms round trip with jitter and 512 KB/s.
func slowKnobs(k *fakenet.Knobs) { k.DelayMs, k.JitterMs, k.BwKBps = 250, 80, 512 }

// lhWorld is A, F, S and Z with the home on A, every host up, and a loop
// on F:fox.
type lhWorld struct {
	w          *World
	a, f, s, z *Host
	loop       *FakeLoop
	times      map[string][]time.Duration
}

func newLH(t *testing.T, id string, sk, zk func(*fakenet.Knobs)) *lhWorld {
	w := NewWorld(t, id)
	w.Timing(lhTiming)
	x := &lhWorld{w: w, times: map[string][]time.Duration{}}
	x.a = w.Host("A", []string{"alpha", "apple"})
	x.f = w.Host("F", []string{"fox", "fig"})
	x.s = w.Host("S", []string{"sun"})
	x.z = w.Host("Z", []string{"zed"})
	if sk != nil {
		w.Knobs("S", sk)
	}
	if zk != nil {
		w.Knobs("Z", zk)
	}
	w.Home(x.a, x.f.Remote(), x.s.Remote(), x.z.Remote())
	for _, n := range []string{"F", "S", "Z"} {
		w.WaitLink(x.a, n, "up", 10*time.Second)
	}
	x.loop = w.FakeLoop(x.a)
	x.loop.Attach(w.Ref(x.a, "F", "fox"), x.f)
	return x
}

func (x *lhWorld) note(key string, d time.Duration) { x.times[key] = append(x.times[key], d) }

func (x *lhWorld) report(t *testing.T) {
	keys := make([]string, 0, len(x.times))
	for k := range x.times {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ds := slices.Clone(x.times[k])
		slices.Sort(ds)
		t.Logf("%-28s median %-8v all %v", k, ds[len(ds)/2].Round(time.Millisecond), ds)
	}
}

// round is one round of actions: freshness both ways, the rows a
// dashboard on F reads, requests both ways, and switches F → A → S → A →
// F through the home.
func (x *lhWorld) round(t *testing.T, n int) {
	w, a, f := x.w, x.a, x.f
	cl := func(h *Host, s string) string {
		ids := h.ClientIDs(s)
		if len(ids) == 0 {
			t.Fatalf("no client on %s:%s", h.Name, s)
		}
		return ids[0]
	}
	// Freshness.
	pa, pf := fmt.Sprintf("pa%d", n), fmt.Sprintf("pf%d", n)
	start := time.Now()
	a.NewSession(pa)
	f.NewSession(pf)
	fc := cl(f, "fox")
	w.Eventually(5*time.Second, "A's new session on F, F's in the home's view", func() bool {
		return HasSession(&f.View(fc).View, "A", pa) && HasSession(&a.View("").View, "F", pf)
	})
	x.note("freshness", time.Since(start))
	// The rows a dashboard on F reads (loop part: the popup itself).
	start = time.Now()
	v := f.View(fc)
	if !HasSession(&v.View, "A", "alpha") || !HasSession(&v.View, "S", "sun") {
		t.Fatal("F's rows lack A or S")
	}
	x.note("rows on F", time.Since(start))
	// Requests, twice: the home on F, and F's dashboard on A.
	for range 2 {
		for _, step := range []struct {
			on       *Host
			host     string
			from, to string
			key      string
		}{{a, "F", "fig", "fig2", "rename F from the home"}, {a, "F", "fig2", "fig", ""}, {f, "A", "apple", "apple2", "rename A from F"}, {f, "A", "apple2", "apple", ""}} {
			start := time.Now()
			ack, _ := step.on.Act("", proto.Request{Op: proto.OpRename, Target: w.Ref(a, step.host, step.from), Name: step.to}, 0)
			if !ack.OK {
				t.Fatalf("rename %s:%s: %+v", step.host, step.from, ack)
			}
			if step.key != "" {
				x.note(step.key, time.Since(start))
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	// Switches through the home, each landing a client on its target.
	// loop part: the dashboard's ⏎, the local A → A switch and prefix L.
	cur := f
	for _, hop := range []struct {
		to   *Host
		sess string
	}{{a, "alpha"}, {x.s, "sun"}, {a, "alpha"}, {f, "fox"}} {
		start := time.Now()
		ack, _ := cur.Act(cl(cur, x.loop.cur.Label), proto.Request{Op: proto.OpSwitch, Target: w.Ref(a, hop.to.Name, hop.sess)}, 0)
		if !ack.OK {
			t.Fatalf("switch to %s:%s: %+v", hop.to.Name, hop.sess, ack)
		}
		next := x.loop.After(42, false)
		if next.Do != proto.NextHandoff {
			t.Fatalf("after: %+v", next)
		}
		x.loop.Attach(next.Target, hop.to)
		x.note("switch → "+hop.to.Name, time.Since(start))
		cur = hop.to
		time.Sleep(500 * time.Millisecond)
	}
}

// LH01: baseline round trips with a slow host and a stalled host in the
// list (here neither is shaped).
func TestLH01(t *testing.T) {
	x := newLH(t, "lh01", nil, nil)
	for n := range 3 {
		x.round(t, n)
	}
	x.report(t)
}

// LH02: S slow, Z stalled each round: a change elsewhere still reaches
// every dashboard fast.
func TestLH02(t *testing.T) {
	window := func(k *fakenet.Knobs) { k.WindowKB = 32 }
	x := newLH(t, "lh02", slowKnobs, window)
	for n := range 3 {
		x.w.Knobs("Z", func(k *fakenet.Knobs) { *k = fakenet.Knobs{Env: k.Env, Drop: k.Drop, WindowKB: 32} })
		x.w.WaitLink(x.a, "Z", "up", 20*time.Second)
		time.Sleep(time.Second)
		x.w.Knobs("Z", func(k *fakenet.Knobs) { k.Stall = true })
		for i := range 6 {
			x.a.NewSession(fmt.Sprintf("churn%d", i))
			time.Sleep(200 * time.Millisecond)
		}
		for i := range 6 {
			x.a.MustTmux("kill-session", "-t", fmt.Sprintf("churn%d", i))
		}
		x.round(t, n)
	}
	x.report(t)
	x.w.ResetKnobs("Z")
}

// LH04: two homes on one remote, one home's link stalled: the other
// home's view keeps flowing.
func TestLH04(t *testing.T) {
	w := NewWorld(t, "lh04")
	w.Timing(lhTiming)
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"})
	f := w.Host("F", []string{"fox"})
	w.SSH("Fb", f, fakenet.Knobs{WindowKB: 32})
	w.Home(a, f.Remote())
	fb := f.Remote()
	fb.Name, fb.SSH = "Fb", "Fb"
	w.Home(b, fb)
	w.WaitLink(a, "F", "up", 10*time.Second)
	w.WaitLink(b, "Fb", "up", 10*time.Second)
	w.Eventually(5*time.Second, "two homes on F", func() bool { return len(f.LiveHomes()) == 2 })
	var took []time.Duration
	for round := range 3 {
		w.Knobs("Fb", func(k *fakenet.Knobs) { *k = fakenet.Knobs{Env: k.Env, Drop: k.Drop, WindowKB: 32} })
		w.WaitLink(b, "Fb", "up", 30*time.Second)
		w.Eventually(5*time.Second, "two homes on F", func() bool { return len(f.LiveHomes()) == 2 })
		time.Sleep(time.Second)
		w.Knobs("Fb", func(k *fakenet.Knobs) { k.Stall = true })
		for i := range 6 {
			f.NewSession(fmt.Sprintf("c%d", i))
			time.Sleep(200 * time.Millisecond)
		}
		probe := fmt.Sprintf("probe%d", round)
		start := time.Now()
		f.NewSession(probe)
		w.Eventually(30*time.Second, "F:"+probe+" at A", func() bool { return HasSession(&a.View("").View, "F", probe) })
		el := time.Since(start)
		took = append(took, el)
		if el > 3*time.Second {
			t.Fatalf("round %d: %v for F:%s to reach the other home", round, el, probe)
		}
		for i := range 6 {
			f.MustTmux("kill-session", "-t", fmt.Sprintf("c%d", i))
		}
	}
	t.Logf("F's change at the other home while one home's link stalled: %v", took)
	w.ResetKnobs("Fb")
}

// LH05: a wake with one wedged master: every other host is back fast.
func TestLH05(t *testing.T) {
	w := NewWorld(t, "lh05")
	w.Timing(lhTiming)
	a := w.Host("A", []string{"alpha"})
	f := w.Host("F", []string{"fox"})
	wh := w.Host("W", []string{"wolf"})
	w.Home(a, wh.Remote(), f.Remote())
	w.WaitLink(a, "W", "up", 10*time.Second)
	w.WaitLink(a, "F", "up", 10*time.Second)
	w.Knobs("W", func(k *fakenet.Knobs) { k.ODelayMs = 2500 })
	var took []time.Duration
	for range 3 {
		before := w.Link(a, "F")
		start := time.Now()
		go a.Call(proto.CallWake, nil, nil)
		w.Eventually(20*time.Second, "F reconnected", func() bool {
			l := w.Link(a, "F")
			return l.Attempts > before.Attempts && l.Status == "up" && l.Link > before.Link
		})
		el := time.Since(start)
		took = append(took, el)
		if el > 2*time.Second {
			t.Fatalf("F back %v after a wake", el)
		}
		w.WaitLink(a, "W", "up", 20*time.Second)
		time.Sleep(time.Second)
	}
	t.Logf("F back after a wake, W's master wedged: %v", took)
}

// LH06: a towerd wedged on a healthy link with a loop's client attached
// there: marked stalled, the stream alone given up, a new towerd started,
// and the client never cut.
func TestLH06(t *testing.T) {
	w := NewWorld(t, "lh06")
	w.Timing(ProductionTimings)
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"})
	w.Knobs("B", func(k *fakenet.Knobs) { k.Mux, k.DelayMs = true, 25 })
	w.Home(a, b.Remote())
	w.WaitLink(a, "B", "up", 15*time.Second)
	l := w.FakeLoop(a)
	l.Attach(w.Ref(a, "B", "bravo"), b)
	w.Eventually(10*time.Second, "B's client on bravo", func() bool { return slices.Equal(b.Clients(), []string{"bravo"}) })
	time.Sleep(2 * time.Second)
	old := b.TowerdPid()
	t.Cleanup(func() { syscall.Kill(old, syscall.SIGCONT); Kill9(old) })
	masterFile := filepath.Join(w.Fake, "masters", "B")
	master, _ := os.ReadFile(masterFile)
	exits := func() int {
		n := 0
		for _, c := range w.SSHLog() {
			if c["host"] == "B" && c["op"] == "exit" {
				n++
			}
		}
		return n
	}
	exits0 := exits()
	gen0 := w.Link(a, "B").Link

	var mu sync.Mutex
	var seq []string
	noclient := false
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			st := HostIn(&a.View("").View, "B")
			cl := b.Clients()
			mu.Lock()
			if st != nil && (len(seq) == 0 || seq[len(seq)-1] != st.Status) {
				seq = append(seq, st.Status)
			}
			if !slices.Equal(cl, []string{"bravo"}) {
				noclient = true
			}
			mu.Unlock()
		}
	}()
	start := time.Now()
	syscall.Kill(old, syscall.SIGSTOP)
	w.WaitLink(a, "B", "stalled", 10*time.Second)
	stalled := time.Since(start)
	w.WaitLink(a, "B", "connecting|down", 20*time.Second)
	down := time.Since(start)
	w.Eventually(30*time.Second, "B back", func() bool {
		ls := w.Link(a, "B")
		return ls.Status == "up" && ls.Link > gen0
	})
	back := time.Since(start)
	close(stop)
	<-done
	t.Logf("B stalled after %v, given up after %v, back after %v; states %v", stalled.Round(100*time.Millisecond), down.Round(100*time.Millisecond), back.Round(100*time.Millisecond), seq)
	if noclient || !slices.Equal(b.Clients(), []string{"bravo"}) {
		t.Fatalf("the client was cut: %v", b.Clients())
	}
	if exits() != exits0 {
		t.Fatal("the master was made to exit")
	}
	if m, _ := os.ReadFile(masterFile); string(m) != string(master) {
		t.Fatalf("the master changed: %q → %q", master, m)
	}
	if pid := b.TowerdPid(); pid == 0 || pid == old || Alive(old) {
		t.Fatalf("towerd %d → %d (old alive %v)", old, pid, Alive(old))
	}
	if !regexp.MustCompile(`stalled.*connecting.*up$`).MatchString(strings.Join(seq, " ")) {
		t.Fatalf("states %v", seq)
	}
	w.WaitLoop(a, "^B:bravo", 3*time.Second)
	if n := len(b.Regs()); n != 1 {
		t.Fatalf("%d registrations on B", n)
	}
}
