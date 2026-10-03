package scenario

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestHarness checks the harness itself: hosts are separate tmux servers,
// the fake ssh runs commands in a host's environment, terminals run and
// report their command's exit, and teardown leaves nothing behind.
func TestHarness(t *testing.T) {
	w := NewWorld(t, "h0")
	a := w.Host("A", []string{"alpha", "apple"})
	b := w.Host("B", []string{"bravo"})
	if got := strings.Join(a.Sessions(), ","); got != "alpha,apple" {
		t.Fatalf("A has %s", got)
	}
	if got := strings.Join(b.Sessions(), ","); got != "bravo" {
		t.Fatalf("B has %s", got)
	}
	cmd := exec.Command(fakeSSH, "-T", "-o", "BatchMode=yes", "B", "--", `tmux $TOWER_TMUX list-sessions -F '#{session_name}'; echo $TOWER_MACHINE_ID`)
	cmd.Env = a.Env()
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "bravo\nB\n" {
		t.Fatalf("fake ssh to B: %q %v", out, err)
	}
	term := w.Term("x", a, nil, "/bin/sh", "-c", "echo hello-$TOWER_MACHINE_ID; exit 3")
	term.Wait(`hello-A`, 5*time.Second)
	term.Wait(`LOOP-EXIT=3`, 5*time.Second)
	if a.Paths().RunDir == b.Paths().RunDir {
		t.Fatal("two machines share a run dir")
	}
}
