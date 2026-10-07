package scenario

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// S02: two homes for one host; one crashes, one exits. One stream per
// home; the crashed home is dropped at once, its _tower goes and its
// server exits with its last session; B's towerd then idles out.
func TestS02(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "s02")
	a1 := w.Host("A1", []string{"one"})
	a2 := w.Host("A2", []string{"two"})
	b := w.Host("B", []string{"bravo"}, Env("TOWER_IDLE", "1500"), SSHHost())
	w.Home(a1, b.Remote())
	w.WaitUp(a1, "B")
	w.Home(a2, b.Remote())
	w.WaitUp(a2, "B")
	w.Eventually(3*time.Second, "two homes on B", func() bool { return len(b.LiveHomes()) == 2 })

	ids := []string{}
	for _, r := range b.LiveHomes() {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	want := []string{a1.Status().ID, a2.Status().ID}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("B's homes are %v, want %v", ids, want)
	}
	if n := b.ControlClients(); n != 1 {
		t.Fatalf("B has %d control clients", n)
	}

	pid := a1.TowerdPid()
	crash := time.Now()
	Kill9(pid)
	w.Eventually(8*time.Second, "B drops the crashed home", func() bool { return len(b.LiveHomes()) == 1 })
	t.Logf("B dropped the crashed home after %v", time.Since(crash).Round(time.Millisecond))
	w.Eventually(4*time.Second, "A1's _tower gone", func() bool { return len(a1.HiddenSessions()) == 0 })
	t.Logf("A1's _tower gone after %v", time.Since(crash).Round(time.Millisecond))

	a1.MustTmux("kill-session", "-t", "one")
	end := time.Now()
	w.Eventually(3*time.Second, "A1's server exits", func() bool { return !a1.HasServer() })
	t.Logf("A1's server exited %v after its last session", time.Since(end).Round(time.Millisecond))
	w.Eventually(2*time.Second, "no tmux process left for A1", func() bool {
		return exec.Command("pgrep", "-f", "tmux -L "+a1.Sock+" ").Run() != nil
	})

	pid2 := a2.TowerdPid()
	if err := a2.Call(proto.CallStop, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !w.WaitGone(pid2, 5*time.Second) {
		t.Fatal("A2's towerd did not stop")
	}
	w.Eventually(3*time.Second, "B has no home", func() bool { return len(b.LiveHomes()) == 0 })
	bpid := b.TowerdPid()
	if !w.WaitGone(bpid, 8*time.Second) {
		t.Fatal("B's towerd did not idle out")
	}
	if h := b.HiddenSessions(); len(h) != 0 {
		t.Fatalf("B still has %v", h)
	}
	if h := a2.HiddenSessions(); len(h) != 0 {
		t.Fatalf("A2 still has %v", h)
	}
}

// S03: one server under two aliases, and another user on the same
// machine: one alias linked, the other a duplicate; one control client
// and one stream on B.
func TestS03(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "s03")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	broot := w.Host("Broot", []string{"rootwork"}, Machine("B"), HomeName("Broot"), SSHHost())
	w.SSH("b", b)
	w.SSH("b-lan", b)
	w.SSH("b-root", broot)
	w.Home(a,
		config.Host{Name: "b", SSH: "b", Tmux: "-L " + b.Sock, Tower: towerBin},
		config.Host{Name: "b-lan", SSH: "b-lan", Tmux: "-L " + b.Sock, Tower: towerBin},
		config.Host{Name: "b-root", SSH: "b-root", Tmux: "-L " + broot.Sock, Tower: towerBin},
	)
	w.WaitLink(a, "b-root", "up", 6*time.Second)
	var primary, dup proto.LinkStatus
	w.Eventually(6*time.Second, "one alias up, the other a duplicate", func() bool {
		x, y := w.Link(a, "b"), w.Link(a, "b-lan")
		switch {
		case x.Status == "up" && y.Status == "dup":
			primary, dup = x, y
		case y.Status == "up" && x.Status == "dup":
			primary, dup = y, x
		default:
			return false
		}
		return true
	})
	t.Logf("duplicate %s: %s", dup.Name, dup.Reason)
	if !strings.HasPrefix(dup.Reason, "dup: same towerd as "+primary.Name) {
		t.Fatalf("duplicate's reason: %q", dup.Reason)
	}
	// Another user has another machine key (it hashes the uid) and so
	// other run and state directories; the suite runs as one uid, so
	// here the separate TOWER_HOME stands in and the towerd id tells the
	// two servers apart.
	root := w.Link(a, "b-root")
	if primary.ID == root.ID {
		t.Fatalf("B and Broot share towerd id %s", root.ID)
	}
	if n := b.ControlClients(); n != 1 {
		t.Fatalf("B has %d control clients", n)
	}
	w.Eventually(3*time.Second, "one stream on B", func() bool { return len(b.LiveHomes()) == 1 })
	v := a.View("")
	n := 0
	for _, h := range v.View.Hosts {
		for _, s := range h.Sessions {
			if s.Name == "bravo" {
				n++
			}
		}
	}
	if n != 1 {
		t.Fatalf("bravo is listed %d times", n)
	}
}

// S05: three servers on one machine: one towerd each, three ids, three
// sockets in one run dir. The machine is the home's, so two remotes share
// it with the home's own server; on the container backend, where only a
// container is reached over ssh, it is B's.
func TestS05(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "s05")
	a := w.Host("A", []string{"alpha"})
	m, remotes := a, []config.Host{}
	if ctrs != nil {
		m = w.Host("B", []string{"bravo"}, SSHHost())
		remotes = append(remotes, m.Remote())
	}
	wk := w.Host("W", []string{"work1"}, Machine(m.Machine), SSHHost())
	p := w.Host("P", []string{"play1"}, Machine(m.Machine), BaseIndex(0), SSHHost())
	w.Home(a, append(remotes, wk.Remote(), p.Remote())...)
	w.WaitUp(a, m.Name, "W", "P")
	ids := map[string]bool{w.Link(a, m.Name).ID: true, w.Link(a, "W").ID: true, w.Link(a, "P").ID: true}
	if len(ids) != 3 {
		t.Fatalf("ids: %v", ids)
	}
	if wk.Paths().RunDir != p.Paths().RunDir || wk.Paths().RunDir != m.Paths().RunDir {
		t.Fatalf("run dirs differ: %s %s %s", m.Paths().RunDir, wk.Paths().RunDir, p.Paths().RunDir)
	}
	if wk.Paths().Socket() == p.Paths().Socket() || wk.Paths().Socket() == m.Paths().Socket() || p.Paths().Socket() == m.Paths().Socket() {
		t.Fatal("one socket for two servers")
	}
	v := a.View("")
	if want := 3 + len(remotes); len(v.View.Hosts) != want {
		t.Fatalf("%d hosts in the view, want %d", len(v.View.Hosts), want)
	}
	for _, h := range v.View.Hosts {
		for _, s := range h.Sessions {
			if strings.Contains(s.Name, "term") || strings.HasPrefix(s.Name, "_tower") {
				t.Fatalf("the view lists %s:%s", h.Name, s.Name)
			}
		}
	}
	// A hand-off between two servers of the machine.
	term := w.LoopTo("t", a, nil, "work1", "^W:work1")
	term.DashTo("play1")
	w.WaitLoop(a, "^P:play1", 8*time.Second)
}
