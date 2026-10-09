package scenario

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

// rttList reads a list of round trips (ms) from env, or def.
func rttList(env string, def ...int) []int {
	v := os.Getenv(env)
	if v == "" {
		return def
	}
	var out []int
	for _, f := range strings.Split(v, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// lvWorld is A (the home), B and C, the remotes at a round trip of rtt.
type lvWorld struct {
	w       *World
	a, b, c *Host
}

func newLV(t *testing.T, id string, rtt int) *lvWorld {
	w := NewWorld(t, id)
	x := &lvWorld{w: w}
	x.a = w.Host("A", []string{"alpha"})
	x.b = w.Host("B", []string{"b-one", "b-two"}, SSHHost())
	x.c = w.Host("C", []string{"c-one", "c-two"}, SSHHost())
	for _, n := range []string{"B", "C"} {
		w.Shape(n, func(l *Link) { l.DelayMs = rtt / 2 })
	}
	w.Home(x.a, x.b.Remote(), x.c.Remote())
	w.WaitLink(x.a, "B", "up", 15*time.Second)
	w.WaitLink(x.a, "C", "up", 15*time.Second)
	w.Eventually(10*time.Second, "each remote holds the other's sessions", func() bool {
		return HasSession(&x.b.View("").View, "C", "c-one") && HasSession(&x.c.View("").View, "B", "b-one")
	})
	return x
}

// race times how long until cond holds on the home's view and on the view
// B holds, polling every 3ms for at most 5s; -1 is a timeout.
func (x *lvWorld) race(act func(), cond func(*proto.View) bool) [2]time.Duration {
	home, held := time.Duration(-1), time.Duration(-1)
	start := time.Now()
	act()
	for time.Since(start) < 5*time.Second && (home < 0 || held < 0) {
		if home < 0 && cond(&x.a.View("").View) {
			home = time.Since(start)
		}
		if held < 0 && cond(&x.b.View("").View) {
			held = time.Since(start)
		}
		time.Sleep(3 * time.Millisecond)
	}
	return [2]time.Duration{home, held}
}

func windowIn(v *proto.View, host, session, window string) *proto.Window {
	h := HostIn(v, host)
	if h == nil {
		return nil
	}
	for _, s := range h.Sessions {
		if s.Name != session {
			continue
		}
		for i := range s.Windows {
			if s.Windows[i].Name == window {
				return &s.Windows[i]
			}
		}
	}
	return nil
}

func loopAt(v *proto.View, host, session string) bool {
	for _, l := range v.Loops {
		if l.Cur.Name == host && l.Cur.Label == session {
			return true
		}
	}
	return false
}

func median(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)/2]
}

// LV01: a change on one remote reaches another remote's view in about
// one round trip; a bell in one round trip (alert hooks); a client's
// switch-client as well.
func TestLV01(t *testing.T) {
	reps := 3
	if n, err := strconv.Atoi(os.Getenv("LV_REPS")); err == nil && n > 0 {
		reps = n
	}
	for _, rtt := range rttList("LV_RTTS", 0, 150) {
		t.Run(fmt.Sprintf("rtt%d", rtt), func(t *testing.T) {
			x := newLV(t, fmt.Sprintf("lv01-%d", rtt), rtt)
			c := x.c
			res := map[string][]time.Duration{}
			add := func(what string, r [2]time.Duration) {
				res[what+" C→home view"] = append(res[what+" C→home view"], r[0])
				res[what+" C→B view"] = append(res[what+" C→B view"], r[1])
			}
			for i := range reps {
				name := fmt.Sprintf("lv%d", i)
				time.Sleep(300 * time.Millisecond)
				add("create", x.race(func() { c.NewSession(name) }, func(v *proto.View) bool { return HasSession(v, "C", name) }))
				time.Sleep(300 * time.Millisecond)
				add("rename", x.race(func() { c.MustTmux("rename-session", "-t", "="+name, name+"r") }, func(v *proto.View) bool { return HasSession(v, "C", name+"r") }))
				time.Sleep(300 * time.Millisecond)
				bw := fmt.Sprintf("bw%d", i)
				c.MustTmux("new-window", "-d", "-t", "="+name+"r:", "-n", bw, "sleep 0.5; printf '\\a'; exec sleep 100")
				x.w.Eventually(5*time.Second, "the bell window on B", func() bool { return windowIn(&x.b.View("").View, "C", name+"r", bw) != nil })
				x.w.Eventually(3*time.Second, "the bell flag", func() bool {
					out, _ := c.Tmux("display-message", "-p", "-t", "="+name+"r:"+bw, "#{window_bell_flag}")
					return strings.TrimSpace(out) == "1"
				})
				add("bell", x.race(func() {}, func(v *proto.View) bool {
					w := windowIn(v, "C", name+"r", bw)
					return w != nil && w.Bell
				}))
			}
			x.w.LoopTo("t", x.a, nil, "c-one", "^C:c-one")
			for range reps {
				// The loop's place can reach the home's view before C
				// lists the client the loop attached.
				var ids []string
				x.w.Eventually(5*time.Second, "a client on c-one", func() bool { ids = c.ClientIDs("c-one"); return len(ids) > 0 })
				name := ClientName(ids[0])
				add("switch-client", x.race(func() { c.MustTmux("switch-client", "-c", name, "-t", "=c-two") }, func(v *proto.View) bool { return loopAt(v, "C", "c-two") }))
				c.MustTmux("switch-client", "-c", name, "-t", "=c-one")
				x.w.Eventually(5*time.Second, "back on c-one in B's view", func() bool { return loopAt(&x.b.View("").View, "C", "c-one") })
				time.Sleep(200 * time.Millisecond)
			}
			// Hand-offs C → B → C from the dashboard there, timed until the
			// home's view shows the loop's new client seen.
			for range reps {
				for _, hop := range []struct {
					from, to *Host
					session  string
				}{{c, x.b, "b-one"}, {x.b, c, "c-one"}} {
					ids := hop.from.ClientIDs("")
					if len(ids) == 0 {
						t.Fatalf("no client on %s", hop.from.Name)
					}
					start := time.Now()
					if out, err := hop.from.UI(ids[0], nil, "goto", hop.to.Name, hop.session); err != nil {
						t.Fatalf("goto %s:%s: %v %s", hop.to.Name, hop.session, err, out)
					}
					x.w.Eventually(15*time.Second, "a client on "+hop.to.Name, func() bool { return len(hop.to.ClientIDs(hop.session)) > 0 })
					bound := time.Duration(-1)
					for time.Since(start) < 5*time.Second {
						v := x.a.View("")
						if v != nil && slices.ContainsFunc(v.View.Loops, func(l proto.Loop) bool {
							return l.Cur.Name == hop.to.Name && l.Cur.Label == hop.session && l.Seen
						}) {
							bound = time.Since(start)
							break
						}
						time.Sleep(3 * time.Millisecond)
					}
					res["hand-off "+hop.from.Name+"→"+hop.to.Name+", home binds"] = append(res["hand-off "+hop.from.Name+"→"+hop.to.Name+", home binds"], bound)
					if bound < 0 {
						t.Fatalf("the home never saw the loop's client on %s", hop.to.Name)
					}
					time.Sleep(400 * time.Millisecond)
				}
			}
			keys := make([]string, 0, len(res))
			for k := range res {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				t.Logf("LVRESULT rtt=%d %-28s median %-8v %v", rtt, k, median(res[k]).Round(time.Millisecond), res[k])
			}
			for _, what := range []string{"create", "rename", "bell", "switch-client"} {
				if ds := res[what+" C→home view"]; slices.Contains(ds, -1) {
					t.Fatalf("%s: timed out on the home: %v", what, ds)
				}
				ds := res[what+" C→B view"]
				if slices.Contains(ds, -1) {
					t.Fatalf("%s: timed out: %v", what, ds)
				}
				if m := median(ds); m >= time.Duration(rtt+200)*time.Millisecond {
					t.Fatalf("%s C→B: median %v at %dms round trips", what, m, rtt)
				}
			}
		})
	}
}

// LV05: under bell-action other, a bell in the current window of a
// detached session on a remote, which sets the flag and runs no alert
// hook, reaches the home's view and another remote's: in one round trip
// on tmux 3.8 (pane-bell), within the subscription's second before it.
// Activity under the default activity-action other takes the
// subscription on every version.
func TestLV05(t *testing.T) {
	reps := 3
	if n, err := strconv.Atoi(os.Getenv("LV_REPS")); err == nil && n > 0 {
		reps = n
	}
	rtt := rttList("LV_RTTS", 150)[0]
	x := newLV(t, "lv05", rtt)
	c := x.c
	ver := strings.TrimSpace(c.MustTmux("display-message", "-p", "#{version}"))
	c.MustTmux("set-option", "-g", "bell-action", "other")
	res := map[string][]time.Duration{}
	for i := range reps {
		for _, what := range []string{"bell", "activity"} {
			name := fmt.Sprintf("lv5%s%d", what[:1], i)
			out := `printf '\a'`
			if what == "activity" {
				out = "echo x"
			}
			// No exec and no rename: a window renamed when the bell
			// comes would have towerd re-read anyway.
			c.MustTmux("new-session", "-d", "-s", name, "sleep 2; "+out+"; sleep 100",
				";", "set-option", "-w", "-t", "="+name+":", "automatic-rename", "off")
			if what == "activity" {
				c.MustTmux("set-option", "-w", "-t", "="+name+":", "monitor-activity", "on")
			}
			x.w.Eventually(5*time.Second, name+" on B", func() bool { return HasSession(&x.b.View("").View, "C", name) })
			x.w.Eventually(5*time.Second, "the "+what+" flag", func() bool {
				out, _ := c.Tmux("display-message", "-p", "-t", "="+name+":", "#{window_bell_flag}#{window_activity_flag}")
				return strings.TrimSpace(out) != "00"
			})
			r := x.race(func() {}, func(v *proto.View) bool {
				h := HostIn(v, "C")
				if h == nil {
					return false
				}
				for _, s := range h.Sessions {
					if s.Name == name && len(s.Windows) > 0 {
						w := s.Windows[0]
						return what == "bell" && w.Bell || what == "activity" && w.Activity
					}
				}
				return false
			})
			res[what+" C→home view"] = append(res[what+" C→home view"], r[0])
			res[what+" C→B view"] = append(res[what+" C→B view"], r[1])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(res)) {
		t.Logf("LVRESULT tmux=%s rtt=%d %-24s median %-8v %v", ver, rtt, k, median(res[k]).Round(time.Millisecond), res[k])
	}
	paneBell := c.TmuxAtLeast(3, 8)
	for _, what := range []string{"bell", "activity"} {
		bound := time.Duration(rtt+1300) * time.Millisecond // the subscription's second
		if what == "bell" && paneBell {
			bound = time.Duration(rtt+200) * time.Millisecond
		}
		for _, view := range []string{"home", "B"} {
			ds := res[what+" C→"+view+" view"]
			if slices.Contains(ds, -1) {
				t.Fatalf("%s: never reached %s's view: %v", what, view, ds)
			}
			if m := median(ds); m >= bound {
				t.Fatalf("%s C→%s on tmux %s: median %v at %dms round trips", what, view, ver, m, rtt)
			}
		}
	}
}

// LV02: a burst of 40 switches and 40 new windows costs the home a few
// states (paced), and the end of it shows on another remote within a
// second plus a round trip.
func TestLV02(t *testing.T) {
	rtt := rttList("LV_RTTS", 50)[0]
	x := newLV(t, "lv02", rtt)
	c := x.c
	l := x.w.FakeLoop(x.a)
	l.Attach(x.w.Ref(x.a, "C", "c-one"), c)
	name := ClientName(c.ClientIDs("c-one")[0])
	time.Sleep(time.Second)
	states0 := x.w.Link(x.a, "C").States
	tx0 := x.w.Link(x.a, "B").Tx
	start := time.Now()
	for i := range 40 {
		to := "=c-one"
		if i%2 == 0 {
			to = "=c-two"
		}
		c.MustTmux("switch-client", "-c", name, "-t", to)
		c.MustTmux("new-window", "-d", "-t", "=c-two:", "-n", fmt.Sprintf("w%d", i))
		time.Sleep(25 * time.Millisecond)
	}
	c.MustTmux("switch-client", "-c", name, "-t", "=c-two")
	c.MustTmux("rename-window", "-t", "=c-two:w0", "final")
	burst := time.Since(start)
	settle := x.race(func() {}, func(v *proto.View) bool { return loopAt(v, "C", "c-two") && windowIn(v, "C", "c-two", "final") != nil })[1]
	time.Sleep(time.Second)
	states := x.w.Link(x.a, "C").States - states0
	tx := x.w.Link(x.a, "B").Tx - tx0
	limit := 2 + int(burst/(100*time.Millisecond)) + 3
	t.Logf("a %v burst: %d states from C (limit %d), %d bytes of views to B, settled on B %v after it", burst.Round(time.Millisecond), states, limit, tx, settle.Round(time.Millisecond))
	if settle < 0 || settle >= time.Second+time.Duration(rtt)*time.Millisecond {
		t.Fatalf("settled after %v", settle)
	}
	if states > limit {
		t.Fatalf("%d states for the burst, limit %d", states, limit)
	}
}
