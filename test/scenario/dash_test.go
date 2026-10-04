package scenario

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/tmux"
)

// Scenarios of the dashboard: the popup, the picker and `tower _ui`.

// Until polls the screen every 3ms until re matches (present) or no
// longer matches (!present), for at most d; it returns the time taken,
// or -1.
func (t *Term) Until(re string, present bool, d time.Duration) time.Duration {
	rx := regexp.MustCompile(re)
	start := time.Now()
	for time.Since(start) < d {
		if rx.MatchString(t.Screen()) == present {
			return time.Since(start)
		}
		time.Sleep(3 * time.Millisecond)
	}
	return -1
}

// CloseDash closes an open dashboard popup: Escape, then the prompt gone.
func (t *Term) CloseDash() {
	t.w.T.Helper()
	time.Sleep(200 * time.Millisecond)
	t.Keys("Escape")
	if t.Until(Prompt, false, 3*time.Second) < 0 {
		t.w.T.Fatalf("terminal %s: the dashboard did not close:\n%s", t.Name, t.Screen())
	}
	time.Sleep(300 * time.Millisecond)
}

// rowRe matches the finder's row of session name on host: the host,
// padded to its column, then the name. (The breadcrumb puts a glyph
// between the two, so it never matches.)
func rowRe(host, name string) string {
	return `(?m)(^|[\s▌])` + regexp.QuoteMeta(host) + ` {2,}` + regexp.QuoteMeta(name) + `( |$)`
}

func (h *Host) hasSession(name string) bool { return slices.Contains(h.Sessions(), name) }

// S01: B is its own home while A's home includes B: one towerd on B plays
// both roles; each client on B is tagged with its home and sees that
// home's view; a hand-off relayed through B leaves B's own loop alone.
func TestS01(t *testing.T) {
	w := NewWorld(t, "s01")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo", "banana"})
	stdSetup(w, a, b)
	w.Home(b) // B's towerd, started by A's bridge: a reload makes it a home
	st := b.Status()
	if st == nil || !st.Home {
		t.Fatalf("B's towerd does not play home: %+v", st)
	}
	if homes := b.LiveHomes(); len(homes) != 1 {
		t.Fatalf("B's homes: %+v", homes)
	}
	if hs := b.HiddenSessions(); !slices.Equal(hs, []string{"_tower:1"}) || b.ControlClients() != 1 {
		t.Fatalf("B's hidden sessions %v, control clients %d", hs, b.ControlClients())
	}
	ta := w.LoopTo("a", a, nil, "bravo", "^B:bravo")
	tb := w.LoopTo("b", b, nil, "banana", "^B:banana")
	onBravo, onBanana := b.ClientIDs("bravo"), b.ClientIDs("banana")
	if len(onBravo) != 1 || len(onBanana) != 1 {
		t.Fatalf("clients on B: %v", b.Clients())
	}
	rows, err := b.UI(onBravo[0], nil, "rows")
	if err != nil || !strings.Contains(rows, "alpha") || !strings.Contains(rows, "bravo") {
		t.Fatalf("rows for A's client on B: %v\n%s", err, rows)
	}
	rows, err = b.UI(onBanana[0], nil, "rows")
	if err != nil || strings.Contains(rows, "alpha") || !strings.Contains(rows, "banana") {
		t.Fatalf("rows for B's own client: %v\n%s", err, rows)
	}
	homeOf := map[string]bool{}
	for _, r := range b.Regs() {
		homeOf[r.Home] = true
	}
	if regs := b.Regs(); len(regs) != 2 || !homeOf[a.Status().ID] || !homeOf[b.Status().ID] {
		t.Fatalf("B's registrations: %+v", regs)
	}
	// A's loop leaves B through B's dashboard: the switch is relayed to A.
	ta.DashTo("alpha")
	w.WaitLoop(a, "^A:alpha", 8*time.Second)
	time.Sleep(500 * time.Millisecond)
	if got := b.Clients(); !slices.Equal(got, []string{"banana"}) {
		t.Fatalf("B's clients after A's hand-off: %v", got)
	}
	w.WaitLoop(b, "^B:banana", time.Second)
	if p := b.Status().Detail.Pending; len(p) != 0 {
		t.Fatalf("B's own loop has a switch pending: %+v", p)
	}
	// B's loop moves on B: its own home, nothing at A.
	tb.DashTo("bravo")
	w.WaitLoop(b, "^B:bravo", 5*time.Second)
	if p := a.Status().Detail.Pending; len(p) != 0 {
		t.Fatalf("A has a switch pending: %+v", p)
	}
}

// S09: a target killed or renamed between listing and ⏎: "selection is
// gone"; an attach by id fails visibly; a renamed session is still
// reached.
func TestS09(t *testing.T) {
	w := NewWorld(t, "s09")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo", "doomed"})
	stdSetup(w, a, b)
	still := map[string]string{"TOWER_LIVE": "0"}
	t1 := w.Loop("t1", a, still)
	t1.Wait("doomed", 6*time.Second)
	link := w.Link(a, "B")
	doomed := b.SessionID("doomed")
	b.MustTmux("kill-session", "-t", doomed)
	t1.Pick("doomed")
	t1.Wait("selection is gone", 5*time.Second)
	if b.hasSession("doomed") {
		t.Fatal("doomed is back")
	}
	var p proto.Prepared
	err := a.Call(proto.CallPrepare, proto.PrepareArgs{Loop: "cafe", Target: proto.Ref{Host: link.ID, Name: "B", Inst: link.Inst, Session: doomed, Label: "doomed"}}, &p)
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("prepare of a killed session: %+v %v", p, err)
	}
	out, err := b.Tmux("attach-session", "-t", doomed)
	if err == nil || !strings.Contains(out+err.Error(), "can't find session") || b.hasSession("doomed") {
		t.Fatalf("attach by id: %v %s", err, out)
	}
	t1.Keys("Escape")
	time.Sleep(300 * time.Millisecond)

	t2 := w.Loop("t2", a, still)
	t2.Wait("bravo", 6*time.Second)
	b.MustTmux("rename-session", "-t", b.SessionID("bravo"), "renamed")
	t2.Pick("bravo")
	w.WaitLoop(a, "^B:renamed", 5*time.Second)
}

// S11: tower inside tmux is the dashboard, never a nested attach;
// dashboards in clients no loop owns, on the home and on a remote, see
// every host and switch locally but cannot hand off; a local target from
// a loop's client is a switch-client in the same attach.
func TestS11(t *testing.T) {
	w := NewWorld(t, "s11")
	a := w.Host("A", []string{"alpha", "apple"})
	b := w.Host("B", []string{"bravo", "berry"})
	stdSetup(w, a, b)

	// (a) tower typed in a tmux session opens the popup there.
	p := w.Term("p", a, nil, tmux.Bin(), "-L", a.Sock, "attach", "-t", "alpha")
	time.Sleep(500 * time.Millisecond)
	p.Type(towerBin + "; echo NEST-EXIT=$?")
	p.Keys("Enter")
	p.Wait(Prompt, 5*time.Second)
	p.Keys("Escape")
	p.Wait("NEST-EXIT=0", 5*time.Second)
	if n := len(a.ClientIDs("")); n != 1 {
		t.Fatalf("A has %d clients", n)
	}
	p.Keys("C-l")

	// (b) a client no loop owns, on the home: every host, a local switch.
	cl := a.ClientIDs("alpha")[0]
	rows, err := a.UI(cl, nil, "rows")
	if err != nil || !strings.Contains(rows, "apple") || !strings.Contains(rows, "bravo") {
		t.Fatalf("rows: %v\n%s", err, rows)
	}
	if out, err := a.UI(cl, nil, "goto", "A", "apple"); err != nil {
		t.Fatalf("goto A apple: %v %s", err, out)
	}
	time.Sleep(300 * time.Millisecond)
	if !slices.Contains(a.Clients(), "apple") {
		t.Fatalf("A's clients: %v", a.Clients())
	}
	// (c) … but no hand-off.
	out, err := a.UI(cl, nil, "goto", "B", "bravo")
	if err == nil || !strings.Contains(out, "⏎ on another host needs the attach loop (run tower outside tmux)") {
		t.Fatalf("goto B from a client no loop owns: %v %s", err, out)
	}

	// (d) the same on a remote.
	w.Term("r", b, nil, tmux.Bin(), "-L", b.Sock, "attach", "-t", "berry")
	time.Sleep(500 * time.Millisecond)
	rcl := b.ClientIDs("berry")
	if len(rcl) != 1 {
		t.Fatalf("B's clients: %v", b.Clients())
	}
	if rows, err := b.UI(rcl[0], nil, "rows"); err != nil || !strings.Contains(rows, "alpha") {
		t.Fatalf("rows on B: %v\n%s", err, rows)
	}
	if v := b.View(rcl[0]); v == nil || v.Owned || !strings.Contains(v.Note, "not a tower terminal: ⏎ to another host needs the attach loop") {
		t.Fatalf("B's view for its own client: %+v", v)
	}
	if out, err := b.UI(rcl[0], nil, "goto", "A", "alpha"); err == nil {
		t.Fatalf("goto A from B's own client: %s", out)
	}
	if n := len(a.ClientIDs("")); n != 1 {
		t.Fatalf("A has %d clients", n)
	}

	// (e) a local target from a loop's client: same attach.
	l := w.LoopTo("l", a, nil, "alpha", "^A:alpha")
	gen := w.WaitLoop(a, "^A:alpha", time.Second).Gen
	l.DashTo("apple")
	if st := w.WaitLoop(a, "^A:apple", 5*time.Second); st.Gen != gen {
		t.Fatalf("a local switch made a new attach: gen %d → %d", gen, st.Gen)
	}
}

// S19: hostile names: made, relayed B → home → C, renamed, picked and
// handed off to, all exact; no name on any ssh command line.
func TestS19(t *testing.T) {
	w := NewWorld(t, "s19")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"})
	c := w.Host("C", []string{"charlie"})
	stdSetup(w, a, b, c)
	names := []string{"a b", "x:y", "p.q", "h#sh", "#{pid}", "q'uote", `dq"x`, "ünï😀", "-lead", "=eq",
		`back\slash`, "$HOME", "~tilde", "semi;colon", "brace{}", "%end 1 1"}
	// stored is a name as tmux keeps it: it escapes a backslash in a
	// session name (C style), so `back\slash` is listed `back\\slash`.
	stored := func(n string) string { return strings.ReplaceAll(n, `\`, `\\`) }
	bid := w.Link(a, "B").ID
	for _, n := range names {
		if ack, _ := b.Act("", proto.Request{Op: proto.OpNew, Target: proto.Ref{Host: bid}, Name: n}, 0); !ack.OK {
			t.Fatalf("new %q: %s", n, ack.Err)
		}
	}
	for _, n := range names {
		if !b.hasSession(stored(n)) {
			t.Fatalf("B lacks %q: %q", n, b.Sessions())
		}
	}
	time.Sleep(600 * time.Millisecond)
	cv := c.View("")
	for _, n := range names {
		if !HasSession(&cv.View, "B", stored(n)) {
			t.Fatalf("C's view lacks B:%q", n)
		}
	}
	for _, n := range names {
		ack, _ := b.Act("", proto.Request{Op: proto.OpRename, Target: proto.Ref{Host: bid, Session: b.SessionID(stored(n))}, Name: n + "²"}, 0)
		if !ack.OK {
			t.Fatalf("rename %q: %s", n, ack.Err)
		}
	}
	for _, n := range names {
		if !b.hasSession(stored(n) + "²") {
			t.Fatalf("B lacks %q: %q", n+"²", b.Sessions())
		}
	}
	term := w.LoopTo("t", a, nil, "alpha", "^A:alpha")
	cl := a.ClientIDs("alpha")[0]
	weird := `#{host} 'q' "d"`
	if out, err := a.UI(cl, nil, "rename", "B", "h#sh²", weird); err != nil || !b.hasSession(weird) {
		t.Fatalf("rename through the dashboard: %v %s; B has %q", err, out, b.Sessions())
	}
	if out, err := a.UI(cl, nil, "goto", "B", "ünï😀²"); err != nil {
		t.Fatalf("goto: %v %s", err, out)
	}
	w.WaitLoop(a, "^B:"+regexp.QuoteMeta("ünï😀²")+"(:@|$)", 6*time.Second)
	for _, n := range []string{"-lead²", "%end 1 1²", "=eq²"} {
		time.Sleep(300 * time.Millisecond)
		ids := b.ClientIDs("")
		if len(ids) != 1 {
			t.Fatalf("B's clients: %v", b.Clients())
		}
		if out, err := b.UI(ids[0], nil, "goto", "B", n); err != nil {
			t.Fatalf("goto %q: %v %s", n, err, out)
		}
		w.WaitLoop(a, "^B:"+regexp.QuoteMeta(n)+"(:@|$)", 6*time.Second)
	}
	term.DashTo("q'uote²")
	w.WaitLoop(a, "^B:"+regexp.QuoteMeta("q'uote²")+"(:@|$)", 6*time.Second)
	for _, e := range w.SSHLog() {
		cmd, _ := e["cmd"].(string)
		for _, bad := range []string{"q'uote", "ünï", "semi;colon", `back\slash`, "brace{}"} {
			if strings.Contains(cmd, bad) {
				t.Fatalf("a name on an ssh command line: %q", cmd)
			}
		}
	}
}
