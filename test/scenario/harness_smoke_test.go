package scenario

import (
	"strings"
	"testing"
	"time"
)

// TestHarness checks the harness itself: hosts are separate tmux servers,
// ssh runs commands in a host's environment, terminals on either host
// run and report their command's exit, and teardown leaves nothing
// behind.
func TestHarness(t *testing.T) {
	w := NewWorld(t, "h0")
	a := w.Host("A", []string{"alpha", "apple"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	if got := strings.Join(a.Sessions(), ","); got != "alpha,apple" {
		t.Fatalf("A has %s", got)
	}
	if got := strings.Join(b.Sessions(), ","); got != "bravo" {
		t.Fatalf("B has %s", got)
	}
	if c := w.ssh(a, "B", `tmux $TOWER_TMUX list-sessions -F '#{session_name}'; echo $TOWER_MACHINE_ID`); c.code != 0 || c.stdout.String() != "bravo\nB\n" {
		t.Fatalf("ssh to B: exit %d, %q %q", c.code, c.stdout.String(), c.stderr.String())
	}
	term := w.Term("x", a, nil, "/bin/sh", "-c", "echo hello-$TOWER_MACHINE_ID; exit 3")
	term.Wait(`hello-A`, 5*time.Second)
	term.Wait(`LOOP-EXIT=3`, 5*time.Second)
	term = w.Term("y", b, nil, "/bin/sh", "-c", "echo hello-$TOWER_MACHINE_ID; exit 4")
	term.Wait(`hello-B`, 5*time.Second)
	term.Wait(`LOOP-EXIT=4`, 5*time.Second)
	if a.Paths().RunDir == b.Paths().RunDir {
		t.Fatal("two machines share a run dir")
	}
}
