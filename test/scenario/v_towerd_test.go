package scenario

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/tmux"
)

// switchFrom asks h's towerd for a dashboard's switch from client cl.
func switchFrom(t *testing.T, h *Host, cl string, target proto.Ref) *proto.Ack {
	t.Helper()
	ack, _ := h.Act(cl, proto.Request{Op: proto.OpSwitch, Target: target, Nonce: config.NewID()}, 0)
	return ack
}

// V02: upgrading the home with a loop attached: replaced fast; the loop
// re-registers by heartbeat; an older binary never downgrades it; a
// switch from an old dashboard binary works.
func TestV02(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "v02")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Home(a, b.Remote())
	w.WaitUp(a, "B")
	term := w.LoopTo("t", a, nil, "bravo", "^B:bravo")
	old := a.TowerdPid()
	start := time.Now()
	out, err := a.TowerBin(tower2, "_ensure")
	if err != nil {
		t.Fatal(err, out)
	}
	var st proto.Status
	json.Unmarshal([]byte(out), &st)
	t.Logf("upgraded in %v", time.Since(start).Round(time.Millisecond))
	if st.Version != Version2 || st.Pid == old || st.Pid == 0 {
		t.Fatalf("after the upgrade: %+v (was pid %d)", st, old)
	}
	w.WaitLink(a, "B", "up", 6*time.Second)
	w.WaitLoop(a, "^B:bravo", 8*time.Second)
	if warn := w.Link(a, "B").Warn; !strings.Contains(warn, Version+" there, "+Version2+" here") {
		t.Fatalf("B's warning: %q", warn)
	}
	cl := b.ClientIDs("bravo")
	if len(cl) != 1 {
		t.Fatalf("clients on bravo: %v", cl)
	}
	// A switch from the old dashboard binary on B, to the new home.
	term.DashTo("alpha")
	w.WaitLoop(a, "^A:alpha", 8*time.Second)
	out, err = a.Tower("_ensure")
	if err != nil {
		t.Fatal(err, out)
	}
	json.Unmarshal([]byte(out), &st)
	if st.Version != Version2 {
		t.Fatalf("an older binary downgraded the home to %s", st.Version)
	}
}

// V03: upgrading a remote with a loop's client attached there: the next
// bridge replaces its towerd, which restores the client from disk.
func TestV03(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "v03")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	link := filepath.Join(w.Dir, "tower-B")
	if err := os.Symlink(towerBin, link); err != nil {
		t.Fatal(err)
	}
	r := b.Remote()
	r.Tower = link
	w.Home(a, r)
	w.WaitUp(a, "B")
	term := w.LoopTo("t", a, nil, "bravo", "^B:bravo")
	old := b.TowerdPid()
	os.Remove(link)
	os.Symlink(tower2, link)
	start := time.Now()
	if err := a.Call(proto.CallWake, nil, nil); err != nil {
		t.Fatal(err)
	}
	w.Eventually(10*time.Second, "B up on the new version", func() bool {
		ls := w.Link(a, "B")
		pid := b.TowerdPid()
		return ls.Status == "up" && ls.Version == Version2 && pid != 0 && pid != old
	})
	t.Logf("B upgraded in %v", time.Since(start).Round(time.Millisecond))
	// The wake made B's master exit, and with it the attach riding it:
	// the loop attaches again, registering anew.
	var regs []proto.Client
	w.Eventually(5*time.Second, "the loop's client registered on B again", func() bool {
		regs = b.Regs()
		return len(regs) == 1 && regs[0].Name != ""
	})
	if len(regs) != 1 || regs[0].Name == "" {
		t.Fatalf("B's registrations after the upgrade: %+v", regs)
	}
	if warn := w.Link(a, "B").Warn; !strings.Contains(warn, Version2+" there") {
		t.Fatalf("B's warning: %q", warn)
	}
	w.WaitLoop(a, "^B:bravo", 5*time.Second)
	cl := b.ClientIDs("bravo")[0]
	start = time.Now()
	w.Eventually(5*time.Second, "B holds the view", func() bool { return HasSession(&b.View(cl).View, "A", "alpha") })
	t.Logf("B's dashboard has the view %v after the upgrade", time.Since(start).Round(time.Millisecond))
	term.DashTo("alpha")
	w.WaitLoop(a, "^A:alpha", 8*time.Second)
}

// V04: two machines sharing one home directory: two towerds, separate
// run and state dirs; a hand-off between them.
func TestV04(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "v04")
	a := w.Host("A", []string{"alpha"})
	x := w.Host("X", []string{"xray"}, HomeName("shared"), SSHHost())
	y := w.Host("Y", []string{"yankee"}, HomeName("shared"), SSHHost())
	w.Home(a, x.Remote(), y.Remote())
	w.WaitUp(a, "X", "Y")
	if w.Link(a, "X").ID == w.Link(a, "Y").ID {
		t.Fatal("one towerd id for two machines")
	}
	xp, yp := x.Paths(), y.Paths()
	if xp.RunDir == yp.RunDir || xp.StateDir == yp.StateDir {
		t.Fatalf("shared dirs: %s %s / %s %s", xp.RunDir, yp.RunDir, xp.StateDir, yp.StateDir)
	}
	if px, py := x.TowerdPid(), y.TowerdPid(); px == 0 || py == 0 || px == py {
		t.Fatalf("towerd pids %d %d", px, py)
	}
	term := w.LoopTo("t", a, nil, "xray", "^X:xray")
	term.DashTo("yankee")
	w.WaitLoop(a, "^Y:yankee", 8*time.Second)
}

// V05: a remote whose own hosts.toml lists another host stays remote-only
// while only a bridge started it; a loop there makes it a home.
func TestV05(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "v05")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	c := w.Host("C", []string{"charlie"}, SSHHost())
	if err := config.SaveHosts(b.Paths().HostsFile(), []config.Host{c.Remote()}); err != nil {
		t.Fatal(err)
	}
	w.Home(a, b.Remote())
	w.WaitUp(a, "B")
	time.Sleep(2 * time.Second)
	if b.Status().Home {
		t.Fatal("a towerd started by a bridge plays home")
	}
	if n := w.sessionCalls("C"); n != 0 {
		t.Fatalf("%d ssh calls to C", n)
	}
	// loop part: `tower` in a terminal on B; here a loop's beat.
	lb := w.FakeLoop(b)
	w.Eventually(6*time.Second, "B a home linked to C", func() bool {
		st := b.Status()
		return st.Home && len(st.Detail.Links) == 1 && st.Detail.Links[0].Status == "up"
	})
	if w.sessionCalls("C") == 0 {
		t.Fatal("no ssh call to C")
	}
	lb.Attach(w.Ref(b, "C", "charlie"), c)
	if got := c.Clients(); !slices.Equal(got, []string{"charlie"}) {
		t.Fatalf("C's clients: %v", got)
	}
	homes := c.LiveHomes()
	if len(homes) != 1 || homes[0].Name != "B" {
		t.Fatalf("C's homes: %+v", homes)
	}
}

// V06: a dashboard on a remote: its rows fast; a session on C shows on B
// fast; a preview of C's pane from B fast.
func TestV06(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "v06")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	c := w.Host("C", []string{"charlie"}, SSHHost())
	stdSetup(w, a, b, c)
	w.LoopTo("t", a, nil, "bravo", "^B:bravo")
	cl := b.ClientIDs("bravo")[0]
	rows := func() string {
		out, err := b.UI(cl, nil, "rows")
		if err != nil {
			t.Fatalf("rows on B: %v\n%s", err, out)
		}
		return out
	}
	var total, call time.Duration
	for range 10 {
		start := time.Now()
		out := rows()
		total += time.Since(start)
		if !strings.Contains(out, "alpha") || !strings.Contains(out, "charlie") {
			t.Fatalf("B's rows lack other hosts:\n%s", out)
		}
		start = time.Now()
		if v := b.View(cl); v == nil {
			t.Fatal("no view from B")
		}
		call += time.Since(start)
	}
	t.Logf("rows on B: %v a run of `tower _ui rows` (process start included), %v the view call alone",
		(total / 10).Round(time.Microsecond), (call / 10).Round(time.Microsecond))
	start := time.Now()
	c.NewSession("fresh")
	w.Eventually(5*time.Second, "C:fresh in B's rows", func() bool { return strings.Contains(rows(), "fresh") })
	t.Logf("C:fresh in B's rows after %v", time.Since(start).Round(time.Millisecond))
	c.MustTmux("send-keys", "-t", "charlie", "echo PREVIEW-MARK-$((6*7))", "Enter")
	time.Sleep(300 * time.Millisecond)
	ack, el := b.Act(cl, proto.Request{Op: proto.OpCapture, Target: w.Ref(a, "C", "charlie")}, 0)
	if !ack.OK || !strings.Contains(ack.Text, "PREVIEW-MARK-42") {
		t.Fatalf("capture: %+v", ack)
	}
	start = time.Now()
	out, err := b.UI(cl, nil, "preview", "C", "charlie")
	if err != nil || !strings.HasPrefix(out, "C:charlie  ($") || !strings.Contains(out, "PREVIEW-MARK-42") {
		t.Fatalf("preview of C from B: %v\n%s", err, out)
	}
	t.Logf("preview of C from B: the capture call %v, `tower _ui preview` %v", el.Round(time.Microsecond), time.Since(start).Round(time.Microsecond))
}

// V07: the home dies while a loop's client is on a remote: B keeps the
// last view, marked not connected; a switch to A is refused before
// anything detaches; a local switch works; the restarted home relearns
// the loop, and the move made meanwhile.
func TestV07(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "v07")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo", "berry"}, SSHHost())
	w.Home(a, b.Remote())
	w.WaitUp(a, "B")
	term := w.LoopTo("t", a, nil, "bravo", "^B:bravo")
	alpha := w.Ref(a, "A", "alpha")
	cl := b.ClientIDs("bravo")[0]
	ctl := a.Status().Detail.Watch.CtlPid
	Kill9(a.TowerdPid())
	w.Eventually(5*time.Second, "B drops the home", func() bool { return len(b.LiveHomes()) == 0 })
	w.Eventually(3*time.Second, "the dead home's control client ended", func() bool { return !Alive(ctl) })
	v := b.View(cl)
	if !v.Owned || !strings.Contains(v.Note, "not connected") || !HasSession(&v.View, "A", "alpha") {
		t.Fatalf("B's view with the home gone: owned %v, %q", v.Owned, v.Note)
	}
	t.Logf("B's note: %s", v.Note)
	if ack := switchFrom(t, b, cl, alpha); ack.OK || !strings.Contains(ack.Err, "home not connected") {
		t.Fatalf("a switch with the home gone: %+v", ack)
	}
	if got := b.Clients(); !slices.Equal(got, []string{"bravo"}) {
		t.Fatalf("B's clients: %v", got)
	}
	if out, err := b.UI(cl, nil, "goto", "A", "alpha"); err == nil || !strings.Contains(out, "home not connected") {
		t.Fatalf("the dashboard's ⏎ on A with the home gone: %v %s", err, out)
	}
	// The dashboard's ⏎ on berry, on the client's own server.
	if out, err := b.UI(cl, nil, "goto", "B", "berry"); err != nil {
		t.Fatalf("goto B berry: %v %s", err, out)
	}
	w.Eventually(3*time.Second, "the client on berry", func() bool { return slices.Equal(b.Clients(), []string{"berry"}) })
	w.StartTowerd(a)
	w.WaitUp(a, "B")
	w.WaitLoop(a, "^B:berry", 10*time.Second)
	time.Sleep(500 * time.Millisecond)
	term.DashTo("alpha")
	w.WaitLoop(a, "^A:alpha", 8*time.Second)
}

// V08: keys: towerd takes M-o and prefix L where free and leaves the
// user's; in a plain terminal prefix L is tmux's own; in a tower terminal
// it asks the home for the previous target across hosts; stopping towerd
// puts the keys back.
func TestV08(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "v08")
	a := w.Host("A", []string{"alpha"})
	k := w.Host("K", []string{"k1", "k2"}, SSHHost())
	u := w.Host("U", []string{"u1"}, SSHHost())
	k.MustTmux("unbind", "-n", "M-o")
	u.MustTmux("bind", "L", "display-message", "the user's L")
	u.MustTmux("set-hook", "-g", "alert-bell", "display-message the-users-bell")
	u.MustTmux("set-hook", "-g", "alert-silence[7193]", "display-message the-users-silence")
	w.Home(a, k.Remote(), u.Remote())
	w.WaitUp(a, "K", "U")
	key := func(h *Host, table, key string) string {
		for _, l := range strings.Split(h.MustTmux("list-keys", "-T", table), "\n") {
			f := strings.Fields(l)
			if i := slices.Index(f, "-T"); i >= 0 && i+2 < len(f) && f[i+1] == table && f[i+2] == key {
				return l
			}
		}
		return ""
	}
	hook := func(h *Host, name string) string { out, _ := h.Tmux("show-hooks", "-g", name); return out }
	w.Eventually(3*time.Second, "K's keys", func() bool {
		return strings.Contains(key(k, "root", "M-o"), "TOWER_CLIENT") && strings.Contains(key(k, "prefix", "L"), "last")
	})
	if !strings.Contains(key(u, "prefix", "L"), "the user's L") {
		t.Fatalf("U's L: %q", key(u, "prefix", "L"))
	}
	if !strings.Contains(hook(k, "alert-bell"), "alert-bell[7193]") || !strings.Contains(hook(k, "alert-bell"), "tower-alert") {
		t.Fatalf("K's alert hook: %q", hook(k, "alert-bell"))
	}
	if h := hook(u, "alert-bell"); !strings.Contains(h, "the-users-bell") || !strings.Contains(h, "tower-alert") {
		t.Fatalf("U's alert-bell: %q", h)
	}
	if h := hook(u, "alert-silence"); !strings.Contains(h, "alert-silence[7193] display-message the-users-silence") {
		t.Fatalf("U's alert-silence: %q", h)
	}

	// A plain terminal: prefix L is tmux's own switch-client -l.
	plain := w.Term("plain", k, nil, tmux.Bin(), "-L", k.Sock, "attach", "-t", "k1")
	time.Sleep(500 * time.Millisecond)
	ids := k.ClientIDs("k1")
	if len(ids) != 1 {
		t.Fatalf("clients on k1: %v", ids)
	}
	k.MustTmux("switch-client", "-c", ClientName(ids[0]), "-t", "k2")
	time.Sleep(200 * time.Millisecond)
	plain.Keys("C-b", "L")
	w.Eventually(3*time.Second, "back on k1", func() bool { return len(k.ClientIDs("k1")) == 1 })
	plain.Keys("C-b", "d")

	// A tower terminal: prefix L on A goes back to K:k1, through the home.
	lt := w.LoopTo("t", a, nil, "k1", "^K:k1")
	lt.DashTo("alpha")
	w.WaitLoop(a, "^A:alpha", 8*time.Second)
	time.Sleep(400 * time.Millisecond)
	lt.Keys("C-b", "L")
	w.WaitLoop(a, "^K:k1", 8*time.Second)

	if out, err := a.Tower("host", "off", "K"); err != nil {
		t.Fatal(err, out)
	}
	w.Eventually(3*time.Second, "K has no home", func() bool { return len(k.LiveHomes()) == 0 })
	if err := k.Call(proto.CallStop, nil, nil); err != nil {
		t.Fatal(err)
	}
	w.Eventually(5*time.Second, "K's keys back", func() bool {
		return key(k, "root", "M-o") == "" && strings.HasSuffix(strings.TrimSpace(key(k, "prefix", "L")), "switch-client -l")
	})
	if h := hook(k, "alert-bell") + hook(k, "alert-activity"); strings.Contains(h, "[7193]") {
		t.Fatalf("K's alert hooks after stop: %q", h)
	}
}

// V09: a rebuilt binary of the same version (another development build)
// replaces the running towerd, as a newer one does; an older release
// still never replaces a newer towerd.
func TestV09(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "v09")
	a := w.Host("A", []string{"alpha"})
	w.Home(a)
	ensure := func(bin string) proto.Status {
		t.Helper()
		out, err := a.TowerBin(bin, "_ensure")
		if err != nil {
			t.Fatal(err, out)
		}
		var st proto.Status
		json.Unmarshal([]byte(out), &st)
		return st
	}
	first := ensure(towerBin)
	if first.Version != Version {
		t.Fatalf("started %+v", first)
	}
	re := ensure(towerRe)
	if re.Version != VersionRebuilt || re.Pid == first.Pid {
		t.Fatalf("a rebuild did not replace the towerd: %+v (was %+v)", re, first)
	}
	// Either build replaces the other: the one just run is the one wanted.
	if back := ensure(towerBin); back.Version != Version || back.Pid == re.Pid {
		t.Fatalf("the first build back: %+v", back)
	}
	up := ensure(tower2)
	if up.Version != Version2 {
		t.Fatalf("an upgrade: %+v", up)
	}
	if old := ensure(towerRe); old.Version != Version2 || old.Pid != up.Pid {
		t.Fatalf("an older build replaced a newer towerd: %+v", old)
	}
}
