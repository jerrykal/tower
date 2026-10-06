package scenario

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// The standby and relay scenarios: hosts with a pty (ssh -t gets one, as
// from sshd) over a shared master.

// statusBar is the host tmux's status line naming session s.
func statusBar(s string) string { return `\[\S+:` + regexp.QuoteMeta(s) + `\]` }

// noStandbys waits until no standby process to any of hs remains.
func (w *World) noStandbys(d time.Duration, hs ...*Host) {
	w.T.Helper()
	w.Eventually(d, "no standby processes", func() bool {
		for _, h := range hs {
			if len(w.StandbyPids(h)) > 0 {
				return false
			}
		}
		return true
	})
}

// clientSize is "WxH" of h's non-control client.
func (h *Host) clientSize() string {
	out, _ := h.Tmux("list-clients", "-F", "#{client_control_mode} #{client_width}x#{client_height}")
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if rest, ok := strings.CutPrefix(l, "0 "); ok {
			return rest
		}
	}
	return ""
}

// LS01: hand-offs through standbys; the host left gets a new one; the
// terminal's size reaches the client; standbys are neither clients nor
// registrations; prefix d and a killed loop leave none behind.
func TestLS01(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "ls01")
	a := w.Host("A", []string{"alpha"})
	// A killed loop's standbys ride the master, which outlives the loop,
	// and the fake keeps their sessions (decision 112): they hear no
	// hang-up and go once their heartbeats stop.
	silence := Env("TOWER_STANDBY_SILENCE", "5s")
	b := w.Host("B", []string{"bravo"}, silence, SSHHost())
	c := w.Host("C", []string{"charlie"}, silence, SSHHost())
	w.Shape("B", ptyLink)
	w.Shape("C", ptyLink)
	stdSetup(w, a, b, c)
	term := w.LoopTo("t", a, nil, "alpha", "^A:alpha")
	w.WaitMark("standby: ready B", 5*time.Second)
	w.WaitMark("standby: ready C", 5*time.Second)
	for _, h := range []*Host{b, c} {
		if len(h.Regs()) != 0 || len(h.Clients()) != 0 {
			t.Fatalf("%s: registrations %v, clients %v", h.Name, h.Regs(), h.Clients())
		}
	}
	if out, _ := a.UI(a.ClientIDs("alpha")[0], nil, "rows"); strings.Contains(out, "standby") {
		t.Fatalf("the rows show a standby: %s", out)
	}
	term.tmux("resize-window", "-t", "term:", "-x", "96", "-y", "28")
	size, _ := term.tmux("display-message", "-p", "-t", "term:", "#{pane_width}x#{pane_height}")
	size = strings.TrimSpace(size)
	time.Sleep(300 * time.Millisecond)
	w.ClearMarks()
	term.DashTo("bravo")
	w.WaitLoop(a, "^B:bravo", 8*time.Second)
	w.WaitMark("standby: taken", 3*time.Second)
	term.Wait(statusBar("bravo"), 5*time.Second)
	w.Eventually(3*time.Second, "B's client at the terminal's size "+size, func() bool { return b.clientSize() == size })
	term.Type("echo ls$((40+2))")
	term.Keys("Enter")
	term.Wait(`ls42`, 5*time.Second)
	w.Eventually(3*time.Second, "one registration on B", func() bool { return len(b.Regs()) == 1 })
	w.ClearMarks()
	term.DashTo("charlie")
	w.WaitLoop(a, "^C:charlie", 8*time.Second)
	w.WaitMark("standby: taken", 3*time.Second)
	w.WaitMark("standby: ready B", 5*time.Second)
	term.Wait(statusBar("charlie"), 5*time.Second)
	term.Keys("C-b", "d")
	term.Wait(`LOOP-EXIT=0`, 5*time.Second)
	w.noStandbys(3*time.Second, b, c)

	// ssh given the terminal leaves the same modes.
	t2 := w.Loop("2", a, map[string]string{"TOWER_RELAY": "0"})
	t2.Wait(Prompt, 6*time.Second)
	t2.Pick("charlie")
	w.WaitLoop(a, "^C:charlie", 8*time.Second)
	t2.Wait(statusBar("charlie"), 5*time.Second)
	t2.Keys("C-b", "d")
	t2.Wait(`LOOP-EXIT=0`, 5*time.Second)
	m1, m2 := term.Stty(), t2.Stty()
	if m1 == "" || m1 != m2 {
		t.Fatalf("modes after the relay %q, after ssh given the terminal %q", m1, m2)
	}

	// A killed loop's standbys go with it.
	w.ClearMarks()
	t3 := w.LoopTo("3", a, nil, "alpha", "^A:alpha")
	w.WaitMark("standby: ready B", 5*time.Second)
	w.WaitMark("standby: ready C", 5*time.Second)
	pids := a.LoopPids()
	if len(pids) != 1 {
		t.Fatalf("loop pid files: %v", pids)
	}
	Kill9(pids[0])
	t3.Wait(`LOOP-EXIT=137`, 5*time.Second)
	w.noStandbys(8*time.Second, b, c)
}

// LS02: a standby that does not answer is given up after its wait and a
// new session lands, the frame held throughout.
func TestLS02(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "ls02")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Shape("B", func(l *Link) { ptyLink(l); l.DelayMs = 25 })
	stdSetup(w, a, b)
	term := w.Loop("t", a, nil)
	term.Record()
	term.Wait(Prompt, 6*time.Second)
	term.Pick("alpha")
	w.WaitLoop(a, "^A:alpha", 6*time.Second)
	w.WaitMark("standby: ready B", 5*time.Second)
	time.Sleep(500 * time.Millisecond)
	stopped := w.StandbyPids(b)
	if len(stopped) == 0 {
		t.Fatal("no standby to B")
	}
	for _, p := range stopped {
		syscall.Kill(p, syscall.SIGSTOP)
	}
	defer func() {
		for _, p := range stopped {
			syscall.Kill(p, syscall.SIGCONT)
		}
	}()
	mark := len(term.Recording())
	w.ClearMarks()
	start := time.Now()
	term.DashTo("bravo")
	w.WaitLoop(a, "^B:bravo", 10*time.Second)
	term.Wait(statusBar("bravo"), 8*time.Second)
	t.Logf("on B in %v", time.Since(start).Round(time.Millisecond))
	var goAt, gaveUp time.Time
	for _, m := range w.ReadMarks() {
		switch m.What {
		case "standby: go":
			goAt = m.At
		case "standby did not answer":
			gaveUp = m.At
		case "standby: taken":
			t.Fatal("the stuck standby was used")
		}
	}
	if goAt.IsZero() || gaveUp.Before(goAt) {
		t.Fatalf("go at %v, given up at %v", goAt, gaveUp)
	}
	t.Logf("given up %v after its go line", gaveUp.Sub(goAt).Round(time.Millisecond))
	if w.CountMarks("attach: session") == 0 {
		t.Fatal("no new session")
	}
	time.Sleep(time.Second)
	rec := term.Recording()[mark:]
	hold, enter := bytes.Index(rec, seqHold), bytes.LastIndex(rec, seqEnter)
	if hold < 0 || enter < hold {
		t.Fatalf("hold at %d, enter at %d", hold, enter)
	}
	ev, _ := frames(rec)
	for _, e := range ev {
		if e.at == enter && !e.held {
			t.Fatal("the frame was not held as the new client entered")
		}
	}
	for _, p := range stopped {
		syscall.Kill(p, syscall.SIGCONT)
	}
	w.Eventually(5*time.Second, "the stuck standby gone", func() bool {
		now := w.StandbyPids(b)
		return !slices.ContainsFunc(stopped, func(p int) bool { return slices.Contains(now, p) })
	})
}

// replaced waits until none of old is still a standby to h and more
// "standby: ready" marks for h arrived than n.
func (w *World) replaced(h *Host, old []int, n int, d time.Duration) {
	w.T.Helper()
	w.Eventually(d, "a new standby to "+h.Name, func() bool {
		now := w.StandbyPids(h)
		gone := !slices.ContainsFunc(old, func(p int) bool { return slices.Contains(now, p) })
		return gone && w.CountMarks("standby: ready "+h.Name) > n
	})
}

// LS03: a wake, a network change, a towerd killed and a stall each
// replace the standby; one made for an earlier link is never used.
func TestLS03(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "ls03")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Shape("B", ptyLink)
	stdSetup(w, a, b)
	term := w.LoopTo("t", a, nil, "alpha", "^A:alpha")
	w.WaitMark("standby: ready B", 5*time.Second)

	// (a) Wake.
	old, n := w.StandbyPids(b), w.CountMarks("standby: ready B")
	a.Call(proto.CallWake, nil, nil)
	w.replaced(b, old, n, 10*time.Second)

	// (b) A network change with the master half-open.
	time.Sleep(500 * time.Millisecond)
	old, n = w.StandbyPids(b), w.CountMarks("standby: ready B")
	changed := time.Now()
	w.NetworkChange("B", changed)
	time.Sleep(time.Second)
	a.Call(proto.CallNetChange, nil, nil)
	// Over real ssh the old standby's far side never hears its
	// connection close: it lives on until its loop's heartbeats have
	// been silent for 30s (checked at the end).
	old, stranded := b.SplitFar(old)
	w.replaced(b, old, n, 15*time.Second)
	w.Heal("B")
	standbys := func() []int {
		return slices.DeleteFunc(w.StandbyPids(b), func(p int) bool { return slices.Contains(stranded, p) })
	}

	// (c) B's towerd killed, and a switch there right after.
	w.WaitLink(a, "B", "up", 10*time.Second)
	time.Sleep(time.Second)
	old, attempts := standbys(), w.Link(a, "B").Attempts
	w.ClearMarks()
	Kill9(b.TowerdPid())
	// The switch as soon as B is back, on a new link, the standby made for
	// the earlier one perhaps still there. (Before that, the dashboard
	// refuses B: it is connecting.)
	w.Eventually(10*time.Second, "B back on a new link", func() bool {
		l := w.Link(a, "B")
		return l.Attempts > attempts && l.Status == "up"
	})
	term.DashTo("bravo")
	w.WaitLoop(a, "^B:bravo", 10*time.Second)
	marks := w.MarkTexts()
	if i := slices.Index(marks, "standby: taken"); i >= 0 && !slices.Contains(marks[:i], "standby: start B") {
		t.Fatalf("a standby made for the earlier link was used: %v", marks)
	}
	w.Eventually(5*time.Second, "the old standby gone", func() bool {
		now := w.StandbyPids(b)
		return !slices.ContainsFunc(old, func(p int) bool { return slices.Contains(now, p) })
	})
	term.DashTo("alpha")
	w.WaitLoop(a, "^A:alpha", 8*time.Second)
	w.WaitMark("standby: ready B", 10*time.Second)

	// (d) A stall.
	time.Sleep(500 * time.Millisecond)
	old, n = standbys(), w.CountMarks("standby: ready B")
	w.Stall("B", true)
	w.Eventually(10*time.Second, "the standby to the stalled B gone", func() bool {
		now := w.StandbyPids(b)
		return !slices.ContainsFunc(old, func(p int) bool { return slices.Contains(now, p) })
	})
	w.Stall("B", false)
	w.Eventually(20*time.Second, "a new standby to B", func() bool { return w.CountMarks("standby: ready B") > n })

	// (b)'s stranded far side has exited for want of heartbeats.
	w.Eventually(time.Until(changed.Add(35*time.Second)), "the far side (b) stranded gone", func() bool {
		return !slices.ContainsFunc(stranded, Alive)
	})
}

// LS04: TOWER_STANDBY=0, standby = false on a host, a remote upgraded
// under a standby, and a reload that turns one on and removes another.
func TestLS04(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "ls04")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	c := w.Host("C", []string{"charlie"}, SSHHost())
	for _, n := range []string{"B", "C"} {
		w.Shape(n, func(l *Link) { l.Mux = true })
	}
	link := filepath.Join(w.Dir, "tower-B")
	if err := os.Symlink(towerBin, link); err != nil {
		t.Fatal(err)
	}
	rb := b.Remote()
	rb.Tower = link
	off := false
	rc := c.Remote()
	rc.Standby = &off
	w.Home(a, rb, rc)
	w.WaitUp(a, "A", "B", "C")

	// (a) TOWER_STANDBY=0.
	t0 := w.Loop("0", a, map[string]string{"TOWER_STANDBY": "0"})
	t0.Wait(Prompt, 6*time.Second)
	time.Sleep(2 * time.Second)
	if p := append(w.StandbyPids(b), w.StandbyPids(c)...); len(p) != 0 || slices.ContainsFunc(w.MarkTexts(), func(m string) bool { return strings.Contains(m, "standby") }) {
		t.Fatalf("standbys with TOWER_STANDBY=0: %v %v", p, w.MarkTexts())
	}
	t0.Keys("Escape")
	t0.Wait(`LOOP-EXIT=`, 5*time.Second)

	// (b) standby = false on C.
	term := w.LoopTo("t", a, nil, "alpha", "^A:alpha")
	w.WaitMark("standby: ready B", 5*time.Second)
	time.Sleep(time.Second)
	if len(w.StandbyPids(c)) != 0 || w.CountMarks("standby: start C") != 0 {
		t.Fatal("a standby to C, which has standby = false")
	}

	// (c) B upgraded under its standby.
	old, n := w.StandbyPids(b), w.CountMarks("standby: ready B")
	os.Remove(link)
	os.Symlink(tower2, link)
	Kill9(b.TowerdPid())
	w.Eventually(10*time.Second, "B up at "+Version2, func() bool {
		l := w.Link(a, "B")
		return l.Status == "up" && l.Version == Version2
	})
	w.Eventually(10*time.Second, "a new standby to B", func() bool { return w.CountMarks("standby: ready B") > n })
	w.Eventually(5*time.Second, "the old standby gone", func() bool {
		now := w.StandbyPids(b)
		return !slices.ContainsFunc(old, func(p int) bool { return slices.Contains(now, p) })
	})
	w.ClearMarks()
	term.DashTo("bravo")
	w.WaitLoop(a, "^B:bravo", 10*time.Second)
	w.WaitMark("standby: taken", 3*time.Second)
	w.Eventually(3*time.Second, "one registration on B", func() bool { return len(b.Regs()) == 1 })
	term.DashTo("alpha")
	w.WaitLoop(a, "^A:alpha", 8*time.Second)
	w.WaitMark("standby: ready B", 10*time.Second)

	// (d) One reload: C's standby on, B removed.
	on := true
	rc.Standby = &on
	if err := config.SaveHosts(a.Paths().HostsFile(), []config.Host{rc}); err != nil {
		t.Fatal(err)
	}
	a.Call(proto.CallReload, nil, nil)
	w.WaitMark("standby: ready C", 10*time.Second)
	w.noStandbys(5*time.Second, b)
	term.Keys("C-b", "d")
	term.Wait(`LOOP-EXIT=0`, 5*time.Second)
	w.noStandbys(3*time.Second, b, c)
}

// LS05: job control is that of ssh -t: through a standby, a new relayed
// session and ssh given the terminal alike.
func TestLS05(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "ls05")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Shape("B", ptyLink)
	stdSetup(w, a, b)
	modes := []struct {
		name string
		env  map[string]string
	}{{"plain", map[string]string{"TOWER_RELAY": "0"}}, {"session", map[string]string{"TOWER_STANDBY": "0"}}, {"standby", map[string]string{"TOWER_STANDBY": "1"}}}
	var summaries []string
	for _, m := range modes {
		w.ClearMarks()
		term := w.LoopTo(m.name, a, m.env, "alpha", "^A:alpha")
		if m.name == "standby" {
			w.WaitMark("standby: ready B", 5*time.Second)
		}
		w.ClearMarks()
		term.DashTo("bravo")
		w.WaitLoop(a, "^B:bravo", 8*time.Second)
		term.Wait(statusBar("bravo"), 5*time.Second)
		if r := w.Route(); r != m.name {
			t.Fatalf("%s: the attach went %s", m.name, r)
		}
		var res []string
		term.Type("sleep 30")
		term.Keys("Enter")
		time.Sleep(500 * time.Millisecond)
		term.Keys("C-z")
		_, ok := term.WaitOK(`Stopped|Suspended`, 3*time.Second)
		res = append(res, fmt.Sprintf("job stopped %v", ok))
		term.Type("kill -9 %1; clear")
		term.Keys("Enter")
		time.Sleep(500 * time.Millisecond)
		pid := 0
		out, _ := b.Tmux("list-clients", "-F", "#{client_control_mode} #{client_pid}")
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if rest, ok := strings.CutPrefix(l, "0 "); ok {
				fmt.Sscan(rest, &pid)
			}
		}
		pid = b.HostPid(pid)
		term.Keys("C-b", "C-z")
		time.Sleep(time.Second)
		st, _ := exec.Command("ps", "-o", "state=", "-p", fmt.Sprint(pid)).Output()
		state := strings.TrimSpace(string(st))
		if state != "" {
			state = state[:1]
		}
		res = append(res, "client state "+state)
		_, ok = term.WaitOK(statusBar("bravo"), 100*time.Millisecond)
		res = append(res, fmt.Sprintf("screen while suspended %v", ok))
		syscall.Kill(pid, syscall.SIGCONT)
		_, ok = term.WaitOK(statusBar("bravo"), 3*time.Second)
		res = append(res, fmt.Sprintf("screen back %v", ok))
		term.Type("echo jc$((1+1))")
		term.Keys("Enter")
		_, ok = term.WaitOK(`jc2`, 3*time.Second)
		res = append(res, fmt.Sprintf("keys work %v", ok))
		summaries = append(summaries, strings.Join(res, "; "))
		t.Logf("%s: %s", m.name, summaries[len(summaries)-1])
		term.Keys("C-b", "d")
		term.Wait(`LOOP-EXIT=`, 6*time.Second)
	}
	if summaries[0] != summaries[1] || summaries[1] != summaries[2] {
		t.Fatalf("job control differs:\n%s", strings.Join(summaries, "\n"))
	}
}

// LS09: every remote attach relayed comes out as with ssh given the
// terminal: exits 255, 43 and 42, hand-offs, a host stalling as it is
// switched to, prefix d and the terminal's modes after it.
func TestLS09(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "ls09")
	w.Timing(lhTiming)
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Shape("B", func(l *Link) { l.Pty = true; lhWindow(l) })
	stdSetup(w, a, b)
	restore := func() { w.Heal("B") }
	modes := []struct {
		name string
		env  map[string]string
	}{{"plain", map[string]string{"TOWER_RELAY": "0"}}, {"session", map[string]string{"TOWER_STANDBY": "0"}}, {"standby", map[string]string{"TOWER_STANDBY": "1"}}}
	var results []string
	relayed := 0
	for _, m := range modes {
		w.ClearMarks()
		term := w.Loop(m.name, a, m.env)
		var out []string
		// readyNow waits, in standby mode, for a ready standby to B that
		// has not been used or dropped since.
		readyNow := func() {
			if m.name == "standby" {
				w.Eventually(10*time.Second, "a standby to B ready", func() bool {
					gone := 0
					for _, t := range w.MarkTexts() {
						if t == "standby: taken" || t == "standby: drop B" || strings.HasPrefix(t, "standby: not used B") {
							gone++
						}
					}
					return w.CountMarks("standby: ready B") > gone
				})
			}
		}
		onB := func(step string, viaStandby bool) {
			t.Helper()
			w.WaitLoop(a, "^B:bravo", 10*time.Second)
			term.Wait(statusBar("bravo"), 5*time.Second)
			w.clientsAre(b, 5*time.Second, "bravo")
			route := w.Route()
			want := m.name
			if m.name == "standby" && !viaStandby {
				want = "session"
			}
			if route != want && !(m.name == "standby" && viaStandby && route == "session") {
				t.Fatalf("%s %s: the attach went %s, want %s", m.name, step, route, want)
			}
			if route != "plain" {
				relayed++
			}
			out = append(out, step+": on B")
		}
		pick := func() {
			term.Wait(Prompt, 6*time.Second)
			term.Pick("bravo")
		}
		// 1. The picker, then B.
		readyNow()
		pick()
		onB("1", true)
		// 2. ssh exits 255: reconnect at once. Over real ssh the
		// connection drops, as it does when ssh exits 255.
		if w.real {
			w.Drop("B")
		} else {
			w.ExitWith("B", 255)
			time.Sleep(200 * time.Millisecond) // live connections read knobs every 20ms
			term.Keys("C-b", "d")
		}
		w.WaitMark("after reconnect", 6*time.Second)
		restore()
		onB("2", false)
		// 3 and 4 are tower's own exits from ssh itself, the fake's only:
		// real ssh exits 255 or with its command (towerd's tests read 42
		// and 43 alone).
		if !w.real {
			ls09Exits(w, term, restore, readyNow, onB)
		}
		// 5. A hand-off away.
		term.DashTo("alpha")
		w.WaitLoop(a, "^A:alpha", 8*time.Second)
		w.clientsAre(b, 3*time.Second)
		out = append(out, "5: on A")
		// 6. And back.
		readyNow()
		term.DashTo("bravo")
		onB("6", true)
		// 7. B stalls as it is switched to.
		term.DashTo("alpha")
		w.WaitLoop(a, "^A:alpha", 8*time.Second)
		time.Sleep(time.Second)
		term.Keys("M-o")
		term.Wait(Prompt, 6*time.Second)
		term.Type("bravo")
		time.Sleep(250 * time.Millisecond)
		w.Stall("B", true)
		time.Sleep(300 * time.Millisecond)
		term.Keys("Enter")
		term.Wait(`(?i)B[^\n]*(not responding|stalled)`, 20*time.Second)
		w.WaitLoop(a, "^A:alpha", 10*time.Second)
		w.clientsAre(b, 10*time.Second)
		out = append(out, "7: told, on A")
		// 8. B back.
		restore()
		w.WaitLink(a, "B", "up", 20*time.Second)
		readyNow()
		if _, ok := term.WaitOK(Prompt, 200*time.Millisecond); ok {
			term.Keys("Escape")
		}
		term.DashTo("bravo")
		onB("8", true)
		// 9. prefix d.
		term.Keys("C-b", "d")
		term.Wait(`LOOP-EXIT=0`, 6*time.Second)
		out = append(out, "9: modes "+term.Stty())
		results = append(results, strings.Join(out, "; "))
		t.Logf("%s: %s", m.name, results[len(results)-1])
	}
	t.Logf("%d attaches relayed", relayed)
	if results[0] != results[1] || results[1] != results[2] {
		t.Fatalf("the modes differ:\n%s", strings.Join(results, "\n"))
	}
}

// ls09Exits are LS09's steps 3 and 4: ssh itself exits 43, then a bare
// 42, each back at the picker saying why.
func ls09Exits(w *World, term *Term, restore, readyNow func(), onB func(string, bool)) {
	w.T.Helper()
	// 3. exit 43: the picker, saying why.
	w.ExitWith("B", 43)
	time.Sleep(200 * time.Millisecond) // live connections read knobs every 20ms
	term.Keys("C-b", "d")
	term.Wait(`B restarted since it was listed; pick again`, 6*time.Second)
	restore()
	readyNow()
	term.Pick("bravo")
	onB("3", true)
	// 4. A bare exit 42: the picker, saying why.
	w.ExitWith("B", 42)
	time.Sleep(200 * time.Millisecond) // live connections read knobs every 20ms
	term.Keys("C-b", "d")
	term.Wait(`exit 42 without a valid hand-off \(no request\)`, 6*time.Second)
	restore()
	readyNow()
	term.Pick("bravo")
	onB("4", true)
}

// LS10: a remote attach over a shared master leaves no client behind,
// even where its session there outlives its ssh (as the fake's does, and
// as in real use; decision 112): hand-offs back and forth
// leave each host only the client the terminal is on, and a killed
// loop's client goes once the home calls the loop gone.
func TestLS10(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "ls10")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	c := w.Host("C", []string{"charlie"}, SSHHost())
	w.Shape("B", ptyLink)
	w.Shape("C", ptyLink)
	stdSetup(w, a, b, c)
	term := w.LoopTo("t", a, nil, "alpha", "^A:alpha")
	w.WaitMark("standby: ready B", 5*time.Second)
	w.WaitMark("standby: ready C", 5*time.Second)
	hops := []struct {
		to, left *Host
		session  string
	}{{b, c, "bravo"}, {c, b, "charlie"}, {b, c, "bravo"}, {c, b, "charlie"}}
	for i, hop := range hops {
		term.DashTo(hop.session)
		w.WaitLoop(a, "^"+hop.to.Name+":"+hop.session, 8*time.Second)
		term.Wait(statusBar(hop.session), 5*time.Second)
		start := time.Now()
		w.clientsAre(hop.to, 3*time.Second, hop.session)
		w.clientsAre(hop.left, 3*time.Second)
		w.Eventually(3*time.Second, "no registration left on "+hop.left.Name, func() bool { return len(hop.left.Regs()) == 0 })
		t.Logf("hand-off %d: %s left clean in %v", i+1, hop.left.Name, time.Since(start).Round(time.Millisecond))
	}

	// A killed loop's client: its session outlives the ssh here, until the
	// home has called the loop gone (15s without a beat) and the loop has
	// stayed unknown for as long again.
	pids := a.LoopPids()
	if len(pids) != 1 {
		t.Fatalf("loop pid files: %v", pids)
	}
	Kill9(pids[0])
	term.Wait(`LOOP-EXIT=137`, 5*time.Second)
	time.Sleep(2 * time.Second)
	// Over real ssh (OpenSSH 9.6 at both ends, in the host containers)
	// the session ends with its ssh, and with it the client.
	if got := c.Clients(); !w.real && !slices.Equal(got, []string{"charlie"}) {
		t.Fatalf("C's clients 2s after the loop died: %v; want the session still there", got)
	}
	start := time.Now()
	w.clientsAre(c, 40*time.Second)
	t.Logf("the killed loop's client gone %v later", (2*time.Second + time.Since(start)).Round(100*time.Millisecond))
}
