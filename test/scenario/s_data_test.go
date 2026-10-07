package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/tmux"
)

// S13: sleep (every connection half-open), then back: detected within
// seconds, back fast; B replaces the stale stream (never two); B's control
// client unaffected.
func TestS13(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "s13")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Home(a, b.Remote())
	w.WaitUp(a, "B")
	w.LoopTo("t", a, nil, "bravo", "^B:bravo")
	attempts := w.Link(a, "B").Attempts
	ctl := b.Status().Detail.Watch.CtlPid

	w.Freeze("B", true)
	el := w.WaitLink(a, "B", "down|connecting", 8*time.Second)
	t.Logf("half-open link given up after %v", el.Round(time.Millisecond))
	if n := len(b.LiveHomes()); n != 1 {
		t.Fatalf("B has %d streams while half-open", n)
	}
	if n := b.ControlClients(); n != 1 {
		t.Fatalf("B has %d control clients", n)
	}
	w.Drop("B")
	time.Sleep(400 * time.Millisecond)
	w.Reset("B")
	el = w.WaitLink(a, "B", "up", 8*time.Second)
	t.Logf("back up %v after the network came back", el.Round(time.Millisecond))
	w.Eventually(3*time.Second, "one live stream on B", func() bool { return len(b.LiveHomes()) == 1 })
	if got := w.Link(a, "B").Attempts - attempts; got > 4 {
		t.Fatalf("%d connects for one sleep", got)
	}
	if got := b.Status().Detail.Watch.CtlPid; got != ctl {
		t.Fatalf("B's control client changed: %d → %d", ctl, got)
	}
	// The attach's ssh ended with the drop: the loop reconnects to where
	// it was.
	w.Eventually(10*time.Second, "B's client on bravo", func() bool { return slices.Equal(b.Clients(), []string{"bravo"}) })
	gen := w.Link(a, "B").Link
	if err := a.Call(proto.CallWake, nil, nil); err != nil {
		t.Fatal(err)
	}
	w.Eventually(5*time.Second, "B up after a wake", func() bool {
		l := w.Link(a, "B")
		return l.Status == "up" && l.Link > gen
	})
	w.Eventually(2*time.Second, "one stream on B", func() bool { return len(b.LiveHomes()) == 1 })
}

// S20: 300 windows, states and views padded to 2 MB: a new session on B
// reaches the view held on C fast; C's dashboard reads its rows fast.
func TestS20(t *testing.T) {
	w := NewWorld(t, "s20")
	pad := Env("TOWER_TEST_PAD", "2000000")
	a := w.Host("A", []string{"alpha"}, pad)
	b := w.Host("B", []string{"bravo"}, pad, SSHHost())
	c := w.Host("C", []string{"charlie"}, pad, SSHHost())
	var script strings.Builder
	for i := range 60 {
		fmt.Fprintf(&script, "new-session -d -s s%03d 'exec sleep 600'\n", i)
		for j := 1; j <= 4; j++ {
			fmt.Fprintf(&script, "new-window -d -t s%03d: -n a-longer-window-name-%d 'exec sleep 600'\n", i, j)
		}
	}
	src := filepath.Join(w.Dir, "s20.tmux")
	os.WriteFile(src, []byte(script.String()), 0o600)
	b.MustTmux("source-file", src)
	w.Home(a, b.Remote(), c.Remote())
	w.WaitLink(a, "B", "up", 10*time.Second)
	w.WaitLink(a, "C", "up", 10*time.Second)
	w.Eventually(10*time.Second, "C holds B:s059", func() bool { return HasSession(&c.View("0:0:none").View, "B", "s059") })

	start := time.Now()
	b.NewSession("probe-1")
	w.Eventually(10*time.Second, "B:probe-1 on C", func() bool { return HasSession(&c.View("0:0:none").View, "B", "probe-1") })
	prop := time.Since(start)
	start = time.Now()
	v := c.View("0:0:none")
	rows := time.Since(start)
	st := w.Link(a, "B")
	t.Logf("propagation B → C %v, rows on C %v (%d hosts); stream B→home %.1f MB, home→B %.1f MB", prop.Round(time.Millisecond), rows.Round(time.Microsecond), len(v.View.Hosts), float64(st.Rx)/1e6, float64(st.Tx)/1e6)
	if prop > 3*time.Second {
		t.Fatalf("propagation took %v", prop)
	}
	if rows > time.Second {
		t.Fatalf("rows took %v", rows)
	}
}

// S21: a remote's clock an hour ahead: the offset measured, ages right,
// and a request with a 1.5s deadline runs there.
func TestS21(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "s21")
	a := w.Host("A", []string{"aold"})
	b := w.Host("B", []string{"bnew", "victim"}, Env("TOWER_TEST_SKEW", "3600000"), SSHHost())
	w.Home(a, b.Remote())
	w.WaitUp(a, "B")
	l := w.FakeLoop(a)
	l.Attach(w.Ref(a, "A", "aold"), a)
	time.Sleep(2 * time.Second)
	l.Attach(w.Ref(a, "B", "bnew"), b)
	w.WaitLoop(a, "^B:bnew", 5*time.Second)
	time.Sleep(500 * time.Millisecond)
	off := w.Link(a, "B").Offset
	t.Logf("B's clock offset %dms", off)
	if off <= 3_599_000 || off >= 3_601_000 {
		t.Fatalf("offset %d", off)
	}
	v := a.View("").View
	age := func(host, name string) int64 {
		for _, s := range HostIn(&v, host).Sessions {
			if s.Name == name {
				return s.Ago
			}
		}
		t.Fatalf("no %s:%s", host, name)
		return 0
	}
	if ag := age("B", "bnew"); ag < 0 || ag > 2000 {
		t.Fatalf("B:bnew is %dms old", ag)
	}
	if ag := age("A", "aold"); ag < 2000 {
		t.Fatalf("A:aold is %dms old", ag)
	}
	// loop part: the dashboard sorts B first (attached last).
	victim := w.Ref(a, "B", "victim")
	ack, el := a.Act("", proto.Request{Op: proto.OpKill, Target: victim}, 1500)
	t.Logf("kill with a 1.5s deadline on a host an hour ahead: %+v in %v", ack, el)
	if !ack.OK {
		t.Fatal(ack.Err)
	}
	if strings.Contains(strings.Join(b.Sessions(), ","), "victim") {
		t.Fatal("victim still there")
	}
}

// S22: control mode's side effects: session_last_attached untouched,
// attached 0, window sizes unaffected, little traffic while a pane prints
// megabytes, 400 concurrent commands matched, _tower bounced and ended
// with the last session.
func TestS22(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "s22")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo", "backup"}, SSHHost())
	before := b.MustTmux("display-message", "-p", "-t", "bravo", "#{session_last_attached}")
	w.Home(a, b.Remote())
	w.WaitUp(a, "B")
	if after := b.MustTmux("display-message", "-p", "-t", "bravo", "#{session_last_attached}"); after != before {
		t.Fatalf("session_last_attached moved: %q → %q", before, after)
	}
	if n := strings.TrimSpace(b.MustTmux("display-message", "-p", "-t", "bravo", "#{session_attached}")); n != "0" {
		t.Fatalf("bravo attached %s", n)
	}
	l := w.FakeLoop(a)
	term := l.Attach(w.Ref(a, "B", "bravo"), b)
	w.Eventually(3*time.Second, "bravo's window the terminal's width", func() bool {
		return strings.HasPrefix(b.MustTmux("display-message", "-p", "-t", "bravo", "#{window_width}x#{window_height}"), "110x")
	})
	w.Eventually(3*time.Second, "the view counts bravo's client", func() bool {
		for _, s := range HostIn(&a.View("").View, "B").Sessions {
			if s.Name == "bravo" {
				return s.Attached == 1
			}
		}
		return false
	})
	ctl0 := b.Status().Detail.Watch.CtlBytes
	rx0 := w.Link(a, "B").Rx
	term.Type("yes | head -c 20000000 > /dev/null; yes 'flood flood flood' | head -n 200000")
	term.Keys("Enter")
	time.Sleep(4 * time.Second)
	ctl1 := b.Status().Detail.Watch.CtlBytes
	rx1 := w.Link(a, "B").Rx
	t.Logf("while bravo printed ~3.6 MB: %d bytes to towerd's control client, %d on the stream", ctl1-ctl0, rx1-rx0)
	if ctl1-ctl0 >= 20000 || rx1-rx0 >= 20000 {
		t.Fatalf("traffic: control client %d, stream %d", ctl1-ctl0, rx1-rx0)
	}

	// 400 concurrent commands on a control client, each matched.
	raw, err := tmux.Attach(tmux.Server{Bin: tmux.Bin(), Args: b.TmuxArgs()}, "backup")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	bad := 0
	for i := range 400 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var r tmux.Reply
			var err error
			if i%7 == 0 {
				r, err = raw.DoTimeout(fmt.Sprintf("show -gv @missing-%d", i), 10*time.Second)
				if err != nil || !r.Err || !strings.Contains(r.Text(), fmt.Sprintf("missing-%d", i)) {
					mu.Lock()
					bad++
					mu.Unlock()
				}
				return
			}
			r, err = raw.DoTimeout(fmt.Sprintf("display -p 'n=%d'", i), 10*time.Second)
			if err != nil || r.Err || r.Text() != fmt.Sprintf("n=%d", i) {
				mu.Lock()
				bad++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	raw.Close()
	if bad != 0 {
		t.Fatalf("%d of 400 replies mismatched", bad)
	}

	// A client moved onto _tower is sent back.
	b.MustTmux("set", "-g", "detach-on-destroy", "off")
	cl := b.ClientIDs("bravo")
	if len(cl) != 1 {
		t.Fatalf("clients on bravo: %v", cl)
	}
	name := cl[0][strings.LastIndex(cl[0], ":")+1:]
	b.MustTmux("switch-client", "-c", name, "-t", "_tower:")
	w.Eventually(3*time.Second, "no client on _tower", func() bool {
		s := b.Clients()
		return len(s) == 1 && !strings.HasPrefix(s[0], "_tower")
	})
	b.MustTmux("kill-session", "-t", "backup")
	b.MustTmux("kill-session", "-t", "bravo")
	w.Eventually(5*time.Second, "B's server exits", func() bool { return !b.HasServer() })
}

// S23: kill, rename and new on C from B's dashboard, through the home:
// fast; a repeated id runs once; C down: an error, fast, never run
// later; the home frozen: a timeout, never run after it thaws; the home
// dead: refused fast.
func TestS23(t *testing.T) {
	w := NewWorld(t, "s23")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	c := w.Host("C", []string{"charlie", "victim", "ren"}, SSHHost())
	w.Home(a, b.Remote(), c.Remote())
	w.WaitUp(a, "B", "C")
	l := w.FakeLoop(a)
	l.Attach(w.Ref(a, "B", "bravo"), b)
	cl := b.ClientIDs("bravo")[0]
	cid := w.Link(a, "C").ID
	act := func(req proto.Request) (*proto.Ack, time.Duration) { return b.Act(cl, req, 1500) }
	has := func(name string) bool { return strings.Contains(","+strings.Join(c.Sessions(), ",")+",", ","+name+",") }

	ack, el := act(proto.Request{Op: proto.OpKill, Target: w.Ref(a, "C", "victim")})
	t.Logf("kill: %v", el)
	if !ack.OK || has("victim") {
		t.Fatalf("kill: %+v", ack)
	}
	ack, el = act(proto.Request{Op: proto.OpRename, Target: w.Ref(a, "C", "ren"), Name: "renamed #1"})
	t.Logf("rename: %v", el)
	if !ack.OK || !has("renamed #1") {
		t.Fatalf("rename: %+v %v", ack, c.Sessions())
	}
	ack, el = act(proto.Request{Op: proto.OpNew, Target: proto.Ref{Host: cid}, Name: "made"})
	t.Logf("new: %v", el)
	if !ack.OK || !has("made") {
		t.Fatalf("new: %+v", ack)
	}
	a1, _ := act(proto.Request{ID: "dup1", Op: proto.OpNew, Target: proto.Ref{Host: cid}, Name: "dupe"})
	a2, _ := act(proto.Request{ID: "dup1", Op: proto.OpNew, Target: proto.Ref{Host: cid}, Name: "dupe"})
	if !a1.OK || !a2.OK || a1.Ref == nil || a2.Ref == nil || a1.Ref.Session != a2.Ref.Session {
		t.Fatalf("a repeated id: %+v %+v", a1, a2)
	}
	n := 0
	for _, s := range c.Sessions() {
		if s == "dupe" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d sessions named dupe", n)
	}

	// C down: refused fast, and never run later.
	charlie := w.Ref(a, "C", "charlie")
	w.Down("C", "refused")
	w.Drop("C")
	w.WaitLink(a, "C", "down", 5*time.Second)
	ack, el = act(proto.Request{Op: proto.OpKill, Target: charlie})
	t.Logf("kill with C down: %q in %v", ack.Err, el)
	if ack.OK || el > 3*time.Second {
		t.Fatalf("kill with C down: %+v in %v", ack, el)
	}
	w.Reset("C")
	w.WaitLink(a, "C", "up", 6*time.Second)
	time.Sleep(time.Second)
	if !has("charlie") {
		t.Fatal("the failed kill ran later")
	}

	// The home frozen: a timeout, never run after it thaws.
	count := len(c.Sessions())
	hp := a.TowerdPid()
	Signal(hp, syscall.SIGSTOP)
	ack, el = act(proto.Request{Op: proto.OpNew, Target: proto.Ref{Host: cid}, Name: "late"})
	Signal(hp, syscall.SIGCONT)
	t.Logf("new with the home frozen: %q in %v", ack.Err, el)
	if ack.OK || el > 3*time.Second || !strings.Contains(ack.Err, "did not answer") {
		t.Fatalf("new with the home frozen: %+v in %v", ack, el)
	}
	time.Sleep(2 * time.Second)
	if got := len(c.Sessions()); got != count {
		t.Fatalf("C has %d sessions, had %d: the timed out request ran", got, count)
	}

	// The home dead: refused fast.
	Kill9(hp)
	w.Eventually(5*time.Second, "B drops the dead home", func() bool { return len(b.LiveHomes()) == 0 })
	ack, el = act(proto.Request{Op: proto.OpKill, Target: charlie})
	t.Logf("kill with the home dead: %q in %v", ack.Err, el)
	if ack.OK || el > time.Second || !strings.Contains(ack.Err, "not connected") {
		t.Fatalf("kill with the home dead: %+v in %v", ack, el)
	}
}
