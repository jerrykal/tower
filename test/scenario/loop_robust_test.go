package scenario

import (
	"syscall"
	"testing"
	"time"
)

// downHost makes h refuse new connections and drops its live ones: the
// home's link goes down and stays down.
func (w *World) downHost(name string) {
	w.Down(name, "refused")
	w.Drop(name)
}

// TestLoopCtrlC: ctrl-c while the loop waits between attaches gives the
// picker, never kills it: with the terminal cooked (the first attach to a
// host that is down, a SIGINT), and while reconnecting with the terminal
// raw (the ctrl-c byte). Esc then exits cleanly, the pid file gone.
func TestLoopCtrlC(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "ctlc")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	stdSetup(w, a, b)

	// Reconnecting: B lost under the attach, prepare waiting for it.
	term := w.LoopTo("r", a, nil, "bravo", "^B:bravo")
	w.downHost("B")
	w.WaitMark("after reconnect", 6*time.Second)
	time.Sleep(time.Second)
	start := time.Now()
	term.Keys("C-c")
	term.Wait(Prompt, 3*time.Second)
	t.Logf("reconnecting: ctrl-c → the picker in %v", time.Since(start).Round(time.Millisecond))
	term.Keys("Escape")
	term.Wait(`LOOP-EXIT=0`, 5*time.Second)

	// Cooked: the loop's first attach is to the last target, B, down.
	n := w.CountMarks("prepare")
	t2 := w.Loop("c", a, map[string]string{"TOWER_TEST_PICKER": "0"})
	w.Eventually(5*time.Second, "the loop preparing B", func() bool { return w.CountMarks("prepare") > n && len(a.LoopPids()) == 1 })
	time.Sleep(time.Second)
	start = time.Now()
	// What ctrl-c on a cooked terminal sends the loop. (The pane's own
	// shell, which a terminal would not have, would take the key's SIGINT
	// too and end the pane.)
	Signal(a.LoopPids()[0], syscall.SIGINT)
	t2.Wait(Prompt, 3*time.Second)
	t.Logf("cooked: ctrl-c → the picker in %v", time.Since(start).Round(time.Millisecond))
	t2.Keys("Escape")
	t2.Wait(`LOOP-EXIT=0`, 5*time.Second)
	if p := a.LoopPids(); len(p) != 0 {
		t.Fatalf("pid files left: %v", p)
	}
}

// TestLoopRevivesTowerd: a home towerd that goes away while the loop sits
// at the picker is started again, and learns the loop.
func TestLoopRevivesTowerd(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "revive")
	a := w.Host("A", []string{"alpha"})
	stdSetup(w, a)
	term := w.Loop("t", a, nil)
	term.Wait(Prompt, 6*time.Second)
	w.Eventually(3*time.Second, "the loop at its home", func() bool { return len(w.Loops(a)) == 1 })
	old := a.TowerdPid()
	Kill9(old)
	w.WaitGone(old, 3*time.Second)
	start := time.Now()
	w.Eventually(8*time.Second, "a towerd again, knowing the loop", func() bool {
		pid := a.TowerdPid()
		return pid != 0 && pid != old && len(w.Loops(a)) == 1
	})
	t.Logf("towerd back with the loop in %v", time.Since(start).Round(time.Millisecond))
	term.Pick("alpha")
	w.WaitLoop(a, "^A:alpha", 6*time.Second)
}
