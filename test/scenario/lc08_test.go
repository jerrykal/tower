package scenario

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// lc08Key is one mode and hop of LC08.
type lc08Key struct{ mode, hop string }

// LC08: a switch's critical path, from the dashboard's ⏎ to the shim
// exec'ing tmux on the target, at round trips 0, 50, 150 and 400ms:
// remote → remote, remote → laptop and laptop → remote, with the
// dashboard ending the old client, the loop ending it, and through
// standby sessions.
func TestLC08(t *testing.T) {
	report := map[int]map[lc08Key][]time.Duration{}
	for _, rtt := range lcRTTs {
		t.Run(fmt.Sprint(rtt), func(t *testing.T) {
			report[rtt] = lc08(t, rtt)
		})
	}
	for _, rtt := range lcRTTs {
		for _, mode := range []string{"dashboard", "eager", "standby"} {
			var line []string
			for _, hop := range []string{"remote → remote", "remote → laptop", "laptop → remote"} {
				ds := slices.Clone(report[rtt][lc08Key{mode, hop}])
				if len(ds) == 0 {
					continue
				}
				slices.Sort(ds)
				line = append(line, fmt.Sprintf("%s %v", hop, ds[len(ds)/2].Round(time.Millisecond)))
			}
			t.Logf("RTT %3dms %-9s %s", rtt, mode, strings.Join(line, ", "))
		}
	}
}

func lc08(t *testing.T, rtt int) map[lc08Key][]time.Duration {
	out := map[lc08Key][]time.Duration{}
	w := NewWorld(t, fmt.Sprintf("lc08-%d", rtt))
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	c := w.Host("C", []string{"charlie"}, SSHHost())
	for _, n := range []string{"B", "C"} {
		w.Shape(n, func(l *Link) { l.DelayMs = rtt / 2 })
	}
	stdSetup(w, a, b, c)
	modes := []struct {
		name string
		env  map[string]string
	}{
		{"dashboard", map[string]string{"TOWER_EAGER": "0", "TOWER_STANDBY": "0"}},
		{"eager", map[string]string{"TOWER_STANDBY": "0"}},
		{"standby", map[string]string{"TOWER_STANDBY": "1"}},
	}
	steps := []struct {
		hop, query, re string
		host           string // a remote target's host, "" for the laptop
	}{
		{"remote → remote", "charlie", "^C:charlie", "C"},
		{"remote → laptop", "alpha", "^A:alpha", ""},
		{"laptop → remote", "bravo", "^B:bravo", "B"},
	}
	for _, m := range modes {
		term := w.LoopTo(m.name, a, m.env, "bravo", "^B:bravo")
		time.Sleep(time.Second)
		for range 3 {
			for _, s := range steps {
				if m.name == "standby" && s.host != "" {
					w.Eventually(10*time.Second, "a standby to "+s.host+" ready", func() bool {
						ms := w.MarkTexts()
						last := -1
						for j, t := range ms {
							if t == "standby: start "+s.host {
								last = j
							}
						}
						return last >= 0 && slices.Contains(ms[last:], "standby: ready "+s.host)
					})
				}
				base := len(w.ReadMarks())
				term.DashTo(s.query)
				w.WaitLoop(a, s.re, 15*time.Second)
				w.Eventually(5*time.Second, "the home seeing the new client", func() bool {
					return slices.ContainsFunc(w.ReadMarks()[base:], func(x Mark) bool { return x.What == "home sees the new client" })
				})
				time.Sleep(300 * time.Millisecond)
				marks := w.ReadMarks()[base:]
				if m.name == "standby" && s.host != "" && !slices.ContainsFunc(marks, func(x Mark) bool { return x.What == "standby: taken" }) {
					t.Fatalf("%s %s: no standby taken", m.name, s.hop)
				}
				var enter, exec time.Time
				for _, x := range marks {
					switch {
					case x.What == "dash: enter" && enter.IsZero():
						enter = x.At
					case x.What == "shim: exec tmux" && !enter.IsZero() && exec.IsZero():
						exec = x.At
					}
				}
				if enter.IsZero() || exec.IsZero() {
					t.Fatalf("%s %s: marks %v", m.name, s.hop, marks)
				}
				k := lc08Key{m.name, s.hop}
				out[k] = append(out[k], exec.Sub(enter))
			}
		}
		term.Keys("C-b", "d")
		term.Wait(`LOOP-EXIT=`, 6*time.Second)
	}
	return out
}
