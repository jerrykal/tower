package scenario

import (
	"os"
	"strings"
	"testing"
	"time"
)

// E01: a second towerd for one server is refused.
func TestE01(t *testing.T) {
	w := NewWorld(t, "e01")
	a := w.Host("A", []string{"alpha"})
	w.Home(a)
	w.Eventually(3*time.Second, "A's control client", func() bool { return a.ControlClients() == 1 })
	out, err := a.Tower("towerd")
	if err == nil || !strings.Contains(out, "already running") {
		t.Fatalf("a second towerd: %v %q", err, out)
	}
	if n := a.ControlClients(); n != 1 {
		t.Fatalf("A has %d control clients", n)
	}
}

// E02: a home with no loops idles out, then its remote; nothing is left
// in tmux.
func TestE02(t *testing.T) {
	w := NewWorld(t, "e02")
	w.Timing(map[string]string{"TOWER_IDLE": "1500"})
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"})
	w.Home(a, b.Remote())
	w.WaitLink(a, "B", "up", 5*time.Second)
	apid, bpid := a.TowerdPid(), b.TowerdPid()
	if !w.WaitGone(apid, 6*time.Second) {
		t.Fatal("the home did not idle out")
	}
	if !w.WaitGone(bpid, 6*time.Second) {
		t.Fatal("the remote did not idle out")
	}
	if h := a.HiddenSessions(); len(h) != 0 {
		t.Fatalf("A still has %v", h)
	}
	if h := b.HiddenSessions(); len(h) != 0 {
		t.Fatalf("B still has %v", h)
	}
}

// E03: the last session ends while the home's stream is half-open: the
// server exits at once, and tmux there works as before.
func TestE03(t *testing.T) {
	w := NewWorld(t, "e03")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"})
	w.Home(a, b.Remote())
	w.WaitUp(a, "B")
	w.Freeze("B", true)
	w.WaitLink(a, "B", "down|connecting", 8*time.Second)
	b.MustTmux("kill-session", "-t", "bravo")
	start := time.Now()
	w.Eventually(5*time.Second, "B's server exits", func() bool { return !b.HasServer() })
	t.Logf("B's server exited after %v", time.Since(start).Round(time.Millisecond))
	b.NewSession("fresh")
	w.Reset("B")
	w.WaitLink(a, "B", "up", 8*time.Second)
}

// E04: tower host add / off / on / rm: the checks in order (ssh, tmux,
// OS, tower); the stream closed on off and rm.
func TestE04(t *testing.T) {
	w := NewWorld(t, "e04")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"})
	w.Home(a)
	out, err := a.Tower("host", "add", "B", "--name", "bee", "--tmux", "-L "+b.Sock, "--tower", towerBin)
	t.Logf("host add:\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
	last := -1
	for _, step := range []string{"✓ ssh", "✓ tmux", "✓ os", "✓ tower"} {
		i := strings.Index(out, step)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order", step)
		}
		last = i
	}
	w.WaitLink(a, "bee", "up", 6*time.Second)
	if out, err := a.Tower("host", "add", "B", "--name", "BEE"); err == nil || !strings.Contains(out, `a host named "BEE" already exists`) {
		t.Fatalf("a taken name: %v %q", err, out)
	}
	if out, err := a.Tower("host", "off", "bee"); err != nil {
		t.Fatal(err, out)
	}
	w.WaitLink(a, "bee", "off", 5*time.Second)
	w.Eventually(3*time.Second, "B has no home", func() bool { return len(b.LiveHomes()) == 0 })
	if out, err := a.Tower("host", "on", "bee"); err != nil {
		t.Fatal(err, out)
	}
	w.WaitLink(a, "bee", "up", 6*time.Second)
	if out, err := a.Tower("host", "rm", "bee"); err != nil {
		t.Fatal(err, out)
	}
	w.WaitLink(a, "bee", "unknown", 5*time.Second)
	w.Eventually(3*time.Second, "B has no home", func() bool { return len(b.LiveHomes()) == 0 })
	if !strings.Contains(strings.Join(b.Sessions(), ","), "bravo") {
		t.Fatal("B:bravo is gone")
	}
}

// V01: 8 concurrent first calls start one towerd; a SIGKILL leaves a
// stale socket the next call replaces; stop leaves nothing.
func TestV01(t *testing.T) {
	w := NewWorld(t, "v01")
	a := w.Host("A", []string{"alpha"})
	start := time.Now()
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			out, err := a.Tower("_ensure")
			if err != nil {
				err = &cmdErr{err, out}
			}
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("8 concurrent ensures: %v", time.Since(start).Round(time.Millisecond))
	time.Sleep(300 * time.Millisecond)
	procs := a.TowerdProcs()
	if len(procs) != 1 {
		t.Fatalf("%d towerds: %v", len(procs), procs)
	}
	old := procs[0]
	Kill9(old)
	time.Sleep(200 * time.Millisecond)
	if !fileExists(a.Paths().Socket()) {
		t.Fatal("the dead towerd's socket is gone")
	}
	if a.Status() != nil {
		t.Fatal("a dead towerd answered")
	}
	start = time.Now()
	if out, err := a.Tower("_ensure"); err != nil {
		t.Fatal(err, out)
	}
	t.Logf("ensure after a crash: %v", time.Since(start).Round(time.Millisecond))
	if pid := a.TowerdPid(); pid == 0 || pid == old {
		t.Fatalf("towerd pid %d after a crash of %d", pid, old)
	}
	w.Eventually(3*time.Second, "one control client on A", func() bool { return a.ControlClients() == 1 })
	if out, err := a.Tower("stop"); err != nil {
		t.Fatal(err, out)
	}
	w.Eventually(3*time.Second, "nothing left", func() bool {
		return !fileExists(a.Paths().Socket()) && len(a.TowerdProcs()) == 0
	})
	if h := a.HiddenSessions(); len(h) != 0 {
		t.Fatalf("A still has %v", h)
	}
}

type cmdErr struct {
	err error
	out string
}

func (e *cmdErr) Error() string { return e.err.Error() + ": " + e.out }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
