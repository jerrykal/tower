package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/tmux"
)

// LC07: spawn cost. tower runs tmux past version-manager shims and passes
// the machine key down, so neither a shim's process start nor the
// machine id lookup sits on a hand-off or a dashboard.
//
// The hosts use the real machine key (no TOWER_MACHINE_ID) and the real
// PATH, with a mise-style shim for tmux put first on it: a script under
// …/mise/shims that logs every call and runs the real tmux. mise cannot
// say which tmux it runs (tmux is not a mise tool), so tower must pass the
// shim over; any call through it is a tmux started past the resolved
// binary.
func TestLC07(t *testing.T) {
	w := NewWorld(t, "lc07")
	shims := filepath.Join(w.Dir, "mise", "shims")
	os.MkdirAll(shims, 0o700)
	shimLog := filepath.Join(w.Dir, "shim.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %s\nexec %s \"$@\"\n", shellQuote(shimLog), shellQuote(tmux.Bin()))
	if err := os.WriteFile(filepath.Join(shims, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	path := shims + ":" + os.Getenv("PATH")
	opts := []HostOpt{Env("TOWER_MACHINE_ID", ""), Env("PATH", path)}
	a := w.Host("A", []string{"alpha"}, opts...)
	b := w.Host("B", []string{"bravo"}, opts...)
	w.Home(a, b.Remote())
	w.WaitLink(a, "B", "up", 15*time.Second)
	st := a.Status()
	t.Logf("tmux at the home: %s; machine key %s", st.TmuxBin, st.MKey)
	if strings.Contains(st.TmuxBin, "/shims/") {
		t.Fatalf("towerd runs tmux through the shim: %s", st.TmuxBin)
	}
	bkey := w.Link(a, "B").MKey
	if bkey == "" {
		t.Fatal("no machine key for B")
	}
	term := w.LoopTo("t", a, nil, "bravo", "^B:bravo")
	time.Sleep(time.Second)

	var handoffs []time.Duration
	for range 4 {
		for _, to := range []string{"alpha", "bravo"} {
			w.ClearMarks()
			term.DashTo(to)
			host := map[string]string{"alpha": "A", "bravo": "B"}[to]
			w.WaitLoop(a, "^"+host+":"+to, 15*time.Second)
			var stored, seen time.Time
			w.Eventually(3*time.Second, "the home sees the new client", func() bool {
				for _, m := range w.ReadMarks() {
					if m.What == "switch stored: hold" && stored.IsZero() {
						stored = m.At
					}
					if m.What == "home sees the new client" && !stored.IsZero() && m.At.After(stored) {
						seen = m.At
						return true
					}
				}
				return false
			})
			handoffs = append(handoffs, seen.Sub(stored))
			time.Sleep(500 * time.Millisecond)
		}
	}
	t.Logf("hand-offs, from the switch stored to the home seeing the new client: median %v %v",
		median(handoffs).Round(time.Millisecond), handoffs)

	attaches := 0
	for _, e := range w.SSHLog() {
		cmd, _ := e["cmd"].(string)
		if tty, _ := e["tty"].(bool); tty && strings.Contains(cmd, " attach ") && strings.Contains(cmd, "--mkey "+bkey) {
			attaches++
		}
	}
	if attaches < 4 {
		t.Fatalf("%d attaches to B carried its machine key, want at least 4", attaches)
	}

	// Dashboard rows, the machine key passed down as a key binding does.
	rows := func(h *Host, session string) []time.Duration {
		cl := h.ClientIDs(session)
		if len(cl) == 0 {
			t.Fatalf("no client on %s:%s", h.Name, session)
		}
		key := h.Status().MKey
		var ds []time.Duration
		for i := range 10 {
			start := time.Now()
			out, err := h.UI(cl[0], map[string]string{"TOWER_MKEY": key}, "rows")
			ds = append(ds, time.Since(start))
			if err != nil || i == 0 && (!strings.Contains(out, "alpha") || !strings.Contains(out, "bravo")) {
				t.Fatalf("rows on %s: %v\n%s", h.Name, err, out)
			}
		}
		return ds
	}
	onB := rows(b, "bravo")
	term.DashTo("alpha")
	w.WaitLoop(a, "^A:alpha", 15*time.Second)
	time.Sleep(500 * time.Millisecond)
	onA := rows(a, "alpha")
	t.Logf("`tower _ui rows`: median %v on B, %v on A", median(onB).Round(100*time.Microsecond), median(onA).Round(100*time.Microsecond))

	if b, _ := os.ReadFile(shimLog); len(b) > 0 {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		t.Fatalf("%d tmux calls went through the shim, e.g. %q", len(lines), lines[:min(len(lines), 5)])
	}
	if slices.Contains(handoffs, 0) {
		t.Fatalf("hand-offs %v", handoffs)
	}
}
