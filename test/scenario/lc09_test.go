package scenario

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// lc09Gaps are LC09's waits between coming back to the laptop and
// pressing prefix L again, in ms.
var lc09Gaps = []int{50, 100, 200, 2000}

// LC09: tower last pressed soon after a switch. Back on the laptop from a
// host, prefix L to the host again 50, 100 or 200ms after the laptop's
// client started is as fast as 2s after, once the host has had a round
// trip and 50ms to hand its session back: the session is reused, not
// opened again.
func TestLC09(t *testing.T) {
	rtts := []int{0, 50, 150}
	report := map[int]map[int]time.Duration{}
	for _, rtt := range rtts {
		t.Run(fmt.Sprint(rtt), func(t *testing.T) {
			report[rtt] = lc09(t, rtt)
		})
	}
	for _, rtt := range rtts {
		var line []string
		for _, gap := range lc09Gaps {
			line = append(line, fmt.Sprintf("%dms after: %v", gap, report[rtt][gap].Round(time.Millisecond)))
		}
		t.Logf("RTT %3dms laptop → remote, %s", rtt, strings.Join(line, ", "))
	}
	for _, rtt := range rtts {
		settled := report[rtt][2000]
		for _, gap := range lc09Gaps {
			if gap >= rtt+50 && report[rtt][gap] > settled+25*time.Millisecond {
				t.Errorf("RTT %dms: prefix L %dms after a switch took %v, %v once settled", rtt, gap, report[rtt][gap], settled)
			}
		}
	}
}

// lc09 is LC09 at one round trip: each gap's median, from the press to
// tmux starting on the host.
func lc09(t *testing.T, rtt int) map[int]time.Duration {
	w := NewWorld(t, fmt.Sprintf("lc09-%d", rtt))
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Shape("B", func(l *Link) { l.DelayMs = rtt / 2 })
	stdSetup(w, a, b)
	term := w.LoopTo("t", a, map[string]string{"TOWER_STANDBY": "1"}, "alpha", "^A:alpha")
	w.WaitMark("standby: ready B", 10*time.Second)
	term.DashTo("bravo")
	w.WaitLoop(a, "^B:bravo", 8*time.Second)
	term.Wait(statusBar("bravo"), 5*time.Second)
	time.Sleep(time.Second)
	// started is when tmux next started on any host after at.
	started := func(at time.Time) time.Time {
		var got time.Time
		w.Eventually(8*time.Second, "tmux started", func() bool {
			for _, m := range w.ReadMarks() {
				if m.What == "shim: exec tmux" && m.At.After(at) {
					got = m.At
					return true
				}
			}
			return false
		})
		return got
	}
	took := map[int][]time.Duration{}
	for range 3 {
		for _, gap := range lc09Gaps {
			w.ClearMarks()
			press := time.Now()
			term.Keys("C-b", "L")
			back := started(press)
			time.Sleep(time.Until(back.Add(time.Duration(gap) * time.Millisecond)))
			press = time.Now()
			term.Keys("C-b", "L")
			took[gap] = append(took[gap], started(press).Sub(press))
			w.WaitLoop(a, "^B:bravo", 8*time.Second)
			term.Wait(statusBar("bravo"), 5*time.Second)
			if w.CountMarks("attach: session") != 0 {
				t.Fatalf("gap %dms: a session was opened: %q", gap, w.MarkTexts())
			}
			time.Sleep(time.Second)
		}
	}
	out := map[int]time.Duration{}
	for gap, ds := range took {
		slices.Sort(ds)
		out[gap] = ds[len(ds)/2]
	}
	return out
}
