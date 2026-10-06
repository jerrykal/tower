package scenario

import (
	"bytes"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

// S04: a server restarts and reuses $0: prepare refuses the old listing,
// and an attach in flight to the old instance exits 43 without a client.
func TestS04(t *testing.T) {
	w := NewWorld(t, "s04")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"old"}, SSHHost())
	stdSetup(w, a, b)
	old := w.Ref(a, "B", "old")
	var p proto.Prepared
	if err := a.Call(proto.CallPrepare, proto.PrepareArgs{Loop: "deadbeef", Target: old}, &p); err != nil {
		t.Fatal(err)
	}
	w.RestartServer(b, "impostor")
	t.Logf("impostor is %s (old was %s)", b.SessionID("impostor"), old.Session)
	w.Eventually(6*time.Second, "B's new instance at the home", func() bool {
		inst := w.Link(a, "B").Inst
		return inst != "" && inst != old.Inst
	})
	err := a.Call(proto.CallPrepare, proto.PrepareArgs{Loop: "deadbeef", Target: old}, nil)
	if err == nil || !strings.Contains(err.Error(), "restarted") {
		t.Fatalf("prepare of the old listing: %v", err)
	}
	sh := ""
	for _, a := range p.Argv {
		sh += " " + shellQuote(a)
	}
	x := w.Term("x", a, nil, "/bin/sh", "-c", sh+"; echo ATTACH-EXIT=$?")
	x.Wait(`ATTACH-EXIT=43`, 6*time.Second)
	if c := b.Clients(); len(c) != 0 {
		t.Fatalf("B has clients %v", c)
	}
}

// S06: exit 42 is never trusted alone: from a program in the pane, from a
// dashboard of an earlier attach, from ssh itself.
func TestS06(t *testing.T) {
	w := NewWorld(t, "s06")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	stdSetup(w, a, b)
	term := w.LoopTo("t", a, nil, "bravo", "^B:bravo")

	// (a) A program in the pane.
	term.Type("tmux detach-client -E 'exit 42'")
	term.Keys("Enter")
	term.Wait(`exit 42 without a valid hand-off \(no request\)`, 6*time.Second)
	before := w.WaitLoop(a, "^B:bravo", time.Second)
	if before.ID == "" {
		t.Fatal("the loop moved")
	}

	// (b) A stale dashboard. The loop's target is the same session as
	// before the pick: wait for the pick's attach, a new generation.
	term.Pick("bravo")
	var l proto.LoopStatus
	w.Eventually(5*time.Second, "the pick's attach", func() bool {
		l = w.WaitLoop(a, "^B:bravo", 5*time.Second)
		return l.Gen > before.Gen
	})
	time.Sleep(300 * time.Millisecond)
	ids := b.ClientIDs("bravo")
	if len(ids) != 1 {
		t.Fatalf("clients on bravo: %v", ids)
	}
	out, err := b.UI(ids[0], map[string]string{"TOWER_TEST_GEN": strconv.Itoa(l.Gen - 1)}, "goto", "A", "alpha")
	if err == nil || !strings.Contains(out, "earlier attach") {
		t.Fatalf("a stale switch: %v %q", err, out)
	}
	w.clientsAre(b, time.Second, "bravo")
	if st := a.Status(); len(st.Detail.Pending) != 0 {
		t.Fatalf("pending %+v", st.Detail.Pending)
	}
	alpha := w.Ref(a, "A", "alpha")
	if err := a.Call(proto.CallPlant, proto.Request{Loop: l.ID, Gen: l.Gen - 1, Nonce: "planted", Target: alpha}, nil); err != nil {
		t.Fatal(err)
	}
	term.Type("tmux detach-client -E 'exit 42'")
	term.Keys("Enter")
	term.Wait(`earlier attach`, 6*time.Second)
	if st := a.Status(); len(st.Detail.Pending) != 0 {
		t.Fatalf("pending %+v", st.Detail.Pending)
	}

	// (c) ssh itself exits 42: the fake's only (real ssh exits 255 or
	// with its command; towerd's tests read a bare 42).
	if w.real {
		return
	}
	term.Pick("bravo")
	w.WaitLoop(a, "^B:bravo", 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	w.ExitWith("B", 42)
	time.Sleep(200 * time.Millisecond)
	term.Keys("C-b", "d")
	term.Wait(`exit 42 without a valid hand-off \(no request\)`, 6*time.Second)
	w.Reset("B")
	if c := a.Clients(); len(c) != 0 {
		t.Fatalf("A has clients %v", c)
	}
}

// S07: a dashboard that dies once its switch is stored: the eager loop
// completes it; with the dashboard doing the detach it stays pending and
// prefix d discards it; a late exit 42 is refused.
func TestS07(t *testing.T) {
	w := NewWorld(t, "s07")
	a := w.Host("A", []string{"alpha"}, Env("TOWER_HANDOFF_TTL", "1500"))
	b := w.Host("B", []string{"bravo"}, Env("TOWER_TEST_CRASH", "after-switch"), SSHHost())
	stdSetup(w, a, b)

	// (a) The eager loop ends the client itself.
	t0 := w.LoopTo("0", a, nil, "bravo", "^B:bravo")
	t0.DashTo("alpha")
	w.WaitLoop(a, "^A:alpha", 6*time.Second)
	w.Eventually(time.Second, "B without clients or registrations", func() bool {
		return len(b.Clients()) == 0 && len(b.Regs()) == 0
	})
	if b.SessionID("bravo") == "" {
		t.Fatal("bravo is gone")
	}
	if st := a.Status(); len(st.Detail.Pending) != 0 {
		t.Fatalf("pending %+v", st.Detail.Pending)
	}
	t0.Keys("C-b", "d")
	t0.Wait(`LOOP-EXIT=0`, 6*time.Second)

	// (b) The dashboard ends the client, and dies first.
	a.Set("TOWER_EAGER", "0")
	restartHome(w, a, b)
	lazy := map[string]string{"TOWER_EAGER": "0"}
	t1 := w.LoopTo("1", a, lazy, "bravo", "^B:bravo")
	t1.DashTo("alpha")
	w.Eventually(3*time.Second, "one pending switch", func() bool { return len(a.Status().Detail.Pending) == 1 })
	time.Sleep(500 * time.Millisecond)
	w.clientsAre(b, time.Second, "bravo")
	t1.Keys("C-b", "d")
	t1.Wait(`discarded[\s\S]*LOOP-EXIT=0`, 6*time.Second)
	if st := a.Status(); len(st.Detail.Pending) != 0 {
		t.Fatalf("pending %+v", st.Detail.Pending)
	}

	// (c) A late exit 42.
	t2 := w.LoopTo("1b", a, lazy, "bravo", "^B:bravo")
	t2.DashTo("alpha")
	w.Eventually(3*time.Second, "one pending switch", func() bool { return len(a.Status().Detail.Pending) == 1 })
	time.Sleep(3 * time.Second)
	t2.Type("tmux detach-client -E 'exit 42'")
	t2.Keys("Enter")
	t2.Wait(`request is \d+s old`, 6*time.Second)
	if c := a.Clients(); len(c) != 0 {
		t.Fatalf("A has clients %v", c)
	}
}

// restartHome stops the home's towerd and starts it again with the
// host's current environment.
func restartHome(w *World, a *Host, remotes ...*Host) {
	w.T.Helper()
	if pid := a.TowerdPid(); pid > 0 {
		a.Call(proto.CallStop, proto.StopArgs{}, nil)
		w.WaitGone(pid, 5*time.Second)
	}
	w.StartTowerd(a)
	names := []string{}
	for _, r := range remotes {
		names = append(names, r.Name)
	}
	w.WaitUp(a, names...)
}

// S08: three terminals on one session, two of them from one home and one
// from another; one of them hands off and only it moves.
func TestS08(t *testing.T) {
	w := NewWorld(t, "s08")
	a := w.Host("A", []string{"alpha"})
	a2 := w.Host("A2", []string{"other"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	stdSetup(w, a, b)
	stdSetup(w, a2, b)
	w.LoopTo("1", a, nil, "bravo", "^B:bravo")
	t2 := w.Loop("2", a, nil)
	t2.Wait(Prompt, 6*time.Second)
	t2.Pick("bravo")
	w.LoopTo("3", a2, nil, "bravo", "^B:bravo")
	w.Eventually(5*time.Second, "three clients on B", func() bool { return len(b.Clients()) == 3 })
	time.Sleep(300 * time.Millisecond)
	t2.DashTo("alpha")
	w.clientsAre(a, 8*time.Second, "alpha")
	time.Sleep(500 * time.Millisecond)
	if n := len(b.Clients()); n != 2 {
		t.Fatalf("B has %d clients", n)
	}
	var at []string
	for _, l := range w.Loops(a) {
		f := strings.SplitN(FormatRef(l.Cur), ":", 3)
		at = append(at, f[0]+":"+f[1])
	}
	slices.Sort(at)
	if !slices.Equal(at, []string{"A:alpha", "B:bravo"}) {
		t.Fatalf("A's loops are at %v", at)
	}
	if ls := w.Loops(a2); len(ls) != 1 || !strings.HasPrefix(FormatRef(ls[0].Cur), "B:bravo") {
		t.Fatalf("A2's loops: %+v", ls)
	}
}

// S10: prefix d ends tower and leaves the session; the next tower
// reattaches to it.
func TestS10(t *testing.T) {
	w := NewWorld(t, "s10")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	stdSetup(w, a, b)
	term := w.LoopTo("t", a, nil, "bravo", "^B:bravo")
	id := w.WaitLoop(a, "^B:bravo", time.Second).ID
	if n := len(b.Regs()); n != 1 {
		t.Fatalf("B has %d registrations", n)
	}
	term.Keys("C-b", "d")
	el := term.Wait(`LOOP-EXIT=0`, 5*time.Second)
	t.Logf("prefix d → the terminal back in %v", el.Round(time.Millisecond))
	if b.SessionID("bravo") == "" {
		t.Fatal("bravo is gone")
	}
	w.Eventually(3*time.Second, "no registration on B", func() bool { return len(b.Regs()) == 0 })
	w.Eventually(3*time.Second, "the loop gone from the home", func() bool {
		return !slices.ContainsFunc(w.Loops(a), func(l proto.LoopStatus) bool { return l.ID == id })
	})
	w.Loop("2", a, map[string]string{"TOWER_TEST_PICKER": "0"})
	w.clientsAre(b, 6*time.Second, "bravo")
}

// S12: windows by id: base-index 0 and 1 make no difference.
func TestS12(t *testing.T) {
	w := NewWorld(t, "s12")
	a := w.Host("A", []string{"azero"}, BaseIndex(0))
	b := w.Host("B", []string{"bone"}, SSHHost())
	for _, h := range []*Host{a, b} {
		s := "azero"
		if h == b {
			s = "bone"
		}
		for _, n := range []string{"second", "third"} {
			h.MustTmux("new-window", "-d", "-t", s+":", "-n", n)
		}
	}
	stdSetup(w, a, b)
	term := w.Loop("t", a, nil)
	term.Wait(Prompt, 6*time.Second)
	// A session and a window name: the finder's cursor on the window.
	term.Pick("bone second")
	w.Eventually(5*time.Second, "B's client on 2:second", func() bool { return b.ClientWindow() == "2 second" })
	time.Sleep(300 * time.Millisecond)
	ids := b.ClientIDs("bone")
	if len(ids) != 1 {
		t.Fatalf("clients on bone: %v", ids)
	}
	if out, err := b.UI(ids[0], nil, "goto", "A", "azero", "1"); err != nil {
		t.Fatalf("goto A azero 1: %v %s", err, out)
	}
	w.Eventually(8*time.Second, "A's client on 1:second", func() bool { return a.ClientWindow() == "1 second" })
}

// S24: previous and current across hosts, kept by the home and relearned
// from the loop's beat after the home restarts.
func TestS24(t *testing.T) {
	w := NewWorld(t, "s24")
	a := w.Host("A", []string{"alpha", "apple"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	stdSetup(w, a, b)
	t1 := w.LoopTo("1", a, nil, "alpha", "^A:alpha")
	t1.DashTo("bravo")
	w.WaitLoop(a, "^B:bravo", 8*time.Second)
	w.LoopTo("2", a, nil, "apple", "^A:apple")
	loops := func() (l1, l2 proto.LoopStatus) {
		for _, l := range w.Loops(a) {
			if strings.HasPrefix(FormatRef(l.Cur), "B:bravo") || l.Prev.Session != "" && strings.HasPrefix(FormatRef(l.Prev), "B:bravo") {
				l1 = l
			} else {
				l2 = l
			}
		}
		return
	}
	l1, l2 := loops()
	if !strings.HasPrefix(FormatRef(l1.Prev), "A:alpha") || !l2.Prev.IsZero() {
		t.Fatalf("previous: l1 %s, l2 %s", FormatRef(l1.Prev), FormatRef(l2.Prev))
	}
	prevKey := func() {
		time.Sleep(300 * time.Millisecond)
		t1.Keys("M-o")
		t1.Wait(Prompt, 6*time.Second)
		time.Sleep(300 * time.Millisecond)
		t1.Keys("-")
		time.Sleep(400 * time.Millisecond)
		t1.Keys("Enter")
	}
	prevKey()
	w.WaitLoop(a, "^A:alpha", 6*time.Second)
	if err := a.Call(proto.CallWake, nil, nil); err != nil {
		t.Fatal(err)
	}
	w.WaitLink(a, "B", "up", 5*time.Second)
	time.Sleep(500 * time.Millisecond)
	l1, _ = loops()
	if !strings.HasPrefix(FormatRef(l1.Prev), "B:bravo") {
		t.Fatalf("l1's previous after the wake: %s", FormatRef(l1.Prev))
	}
	id := l1.ID
	Kill9(a.TowerdPid())
	w.WaitGone(a.TowerdPid(), 3*time.Second)
	w.StartTowerd(a)
	w.WaitLink(a, "B", "up", 6*time.Second)
	w.Eventually(8*time.Second, "l1 relearned with its previous", func() bool {
		for _, l := range w.Loops(a) {
			if l.ID == id && strings.HasPrefix(FormatRef(l.Prev), "B:bravo") {
				return true
			}
		}
		return false
	})
	prevKey()
	w.WaitLoop(a, "^B:bravo", 8*time.Second)
	if _, l2 := loops(); !strings.HasPrefix(FormatRef(l2.Cur), "A:apple") {
		t.Fatalf("l2 moved to %s", FormatRef(l2.Cur))
	}
}

// S25: everyday habits: the last pane closing, tower last, nothing left on
// a host, nothing left anywhere, prefix d.
func TestS25(t *testing.T) {
	w := NewWorld(t, "s25")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo", "spare"}, SSHHost())
	c := w.Host("C", []string{"charlie"}, SSHHost())
	stdSetup(w, a, b, c)

	// (a) The last pane of a session closes, another session on the server.
	term := w.LoopTo("t", a, nil, "bravo", "^B:bravo")
	term.Type("exit")
	term.Keys("Enter")
	w.WaitLoop(a, "^B:spare", 5*time.Second)
	w.clientsAre(b, 3*time.Second, "spare")

	// (b) tower last across hosts.
	term.DashTo("charlie")
	w.WaitLoop(a, "^C:charlie", 6*time.Second)
	time.Sleep(400 * time.Millisecond)
	start := time.Now()
	term.Keys("M-l")
	w.WaitLoop(a, "^B:spare", 6*time.Second)
	t.Logf("prefix L C → B: %v", time.Since(start).Round(time.Millisecond))
	time.Sleep(400 * time.Millisecond)
	term.Keys("M-l")
	w.WaitLoop(a, "^C:charlie", 6*time.Second)

	// (c) Nothing left on C.
	time.Sleep(400 * time.Millisecond)
	term.Type("exit")
	start = time.Now()
	term.Keys("Enter")
	w.WaitLoop(a, "^B:spare", 6*time.Second)
	t.Logf("charlie ended → on B:spare in %v", time.Since(start).Round(time.Millisecond))
	term.Wait(`charlie ended; now on B:spare`, 4*time.Second)
	if c.HasServer() {
		t.Fatal("C's server is still running")
	}

	// (d) Nothing anywhere.
	a.MustTmux("kill-session", "-t", "alpha")
	time.Sleep(500 * time.Millisecond)
	term.Type("exit")
	term.Keys("Enter")
	term.Wait(`LOOP-EXIT=0`, 6*time.Second)

	// (e) prefix d.
	w.RestartServer(b, "fresh")
	w.Eventually(5*time.Second, "B:fresh in the home's view", func() bool { return HasSession(&a.View("").View, "B", "fresh") })
	t2 := w.LoopTo("2", a, nil, "fresh", "^B:fresh")
	t2.Keys("C-b", "d")
	el := t2.Wait(`LOOP-EXIT=0`, 5*time.Second)
	t.Logf("prefix d: %v", el.Round(time.Millisecond))
	if b.SessionID("fresh") == "" {
		t.Fatal("fresh is gone")
	}
}

// syncHosts turns synchronized output on in the hosts' tmux servers, so
// their clients draw synced frames, as with a terminal that has it.
func syncHosts(hs ...*Host) {
	for _, h := range hs {
		h.MustTmux("set", "-as", "terminal-features", ",*:sync")
	}
}

// S26: a hand-off shows no flash: the frame is held from before the old
// client leaves the alternate screen until the new one has entered it,
// then released; nothing of tower's is drawn in between.
func TestS26(t *testing.T) {
	w := NewWorld(t, "s26")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, Env("TOWER_TEST_NOTTY", "1"), SSHHost())
	syncHosts(a, b)
	stdSetup(w, a, b)
	term := w.Loop("t", a, nil)
	term.Record()
	term.Wait(Prompt, 6*time.Second)
	term.Pick("bravo")
	w.WaitLoop(a, "^B:bravo", 6*time.Second)
	for _, hop := range []struct{ name, query, re string }{{"B → A", "alpha", "^A:alpha"}, {"A → B", "bravo", "^B:bravo"}} {
		time.Sleep(700 * time.Millisecond)
		mark := len(term.Recording())
		term.DashTo(hop.query)
		w.WaitLoop(a, hop.re, 8*time.Second)
		time.Sleep(2 * time.Second)
		b := term.Recording()[mark:]
		hold := bytes.Index(b, seqHold)
		leave := bytes.LastIndex(b, seqLeave)
		enter := bytes.LastIndex(b, seqEnter)
		if hold < 0 || leave < hold || enter < leave {
			t.Fatalf("%s: hold at %d, leave at %d, enter at %d", hop.name, hold, leave, enter)
		}
		ev, heldAtEnd := frames(b)
		for _, e := range ev {
			if (e.at == leave || e.at == enter) && !e.held {
				t.Fatalf("%s: the frame was not held at %d (enter %v)", hop.name, e.at, e.enter)
			}
		}
		if heldAtEnd {
			t.Fatalf("%s: the frame is still held", hop.name)
		}
		if bytes.Contains(b[leave:enter], []byte("[tower]")) || bytes.Contains(b[leave:enter], []byte("tower:")) {
			t.Fatalf("%s: tower drew between the clients: %q", hop.name, b[leave:enter])
		}
	}
}

// S27: rapid prefix L: every screen change of six quick hand-offs is held.
func TestS27(t *testing.T) {
	w := NewWorld(t, "s27")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	syncHosts(a, b)
	stdSetup(w, a, b)
	term := w.Loop("t", a, nil)
	term.Record()
	term.Wait(Prompt, 6*time.Second)
	term.Pick("alpha")
	w.WaitLoop(a, "^A:alpha", 6*time.Second)
	time.Sleep(300 * time.Millisecond)
	term.DashTo("bravo")
	w.WaitLoop(a, "^B:bravo", 8*time.Second)
	time.Sleep(700 * time.Millisecond)
	mark := len(term.Recording())
	for _, pause := range []int{120, 180, 240, 120, 180, 240} {
		term.Keys("M-l")
		time.Sleep(time.Duration(pause) * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	ev, heldAtEnd := frames(term.Recording()[mark:])
	leaves, bad := 0, 0
	for _, e := range ev {
		if !e.enter {
			leaves++
		}
		if !e.held {
			bad++
		}
	}
	t.Logf("%d screen changes, %d leaves, %d not held", len(ev), leaves, bad)
	if leaves < 3 || bad != 0 || heldAtEnd {
		t.Fatalf("%d leaves, %d changes not held, held at the end %v", leaves, bad, heldAtEnd)
	}
}

// D01: tower dash: a second terminal picks its own target while the
// first stays; esc in tower dash attaches to the last target, or exits
// when there is none.
func TestD01(t *testing.T) {
	w := NewWorld(t, "d01")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	// With no last target, esc exits.
	stdSetup(w, a, b)
	t0 := w.Term("0", a, nil, towerBin, "dash")
	t0.Wait(Prompt, 6*time.Second)
	t0.Keys("Escape")
	t0.Wait(`LOOP-EXIT=0`, 5*time.Second)

	w.LoopTo("1", a, nil, "bravo", "^B:bravo")
	t2 := w.Term("2", a, map[string]string{"TOWER_TEST_PICKER": "0"}, towerBin, "dash")
	t2.Wait(Prompt, 6*time.Second)
	t2.Pick("alpha")
	w.clientsAre(a, 6*time.Second, "alpha")
	w.clientsAre(b, time.Second, "bravo")
	time.Sleep(300 * time.Millisecond)
	t2.Keys("C-b", "d")
	t2.Wait(`LOOP-EXIT=0`, 5*time.Second)

	// esc attaches to the last target (A:alpha, the last one prepared).
	t3 := w.Term("3", a, nil, towerBin, "dash")
	t3.Wait(Prompt, 6*time.Second)
	t3.Keys("Escape")
	w.clientsAre(a, 6*time.Second, "alpha")
	w.clientsAre(b, time.Second, "bravo")
}

func shellQuote(s string) string {
	if regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`).MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
