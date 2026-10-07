package scenario

import (
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// sessionCalls counts the session calls (no -O) to host in the ssh log.
func (w *World) sessionCalls(host string) int {
	n := 0
	for _, c := range w.SSHLog() {
		if c["host"] == host && c["op"] == "" {
			n++
		}
	}
	return n
}

// S14: a tunnel flapping 0.6s down, 0.6s up for 12s: few ssh calls
// (backoff), up once steady, the local server's watch untouched.
func TestS14(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "s14")
	a := w.Host("A", []string{"alpha"})
	tn := w.Host("T", []string{"tun"}, SSHHost())
	w.Home(a, tn.Remote())
	w.WaitUp(a, "T")
	before := w.sessionCalls("T")
	ctl := a.Status().Detail.Watch.CtlPid
	end := time.Now().Add(12 * time.Second)
	for time.Now().Before(end) {
		w.Down("T", "refused")
		w.Drop("T")
		time.Sleep(600 * time.Millisecond)
		w.Reset("T")
		time.Sleep(600 * time.Millisecond)
	}
	w.Reset("T")
	calls := w.sessionCalls("T") - before
	t.Logf("%d ssh calls in 12s of flapping", calls)
	if calls > 30 {
		t.Fatalf("%d ssh calls: no backoff", calls)
	}
	w.WaitLink(a, "T", "up", 6*time.Second)
	if got := a.Status().Detail.Watch.CtlPid; got != ctl {
		t.Fatalf("A's control client changed: %d → %d", ctl, got)
	}
}

// S15: ssh would prompt or hang: each host down with its reason and fix,
// fast; every call in batch mode; tower host add says the same. Real
// ssh has no Tailscale check to meet (transport's tests read it).
func TestS15(t *testing.T) {
	w := NewWorld(t, "s15")
	a := w.Host("A", []string{"alpha"})
	x := w.Host("X", nil, SSHHost())
	downs := map[string]string{"hk": "hostkey", "pw": "password", "ak": "auth", "to": "timeout", "rs": "resolve", "pw2": "password"}
	want := map[string]string{
		"hk": "host key", "pw": "authentication failed", "ak": "authentication failed",
		"to": "timed out", "rs": "resolve",
	}
	adds := map[string]string{"pw2": "ssh-add"}
	for name, how := range downs {
		w.SSH(name, x)
		w.Down(name, how)
	}
	var hosts []config.Host
	for _, n := range slices.Sorted(maps.Keys(want)) {
		hosts = append(hosts, config.Host{Name: n, SSH: n, Tower: towerBin})
	}
	start := time.Now()
	w.Home(a, hosts...)
	took := map[string]time.Duration{}
	w.Eventually(10*time.Second, "every host down", func() bool {
		for n := range want {
			if _, ok := took[n]; ok {
				continue
			}
			if l := w.Link(a, n); l.Status == "down" {
				took[n] = time.Since(start)
				if !strings.Contains(l.Reason, want[n]) {
					t.Fatalf("%s is down with %q, want %q", n, l.Reason, want[n])
				}
				if !strings.Contains(l.Reason, "ssh") && n != "to" && n != "rs" {
					t.Fatalf("%s's reason has no fix: %q", n, l.Reason)
				}
			}
		}
		return len(took) == len(want)
	})
	t.Logf("down after %v", took)
	if took["ts"] > 3*time.Second {
		t.Fatalf("the Tailscale check took %v", took["ts"])
	}
	for _, c := range w.SSHLog() {
		if c["op"] == "" && c["batch"] != "yes" {
			t.Fatalf("an ssh call without BatchMode: %v", c)
		}
	}
	for host, want := range adds {
		start := time.Now()
		out, _ := a.Tower("host", "add", host, "--tower", towerBin)
		el := time.Since(start)
		t.Logf("host add %s (%v):\n%s", host, el.Round(time.Millisecond), out)
		if el > 3*time.Second || !strings.Contains(out, want) {
			t.Fatalf("host add %s: %v, %q", host, el, out)
		}
	}
}

// S16: socket paths fit 104 bytes; one control path value on every call;
// a stale control socket is removed, a busy one kept; no ssh -O check.
func TestS16(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "s16")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Home(a, b.Remote())
	w.WaitUp(a, "B")
	p := a.Paths()
	t.Logf("state %s (%d), run dir %s", p.StateRoot, len(p.StateRoot), p.RunDir)
	if n := len(p.CMDir()) + 1 + 40 + 17; n > 103 {
		t.Fatalf("a control path takes %d bytes", n)
	}
	if n := len(p.Socket()); n > 103 {
		t.Fatalf("the towerd socket path takes %d bytes", n)
	}
	cp := filepath.Join(p.CMDir(), "%C")
	for _, c := range w.SSHLog() {
		if v, _ := c["cp"].(string); v != "" && v != cp {
			t.Fatalf("control path %q, want %q", v, cp)
		}
	}
	stale := filepath.Join(p.CMDir(), "0123456789abcdef0123456789abcdef01234567")
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("no stale socket")
	}
	busy := filepath.Join(p.CMDir(), "76543210fedcba9876543210fedcba9876543210")
	bl, err := net.Listen("unix", busy)
	if err != nil {
		t.Fatal(err)
	}
	defer bl.Close()
	gen := w.Link(a, "B").Link
	if err := a.Call(proto.CallWake, nil, nil); err != nil {
		t.Fatal(err)
	}
	w.Eventually(6*time.Second, "B up again after the wake", func() bool {
		l := w.Link(a, "B")
		return l.Status == "up" && l.Link > gen
	})
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("the stale socket is still there")
	}
	if _, err := os.Stat(busy); err != nil {
		t.Fatal("the busy socket was removed")
	}
	for _, c := range w.SSHLog() {
		if c["op"] == "check" {
			t.Fatalf("an ssh -O check: %v", c)
		}
	}
}

// S17: no server: towerd polls and never starts one; a new session there
// starts one, waiting for a slow config; _tower never keeps a server
// alive; towerd outlives its server.
func TestS17(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "s17")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	n := w.Host("N", nil, SSHHost())
	w.sockets = append(w.sockets, n.Sock)
	w.Home(a, n.Remote(), b.Remote())
	w.WaitLink(a, "N", "nosrv", 6*time.Second)
	time.Sleep(2 * time.Second)
	if n.HasServer() {
		t.Fatal("towerd started a server on N")
	}
	conf := filepath.Join(w.UserHome, ".config", "tmux", "tmux.conf")
	os.MkdirAll(filepath.Dir(conf), 0o700)
	if err := os.WriteFile(conf, []byte("run-shell 'sleep 6'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// n on N in the picker's columns: the dashboard says it is starting
	// tmux there while towerd waits for the slow config.
	term := w.Loop("t", a, nil)
	term.Wait(Prompt, 6*time.Second)
	term.Keys("C-l", "1", "/")
	term.Type("N")
	time.Sleep(100 * time.Millisecond)
	term.Keys("Escape") // alone: ESC and a key at once read as alt+key
	time.Sleep(100 * time.Millisecond)
	term.Keys("l")
	term.Wait(`n makes one`, 6*time.Second)
	term.Keys("n")
	time.Sleep(300 * time.Millisecond)
	term.Type("fresh")
	start := time.Now()
	term.Keys("Enter")
	term.Wait("starting tmux on N…", 3*time.Second)
	w.Eventually(12*time.Second, "N:fresh", func() bool { return slices.Contains(n.Sessions(), "fresh") })
	term.Wait("new on N: done", 6*time.Second)
	t.Logf("new on N, with a 6s config: done after %v", time.Since(start).Round(time.Millisecond))
	if !HasSession(&a.View("").View, "N", "fresh") {
		t.Fatal("the answer came before the view had the new session")
	}
	os.Remove(conf)
	w.WaitLink(a, "N", "up", 6*time.Second)

	b.MustTmux("kill-session", "-t", "bravo")
	w.Eventually(5*time.Second, "B's server exits", func() bool { return !b.HasServer() })
	w.WaitLink(a, "B", "nosrv", 5*time.Second)
	if b.Status() == nil {
		t.Fatal("B's towerd went with its server")
	}
}

// S18: a host without the binary, one speaking protocol 0 only, newer
// peers (1–2) and one sending unknown messages: each its outcome, and a
// hand-off to the newer peer works.
func TestS18(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "s18")
	a := w.Host("A", []string{"alpha"})
	m := w.Host("M", []string{"mike"}, SSHHost())
	o := w.Host("O", []string{"oscar"}, Env("TOWER_TEST_PROTO", "0-0"), SSHHost())
	nv := w.Host("N", []string{"november"}, Env("TOWER_TEST_PROTO", "1-2"), SSHHost())
	f := w.Host("F", []string{"foxtrot"}, Env("TOWER_TEST_PROTO", "1-2"), Env("TOWER_TEST_FUTURE", "1"), SSHHost())
	mr := m.Remote()
	mr.Tower = "/nonexistent/tower"
	w.Home(a, mr, o.Remote(), nv.Remote(), f.Remote())
	w.WaitLink(a, "M", "failed", 6*time.Second)
	if l := w.Link(a, "M"); !strings.Contains(l.Reason, "not installed") {
		t.Fatalf("M: %q", l.Reason)
	}
	w.WaitLink(a, "O", "failed", 6*time.Second)
	if l := w.Link(a, "O"); !strings.Contains(l.Reason, "incompatible protocol") {
		t.Fatalf("O: %q", l.Reason)
	} else {
		t.Logf("O: %s", l.Reason)
	}
	w.WaitLink(a, "N", "up", 6*time.Second)
	if l := w.Link(a, "N"); l.Proto != 1 {
		t.Fatalf("N speaks protocol %d", l.Proto)
	}
	w.WaitLink(a, "F", "up", 6*time.Second)
	w.Eventually(3*time.Second, "the home logging the unknown message type", func() bool {
		logb, _ := os.ReadFile(a.Paths().State("towerd.log"))
		return strings.Contains(string(logb), `ignoring unknown message type "future-thing"`)
	})
	// A hand-off from N to the newer peer F, towerd's side: the dashboard
	// on N stores the switch through the home, the loop is woken, holds,
	// and after its client ends goes to F.
	l := w.FakeLoop(a)
	l.Attach(w.Ref(a, "N", "november"), nv)
	woke := l.WaitSwitch()
	time.Sleep(100 * time.Millisecond)
	cl := nv.ClientIDs("november")
	if len(cl) != 1 {
		t.Fatalf("clients on november: %v", cl)
	}
	ackC := make(chan *proto.Ack, 1)
	go func() {
		ack, _ := nv.Act(cl[0], proto.Request{Op: proto.OpSwitch, Target: w.Ref(a, "F", "foxtrot"), Nonce: "n1"}, 0)
		ackC <- ack
	}()
	if !<-woke {
		t.Fatal("wait-switch did not wake")
	}
	if !l.Held() {
		t.Fatal("held: the loop was not told to end the client")
	}
	ack := <-ackC
	if !ack.OK || !ack.Ended {
		t.Fatalf("switch: %+v", ack)
	}
	next := l.After(42, true)
	if next.Do != proto.NextHandoff || next.Target.Name != "F" {
		t.Fatalf("after: %+v", next)
	}
	l.Attach(next.Target, f)
	w.WaitLoop(a, "^F:foxtrot", 8*time.Second)
	w.Eventually(6*time.Second, "M retried at the cap", func() bool { return w.Link(a, "M").Attempts <= 4 })
}
