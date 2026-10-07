package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

// lhTiming is the slow-host family's timing: production keepalive, and
// states and views padded to 30 KB so a stalled pipe fills.
var lhTiming = map[string]string{"TOWER_SILENCE": "15000", "TOWER_TEST_PAD": "30000"}

// slowLink is a 500ms round trip with jitter and 512 KB/s.
func slowLink(l *Link) { l.DelayMs, l.JitterMs, l.BwKBps = 250, 80, 512 }

// lhWindow is a link that stalls: on the fake a 32 KB window, so a stall
// fills the pipe at once. Real ssh's channel window is its own (2 MB):
// there the stall alone holds the link, the stream's writer never blocks
// its caller either way (internal/stream's tests block it).
func lhWindow(l *Link) {
	if !overRealSSH() {
		l.WindowKB = 32
	}
}

// lhWorld is A, F, S and Z with the home on A, every host up, and a loop
// on F:fox.
type lhWorld struct {
	w          *World
	a, f, s, z *Host
	term       *Term
	times      map[string][]time.Duration
}

func newLH(t *testing.T, id string, sk, zk func(*Link)) *lhWorld {
	w := NewWorld(t, id)
	w.Timing(lhTiming)
	x := &lhWorld{w: w, times: map[string][]time.Duration{}}
	x.a = w.Host("A", []string{"alpha", "apple"})
	x.f = w.Host("F", []string{"fox", "fig"}, SSHHost())
	x.s = w.Host("S", []string{"sun"}, SSHHost())
	x.z = w.Host("Z", []string{"zed"}, SSHHost())
	if sk != nil {
		w.Shape("S", sk)
	}
	if zk != nil {
		w.Shape("Z", zk)
	}
	w.Home(x.a, x.f.Remote(), x.s.Remote(), x.z.Remote())
	for _, n := range []string{"F", "S", "Z"} {
		w.WaitLink(x.a, n, "up", 10*time.Second)
	}
	x.term = w.LoopTo("t", x.a, nil, "fox", "^F:fox")
	return x
}

// onSession reports whether h has a client on session s.
func onSession(h *Host, s string) bool { return slices.Contains(h.Clients(), s) }

// dashTo is the dashboard's ⏎ on query in the loop's terminal; it waits
// until to has a client on session.
func (x *lhWorld) dashTo(t *testing.T, query string, to *Host, session string) time.Duration {
	t.Helper()
	start := time.Now()
	x.term.Keys("M-o")
	x.term.Wait(Prompt, 8*time.Second)
	x.term.Type(query)
	time.Sleep(300 * time.Millisecond)
	x.term.Keys("Enter")
	x.w.Eventually(15*time.Second, "a client on "+to.Name+":"+session, func() bool { return onSession(to, session) })
	return time.Since(start)
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
		var ids []string
		w.Eventually(5*time.Second, "a client on "+h.Name+":"+s, func() bool {
			ids = h.ClientIDs(s)
			return len(ids) > 0
		})
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
	// The dashboard opens on F, twice.
	for range 2 {
		start := time.Now()
		x.term.Keys("M-o")
		x.term.Wait(Prompt, 8*time.Second)
		x.w.Eventually(8*time.Second, "the popup's rows", func() bool {
			sc := x.term.Screen()
			return strings.Contains(sc, "alpha") && strings.Contains(sc, "sun")
		})
		x.note("dashboard on F", time.Since(start))
		x.term.Keys("Escape")
		x.w.Eventually(5*time.Second, "the popup closed", func() bool { return !strings.Contains(x.term.Screen(), Prompt) })
		time.Sleep(200 * time.Millisecond)
	}
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
	// Switches from the dashboard, each landing a client on its target.
	x.note("switch F → A", x.dashTo(t, "alpha", a, "alpha"))
	time.Sleep(500 * time.Millisecond)
	x.note("switch A → A", x.dashTo(t, "apple", a, "apple"))
	time.Sleep(500 * time.Millisecond)
	for _, s := range []string{"alpha", "apple"} {
		start := time.Now()
		x.term.Keys("M-l")
		w.Eventually(6*time.Second, "prefix L to A:"+s, func() bool { return onSession(a, s) })
		x.note("prefix L", time.Since(start))
		time.Sleep(400 * time.Millisecond)
	}
	x.note("switch A → S", x.dashTo(t, "sun", x.s, "sun"))
	time.Sleep(800 * time.Millisecond)
	x.note("switch S → A", x.dashTo(t, "alpha", a, "alpha"))
	time.Sleep(500 * time.Millisecond)
	x.note("switch A → F", x.dashTo(t, "fox", f, "fox"))
	time.Sleep(500 * time.Millisecond)
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
	x := newLH(t, "lh02", slowLink, lhWindow)
	for n := range 3 {
		x.w.Heal("Z")
		x.w.WaitLink(x.a, "Z", "up", 20*time.Second)
		time.Sleep(time.Second)
		x.w.Stall("Z", true)
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
	x.w.Reset("Z")
}

// lhTold is the dashboard's or the status line's word that Z is stalled.
var lhTold = regexp.MustCompile(`(?i)(Z|zed)[^\n]*(not responding|stalled|unreachable|lost|down|did not answer|timed out)`)

// bottomLines are the last n non-empty lines of a screen: the status
// lines, the popup's above tmux's own.
func bottomLines(screen string, n int) string {
	var out []string
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			out = append(out, lines[i])
		}
	}
	return strings.Join(out, "\n")
}

// LH03: a switch to a host that stalls: refused fast once it is marked,
// given up fast when it stalls as it is switched to; the terminal back
// where it was, told why; the host usable again once it recovers.
func TestLH03(t *testing.T) {
	x := newLH(t, "lh03", nil, lhWindow)
	w, a, f, z := x.w, x.a, x.f, x.z
	zed := w.Ref(a, "Z", "zed")
	for _, after := range []time.Duration{4 * time.Second, 4 * time.Second, 300 * time.Millisecond, 300 * time.Millisecond} {
		w.Heal("Z")
		w.WaitLink(a, "Z", "up", 30*time.Second)
		time.Sleep(1500 * time.Millisecond)
		w.Eventually(10*time.Second, "the screen no longer telling", func() bool { return !lhTold.MatchString(x.term.Screen()) })
		x.term.Keys("M-o")
		x.term.Wait(Prompt, 8*time.Second)
		x.term.Type("zed")
		w.Stall("Z", true)
		stalled := time.Now()
		if after > time.Second {
			time.Sleep(300 * time.Millisecond)
			ack, took := a.Act("", proto.Request{Op: proto.OpRename, Target: zed, Name: "zed2"}, 0)
			if ack.OK || took > 5*time.Second {
				t.Fatalf("a rename on the stalled Z: %+v in %v", ack, took)
			}
			x.note("refused (rename)", took)
			x.term.Keys("BSpace")
			time.Sleep(time.Until(stalled.Add(after - 300*time.Millisecond)))
			x.term.Type("d")
		}
		time.Sleep(time.Until(stalled.Add(after)))
		x.term.Keys("Enter")
		pressed := time.Now()
		if _, ok := x.term.WaitOK(`(?s).`, 0); ok {
			told := time.Now().Add(30 * time.Second)
			for !lhTold.MatchString(bottomLines(x.term.Screen(), 2)) {
				if time.Now().After(told) {
					t.Fatalf("not told that Z does not answer (⏎ %v after the stall); screen:\n%s", after, x.term.Screen())
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		x.note(fmt.Sprintf("told (⏎ %v after the stall)", after), time.Since(pressed))
		w.Eventually(10*time.Second, "back on F:fox", func() bool { return onSession(f, "fox") })
		// Recovery.
		w.Stall("Z", false)
		w.Eventually(40*time.Second, "Z usable again", func() bool {
			ack, _ := a.Act("", proto.Request{Op: proto.OpHas, Target: zed}, 0)
			return ack.OK
		})
		if !onSession(f, "fox") {
			w.Eventually(30*time.Second, "on Z, on F or at the picker", func() bool {
				return onSession(z, "zed") || onSession(f, "fox") || strings.Contains(x.term.Screen(), Prompt)
			})
			switch {
			case strings.Contains(x.term.Screen(), Prompt):
				x.term.Pick("fox")
			case onSession(z, "zed"):
				time.Sleep(500 * time.Millisecond)
				x.dashTo(t, "fox", f, "fox")
			}
		}
		w.Eventually(15*time.Second, "on F:fox", func() bool { return onSession(f, "fox") })
		if strings.Contains(x.term.Screen(), Prompt) {
			// The popup that refused the switch is still open.
			x.term.Keys("Escape")
			w.Eventually(5*time.Second, "the popup closed", func() bool { return !strings.Contains(x.term.Screen(), Prompt) })
		}
		time.Sleep(time.Second)
	}
	x.report(t)
}

// LH04: two homes on one remote, one home's link stalled: the other
// home's view keeps flowing.
func TestLH04(t *testing.T) {
	w := NewWorld(t, "lh04")
	w.Timing(lhTiming)
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	f := w.Host("F", []string{"fox"}, SSHHost())
	w.SSH("Fb", f)
	w.Shape("Fb", lhWindow)
	// B first: its bridge starts F's towerd, which the stall of B's name
	// must not stop, as a stalled network would not.
	fb := f.Remote()
	fb.Name, fb.SSH = "Fb", "Fb"
	w.Home(b, fb)
	w.WaitLink(b, "Fb", "up", 10*time.Second)
	w.Home(a, f.Remote())
	w.WaitLink(a, "F", "up", 10*time.Second)
	w.Eventually(5*time.Second, "two homes on F", func() bool { return len(f.LiveHomes()) == 2 })
	var took []time.Duration
	for round := range 3 {
		w.Heal("Fb")
		w.WaitLink(b, "Fb", "up", 30*time.Second)
		w.Eventually(5*time.Second, "two homes on F", func() bool { return len(f.LiveHomes()) == 2 })
		time.Sleep(time.Second)
		w.Stall("Fb", true)
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
	w.Reset("Fb")
}

// LH05: a wake with one wedged master: every other host is back fast.
func TestLH05(t *testing.T) {
	w := NewWorld(t, "lh05")
	w.Timing(lhTiming)
	a := w.Host("A", []string{"alpha"})
	f := w.Host("F", []string{"fox"}, SSHHost())
	wh := w.Host("W", []string{"wolf"}, SSHHost())
	w.Home(a, wh.Remote(), f.Remote())
	w.WaitLink(a, "W", "up", 10*time.Second)
	w.WaitLink(a, "F", "up", 10*time.Second)
	// Every ssh -O to W takes 2.5s: the fake's knob, or the home's real
	// master to W stopped for 2.5s from each wake.
	if !w.real {
		w.SlowControl("W", 2500)
	}
	var took []time.Duration
	for range 3 {
		before := w.Link(a, "F")
		if w.real {
			pid := w.masterPid(a, "W")
			if pid == 0 {
				t.Fatal("no master to W")
			}
			Signal(pid, syscall.SIGSTOP)
			t.Cleanup(func() { Signal(pid, syscall.SIGCONT) })
			time.AfterFunc(2500*time.Millisecond, func() { Signal(pid, syscall.SIGCONT) })
		}
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
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Shape("B", func(l *Link) { l.Mux, l.DelayMs = true, 25 })
	w.Home(a, b.Remote())
	w.WaitLink(a, "B", "up", 15*time.Second)
	// A real loop: nothing it does while B's towerd is wedged may cut its
	// client.
	w.LoopTo("t", a, nil, "bravo", "^B:bravo")
	w.Eventually(10*time.Second, "B's client on bravo", func() bool { return slices.Equal(b.Clients(), []string{"bravo"}) })
	time.Sleep(2 * time.Second)
	old := b.TowerdPid()
	if old == 0 {
		t.Fatal("B's towerd is not running")
	}
	t.Cleanup(func() { Signal(old, syscall.SIGCONT); Kill9(old) })
	// The master: the fake's record of it, or the real one's pid.
	masterOf := func() string {
		if w.real {
			return strconv.Itoa(w.masterPid(a, "B"))
		}
		m, _ := os.ReadFile(filepath.Join(w.Fake, "masters", "B"))
		return string(m)
	}
	master := masterOf()
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
	Signal(old, syscall.SIGSTOP)
	w.WaitLink(a, "B", "stalled", 10*time.Second)
	stalled := time.Since(start)
	w.WaitLink(a, "B", "connecting|down", 20*time.Second)
	down := time.Since(start)
	w.Eventually(30*time.Second, "B back", func() bool {
		ls := w.Link(a, "B")
		return ls.Status == "up" && ls.Link > gen0
	})
	back := time.Since(start)
	// Let the view poller catch the same moment.
	w.Eventually(time.Second, "the poller sees B up", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seq) > 0 && seq[len(seq)-1] == "up"
	})
	close(stop)
	<-done
	t.Logf("B stalled after %v, given up after %v, back after %v; states %v", stalled.Round(100*time.Millisecond), down.Round(100*time.Millisecond), back.Round(100*time.Millisecond), seq)
	if noclient || !slices.Equal(b.Clients(), []string{"bravo"}) {
		t.Fatalf("the client was cut: %v", b.Clients())
	}
	if exits() != exits0 {
		t.Fatal("the master was made to exit")
	}
	if m := masterOf(); m != master || m == "0" {
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
