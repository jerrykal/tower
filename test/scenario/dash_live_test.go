package scenario

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Live dashboards and their requests: LV04, LD01, LD03.

// cursorRe reads the dashboard's cursor row from its breadcrumb: the
// host, then the session after its glyph.
var cursorRe = regexp.MustCompile(`([A-Z])  \x{ebc8} (\S+)`)

// crumbRe is the breadcrumb on C:charlie with its window, which comes
// from the view, before the capture.
var crumbRe = regexp.MustCompile(`C  \x{ebc8} charlie  \x{f04e9} \d+:`)

func (t *Term) cursor() string {
	m := cursorRe.FindStringSubmatch(t.Screen())
	if m == nil {
		return ""
	}
	return m[1] + ":" + m[2]
}

// timeUntil runs act and times how long until the screen does (present)
// or no longer does match re, polling every 3ms for at most d; -1 is a
// timeout.
func (t *Term) timeUntil(act func(), re string, present bool, d time.Duration) time.Duration {
	start := time.Now()
	act()
	if t.Until(re, present, d) < 0 {
		return -1
	}
	return time.Since(start)
}

func logResults(t *testing.T, tag string, res map[string][]time.Duration) {
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range res {
			if !yield(k) {
				return
			}
		}
	})
	var lines []string
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("%s %-34s median %-8v %v", tag, k, median(res[k]).Round(time.Millisecond), res[k]))
	}
	t.Log("\n" + strings.Join(lines, "\n"))
	if out := os.Getenv("LD_OUT"); out != "" {
		if f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.WriteString(strings.Join(lines, "\n") + "\n")
			f.Close()
		}
	}
}

// LV04: an open dashboard follows the view without a key, within a
// round trip plus its redraw, and keeps its cursor on its row.
func TestLV04(t *testing.T) {
	rtt := rttList("LV_RTTS", 50)[0]
	x := newLV(t, "lv04", rtt)
	term := x.w.LoopTo("t", x.a, nil, "b-one", "^B:b-one")
	term.Keys("M-o")
	term.Wait(Prompt, 6*time.Second)
	res := map[string][]time.Duration{}
	for i := range 3 {
		time.Sleep(400 * time.Millisecond)
		name := fmt.Sprintf("lvd%d", i)
		res["create on C, on screen"] = append(res["create on C, on screen"],
			term.timeUntil(func() { x.c.NewSession(name) }, rowRe("C", name), true, 3*time.Second))
	}
	time.Sleep(400 * time.Millisecond)
	res["kill on C, off screen"] = append(res["kill on C, off screen"],
		term.timeUntil(func() { x.c.MustTmux("kill-session", "-t", "=lvd0") }, rowRe("C", "lvd0"), false, 3*time.Second))
	logResults(t, fmt.Sprintf("LV04 rtt=%d", rtt), res)
	for k, ds := range res {
		if slices.Contains(ds, -1) {
			t.Fatalf("%s: timed out: %v", k, ds)
		}
		if m := median(ds); m >= time.Duration(rtt+300)*time.Millisecond {
			t.Fatalf("%s: median %v at %dms round trips", k, m, rtt)
		}
	}

	// The cursor stays on its row, not its line.
	x.c.NewSession("lvd3")
	x.c.NewSession("lvd4")
	term.Wait(rowRe("C", "lvd4"), 3*time.Second)
	// New sessions are the most recent, above the cursor: move up to it
	// (or down, should it be below).
	for _, key := range []string{"Up", "Down"} {
		for range 12 {
			if term.cursor() == "C:lvd3" {
				break
			}
			term.Keys(key)
			time.Sleep(150 * time.Millisecond)
		}
	}
	if got := term.cursor(); got != "C:lvd3" {
		t.Fatalf("the cursor never reached C:lvd3 (on %q):\n%s", got, term.Screen())
	}
	x.c.MustTmux("kill-session", "-t", "=lvd1")
	if term.Until(rowRe("C", "lvd1"), false, 3*time.Second) < 0 {
		t.Fatalf("lvd1 still on screen:\n%s", term.Screen())
	}
	time.Sleep(300 * time.Millisecond)
	if got := term.cursor(); got != "C:lvd3" {
		t.Fatalf("after a row above went the cursor is on %q, want C:lvd3:\n%s", got, term.Screen())
	}
	term.CloseDash()
}

// LD01: dashboard requests at 150ms (and LD_RTTS) round trips: rows and
// previews fast; after every answer the rows show its result; a kill's
// row goes before the answer; a preview's windows before its capture.
func TestLD01(t *testing.T) {
	reps := 3
	if n := envInt("LD_REPS"); n > 0 {
		reps = n
	}
	for _, rtt := range rttList("LD_RTTS", 150) {
		t.Run(fmt.Sprint(rtt), func(t *testing.T) {
			w := NewWorld(t, fmt.Sprintf("ld01-%d", rtt))
			a := w.Host("A", []string{"alpha", "apple"})
			b := w.Host("B", []string{"bravo"})
			c := w.Host("C", []string{"charlie"})
			for _, n := range []string{"B", "C"} {
				w.Shape(n, func(l *Link) { l.DelayMs = rtt / 2 })
			}
			w.Home(a, b.Remote(), c.Remote())
			w.WaitLink(a, "B", "up", 15*time.Second)
			w.WaitLink(a, "C", "up", 15*time.Second)
			tb := w.LoopTo("tb", a, nil, "bravo", "^B:bravo")
			ta := w.LoopTo("ta", a, nil, "alpha", "^A:alpha")
			time.Sleep(time.Second)
			type dash struct {
				h    *Host
				cl   string
				term *Term
			}
			var dashes []dash
			for _, d := range []struct {
				h       *Host
				session string
				term    *Term
			}{{b, "bravo", tb}, {a, "alpha", ta}} {
				ids := d.h.ClientIDs(d.session)
				if len(ids) == 0 {
					t.Fatalf("no client on %s:%s", d.h.Name, d.session)
				}
				dashes = append(dashes, dash{d.h, ids[0], d.term})
			}
			res := map[string][]time.Duration{}
			add := func(k string, d time.Duration) { res[k] = append(res[k], d) }
			ui := func(d dash, args ...string) (string, time.Duration) {
				t.Helper()
				start := time.Now()
				out, err := d.h.UI(d.cl, nil, args...)
				if err != nil {
					t.Fatalf("_ui %v on %s: %v\n%s", args, d.h.Name, err, out)
				}
				return out, time.Since(start)
			}

			// Rows and previews.
			for _, d := range dashes {
				for range 2 * reps {
					_, el := ui(d, "rows")
					add("rows on "+d.h.Name, el)
				}
			}
			for _, p := range []struct {
				d             dash
				host, session string
			}{{dashes[1], "A", "alpha"}, {dashes[1], "B", "bravo"}, {dashes[0], "B", "bravo"}, {dashes[0], "A", "apple"}, {dashes[0], "C", "charlie"}} {
				for range reps {
					out, el := ui(p.d, "preview", p.host, p.session)
					if !strings.HasPrefix(out, p.host+":"+p.session+"  ($") {
						t.Fatalf("preview of %s:%s from %s: %q", p.host, p.session, p.d.h.Name, out)
					}
					add(fmt.Sprintf("preview %s→%s", p.d.h.Name, p.host), el)
				}
			}

			// Requests: the first rows read after each answer show it.
			fresh := map[string]int{}
			for _, d := range dashes {
				for _, th := range []string{"A", "B", "C"} {
					for i := range reps {
						name := strings.ToLower(fmt.Sprintf("ld%s%s%d", d.h.Name, th, i))
						for _, step := range []struct {
							op   string
							args []string
							row  string
							want bool
						}{
							{"new", []string{"new", th, name}, name, true},
							{"rename", []string{"rename", th, name, name + "r"}, name + "r", true},
							{"kill", []string{"kill", th, name + "r"}, name + "r", false},
						} {
							out, el := ui(d, step.args...)
							if !strings.Contains(out, step.op+" on "+th+": done") {
								t.Fatalf("%v from %s: %q", step.args, d.h.Name, out)
							}
							add(fmt.Sprintf("%s %s→%s", step.op, d.h.Name, th), el)
							rows, _ := ui(d, "rows")
							if strings.Contains(rows, "  "+step.row+"  ") == step.want {
								fresh[step.op]++
							}
						}
					}
				}
			}
			for _, op := range []string{"new", "rename", "kill"} {
				if fresh[op] != 6*reps {
					t.Errorf("%s: the rows right after the answer showed it %d times of %d", op, fresh[op], 6*reps)
				}
			}

			// ^x in the popup: the row goes before the answer.
			early := map[string]int{}
			for _, d := range dashes {
				for i := range reps {
					victim := strings.ToLower(fmt.Sprintf("vic%s%d", d.h.Name, i))
					c.NewSession(victim)
					w.Eventually(5*time.Second, victim+" in the rows", func() bool {
						rows, _ := d.h.UI(d.cl, nil, "rows")
						return strings.Contains(rows, victim)
					})
					time.Sleep(200 * time.Millisecond)
					d.term.Keys("M-o")
					d.term.Wait(Prompt, 6*time.Second)
					d.term.Wait(rowRe("C", victim), 6*time.Second)
					d.term.Type(victim)
					time.Sleep(300 * time.Millisecond)
					pressed := time.Now()
					// ^x asks; y kills.
					gone := d.term.timeUntil(func() { d.term.Keys("C-x", "y") }, rowRe("C", victim), false, 3*time.Second)
					add("^x row gone on "+d.h.Name, gone)
					var answered time.Time
					w.Eventually(3*time.Second, "the kill's answer", func() bool {
						for _, m := range w.ReadMarks() {
							if m.What == "dash: kill answered" && m.At.After(pressed) {
								answered = m.At
								return true
							}
						}
						return false
					})
					add("^x answered on "+d.h.Name, answered.Sub(pressed))
					if gone >= 0 && pressed.Add(gone).Before(answered) {
						early[d.h.Name]++
					}
					w.Eventually(3*time.Second, victim+" killed on C", func() bool { return !c.hasSession(victim) })
					d.term.CloseDash()
				}
			}

			// The preview: the windows at once, the capture after.
			c.MustTmux("send-keys", "-t", "charlie", "clear; echo PREVIEW-MARK-$((6*7))", "Enter")
			time.Sleep(300 * time.Millisecond)
			for _, d := range dashes {
				for range reps {
					d.term.Keys("M-o")
					d.term.Wait(Prompt, 6*time.Second)
					d.term.Wait("charlie", 6*time.Second)
					time.Sleep(300 * time.Millisecond)
					start := time.Now()
					d.term.Type("charli")
					win, pane := time.Duration(-1), time.Duration(-1)
					for time.Since(start) < 5*time.Second && pane < 0 {
						s := d.term.Screen()
						if win < 0 && crumbRe.MatchString(s) {
							win = time.Since(start)
						}
						if strings.Contains(s, "PREVIEW-MARK-42") {
							pane = time.Since(start)
						}
						time.Sleep(3 * time.Millisecond)
					}
					add("preview windows on "+d.h.Name, win)
					add("preview pane on "+d.h.Name, pane)
					if win < 0 || pane < 0 {
						t.Fatalf("preview on %s: windows %v, pane %v:\n%s", d.h.Name, win, pane, d.term.Screen())
					}
					d.term.CloseDash()
				}
			}
			logResults(t, fmt.Sprintf("LD01 rtt=%d", rtt), res)
			for _, d := range dashes {
				if ds := res["^x row gone on "+d.h.Name]; slices.Contains(ds, -1) {
					t.Fatalf("^x on %s: a row never went: %v", d.h.Name, ds)
				}
				if early[d.h.Name] != reps {
					t.Fatalf("^x on %s: the row went before the answer %d times of %d", d.h.Name, early[d.h.Name], reps)
				}
			}
		})
	}
}

// LD03: ^x three times fast kills three sessions.
func TestLD03(t *testing.T) {
	w := NewWorld(t, "ld03")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"})
	c := w.Host("C", []string{"charlie", "spam1", "spam2", "spam3", "spam4", "spam5"})
	for _, n := range []string{"B", "C"} {
		w.Shape(n, func(l *Link) { l.DelayMs = 75 })
	}
	w.Home(a, b.Remote(), c.Remote())
	w.WaitLink(a, "B", "up", 15*time.Second)
	w.WaitLink(a, "C", "up", 15*time.Second)
	term := w.LoopTo("t", a, nil, "bravo", "^B:bravo")
	time.Sleep(time.Second)
	term.Keys("M-o")
	term.Wait(Prompt, 6*time.Second)
	term.Type("spam")
	time.Sleep(400 * time.Millisecond)
	for range 3 {
		term.Keys("C-x", "y")
		time.Sleep(80 * time.Millisecond)
	}
	spam := func() int {
		n := 0
		for _, s := range c.Sessions() {
			if strings.HasPrefix(s, "spam") {
				n++
			}
		}
		return n
	}
	w.Eventually(3*time.Second, "three spam sessions killed", func() bool { return spam() <= 2 })
	rows := regexp.MustCompile(`C {3,}\S spam\d( |$)`) // as rowRe
	w.Eventually(3*time.Second, "two spam rows", func() bool { return len(rows.FindAllString(term.Screen(), -1)) == 2 })
	time.Sleep(500 * time.Millisecond)
	if n := spam(); n != 2 || !c.hasSession("charlie") {
		t.Fatalf("C has %q", c.Sessions())
	}
	if n := len(rows.FindAllString(term.Screen(), -1)); n != 2 {
		t.Fatalf("the dashboard lists %d spam rows:\n%s", n, term.Screen())
	}
}
