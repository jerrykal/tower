package ui

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/relay"
)

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		q, text string
		ok      bool
		pos     []int
	}{
		{"", "B bravo", true, nil},
		{"bra", "B bravo", true, []int{2, 3, 4}},
		{"BRA", "B bravo", true, []int{2, 3, 4}},
		{"b bra", "B bravo", true, []int{0, 2, 3, 4}},
		{"bv", "B bravo", true, []int{2, 5}},
		{"bbl", "C bravo-logs", false, nil},
		{"cbl", "C bravo-logs", true, []int{0, 2, 8}},
		{"ovar", "B bravo", false, nil},
		{"日本", "N 日x本", true, []int{2, 4}},
	} {
		_, pos, ok := rank(fold([]rune(c.q)), c.text, strings.IndexByte(c.text, 32)+1)
		if ok != c.ok || !slices.Equal(pos, c.pos) {
			t.Errorf("rank(%q, %q) = %v %v, want %v %v", c.q, c.text, pos, ok, c.pos, c.ok)
		}
	}
}

func TestRows(t *testing.T) {
	m, _, _ := newTestModel(t, testDash(), false)
	want := []string{"A alpha", "B bravo", "B banana", "A apple", "D (no sessions)", "C charlie"}
	if got := names(m); !slices.Equal(got, want) {
		t.Fatalf("rows %q, want %q (reachable first, then by recency)", got, want)
	}
	cols := layout(m.rows)
	text := map[string]string{}
	for i := range m.rows {
		text[m.rows[i].text] = m.rows[i].plain(cols)
	}
	for row, want := range map[string]string{
		"A alpha":         "A  alpha    1w  now  " + glyphCur,
		"B bravo":         "B  bravo    2w  1m   " + glyphPrev + "  " + glyphBell,
		"B banana":        "B  banana   1w  10m  " + glyphClients + " 2",
		"A apple":         "A  apple    1w  2h",
		"D (no sessions)": "D  (no sessions)",
		"C charlie":       "C  charlie  1w  now  down: timed out",
	} {
		if text[row] != want {
			t.Errorf("%s: %q, want %q", row, text[row], want)
		}
	}
}

// TestNoQuitWhileEnterInFlight: once ⏎ is on its way (a switch may be
// stored), neither esc nor ^c ends the dashboard: its outcome does.
func TestNoQuitWhileEnterInFlight(t *testing.T) {
	m, _, _ := newTestModel(t, testDash(), false)
	press(t, m, "b", "r", "a")
	pressNoRun(m, "enter") // in flight: its command not run yet
	if !m.busy {
		t.Fatal("⏎ is not in flight")
	}
	press(t, m, "ctrl+c") // clears the query
	press(t, m, "ctrl+c", "esc")
	if m.quitted {
		t.Fatal("the dashboard quit with ⏎ in flight")
	}
}

func TestFilter(t *testing.T) {
	m, _, _ := newTestModel(t, testDash(), false)
	press(t, m, "b", "a", "n")
	if got := names(m); !slices.Equal(got, []string{"B banana"}) {
		t.Fatalf("ban: %q", got)
	}
	press(t, m, "backspace", "backspace", "backspace", "B", " ", "R")
	if got := names(m); !slices.Equal(got, []string{"B bravo"}) {
		t.Fatalf("B R (case and spaces ignored): %q", got)
	}
	press(t, m, "ctrl+c")
	if len(m.query) != 0 || len(m.shown) != len(m.rows) || m.quitted {
		t.Fatalf("^c clears the query first: %q quit=%v", string(m.query), m.quitted)
	}
	press(t, m, "ctrl+c")
	if !m.quitted {
		t.Fatal("^c on an empty query quits")
	}
}

func TestCursorKeepsItsRow(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	press(t, m, "down", "down")
	if cursorText(m) != "B banana" {
		t.Fatalf("cursor on %q", cursorText(m))
	}
	// New sessions above it and a reorder: the cursor stays on banana.
	f.update(func(d *proto.Dash) {
		b := &d.View.Hosts[1]
		b.Sessions = append(b.Sessions, proto.Session{ID: "$9", Name: "blueberry", Ago: 0})
		b.Sessions[1].Ago = 2_000 // banana now more recent than bravo
	})
	drive(t, m, m.read())
	if got := names(m)[:3]; !slices.Equal(got, []string{"B blueberry", "A alpha", "B banana"}) {
		t.Fatalf("rows %q", got)
	}
	if cursorText(m) != "B banana" {
		t.Fatalf("after the update the cursor is on %q, want banana", cursorText(m))
	}
	// banana gone: the cursor goes to the row that followed it.
	f.update(func(d *proto.Dash) {
		b := &d.View.Hosts[1]
		b.Sessions = slices.DeleteFunc(b.Sessions, func(s proto.Session) bool { return s.Name == "banana" })
	})
	drive(t, m, m.read())
	if cursorText(m) != "B bravo" {
		t.Fatalf("banana gone: cursor on %q, want its neighbour bravo", cursorText(m))
	}
	// A typed query puts the cursor on the first match.
	press(t, m, "a", "p", "p")
	if cursorText(m) != "A apple" {
		t.Fatalf("query: cursor on %q", cursorText(m))
	}
}

func TestKillHidesUntilAnswered(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	release := make(chan bool)
	f.setAnswer(func(r proto.Request) proto.Ack {
		if r.Op != proto.OpKill {
			return proto.Ack{OK: true}
		}
		if <-release {
			f.update(func(d *proto.Dash) {
				b := &d.View.Hosts[1]
				b.Sessions = slices.DeleteFunc(b.Sessions, func(s proto.Session) bool { return s.ID == r.Target.Session })
			})
			return proto.Ack{OK: true}
		}
		return proto.Ack{Err: "no such session"}
	})
	press(t, m, "down") // B bravo
	killCmd := pressNoRun(m, "ctrl+x")
	if slices.Contains(names(m), "B bravo") || cursorText(m) != "B banana" {
		t.Fatalf("^x hides the row at once and moves to its neighbour: %q, cursor %q", names(m), cursorText(m))
	}
	// Reloads while the kill is in flight keep it hidden.
	drive(t, m, m.read())
	if slices.Contains(names(m), "B bravo") {
		t.Fatal("a reload brought back a row whose kill is in flight")
	}
	// The kill fails: the row comes back, with the error.
	go func() { release <- false }()
	drive(t, m, killCmd)
	if !slices.Contains(names(m), "B bravo") || !m.noteErr || !strings.Contains(m.note, "kill on B: no such session") {
		t.Fatalf("a failed kill brings the row back with the error: %q, note %q", names(m), m.note)
	}
	// Three ^x in a row kill three sessions, each hidden at once.
	m.at = 0
	m.sync()
	var cmds []func()
	for range 3 {
		cmd := pressNoRun(m, "ctrl+x")
		cmds = append(cmds, func() { drive(t, m, cmd) })
	}
	if got := names(m); !slices.Equal(got, []string{"A apple", "D (no sessions)", "C charlie"}) {
		t.Fatalf("after three ^x: %q", got)
	}
	f.setAnswer(func(r proto.Request) proto.Ack {
		if r.Op != proto.OpKill {
			return proto.Ack{OK: true}
		}
		f.update(func(d *proto.Dash) {
			for i := range d.View.Hosts {
				h := &d.View.Hosts[i]
				if h.ID == r.Target.Host {
					h.Sessions = slices.DeleteFunc(h.Sessions, func(s proto.Session) bool { return s.ID == r.Target.Session })
				}
			}
		})
		return proto.Ack{OK: true}
	})
	for _, c := range cmds {
		c()
	}
	if got := names(m); !slices.Equal(got, []string{"A apple", "B (no sessions)", "D (no sessions)", "C charlie"}) {
		t.Fatalf("after the answers: %q", got)
	}
	if len(m.hidden) != 0 {
		t.Fatalf("hidden set left: %v", m.hidden)
	}
	if n := len(f.actsOf(proto.OpKill)); n != 4 {
		t.Fatalf("%d kills sent, want 4", n)
	}
	if m.note != "kill on B: done" {
		t.Fatalf("note %q", m.note)
	}
}

func TestKillSurvivesStaleRead(t *testing.T) {
	// The answer arrives while a read that started before it is in
	// flight: that read still shows the session, so the row must stay
	// hidden until the next one.
	m, _, _ := newTestModel(t, testDash(), false)
	pressNoRun(m, "ctrl+x") // A alpha
	readCmd := m.read()
	m.Update(actMsg{op: "kill", host: "A", key: rowKey{Host: "aaaa", Inst: "1:1", Session: "$0"}, ack: proto.Ack{OK: true}})
	m.Update(readCmd()) // the stale read lands; the session is still in the view
	if slices.Contains(names(m), "A alpha") {
		t.Fatal("a read started before the answer brought the row back")
	}
	// Only a read started after the answer clears the hide.
	if h := m.hidden[rowKey{Host: "aaaa", Inst: "1:1", Session: "$0"}]; h.clearAt != 2 {
		t.Fatalf("hide %+v, want cleared by read 2", h)
	}
}

func TestEnterOnGoneRow(t *testing.T) {
	m, f, x := newTestModel(t, testDash(), false)
	press(t, m, "a", "p", "p")
	// apple is killed elsewhere; with live updates off the row stays.
	f.update(func(d *proto.Dash) { d.View.Hosts[0].Sessions = d.View.Hosts[0].Sessions[:1] })
	press(t, m, "enter")
	if m.note != "selection is gone" || m.quitted || len(x.calls) != 0 {
		t.Fatalf("note %q quit %v tmux %v", m.note, m.quitted, x.calls)
	}
	// A renamed session is still reached, by id.
	m2, f2, x2 := newTestModel(t, testDash(), false)
	press(t, m2, "a", "p", "p")
	f2.update(func(d *proto.Dash) { d.View.Hosts[0].Sessions[1].Name = "pear" })
	press(t, m2, "enter")
	if !m2.quitted || !slices.Equal(x2.called("switch-client")[0], []string{"switch-client", "-c", "/dev/ttys042", "-t", "$1"}) {
		t.Fatalf("renamed: quit %v calls %v note %q", m2.quitted, x2.calls, m2.note)
	}
	// A server that restarted since the row was drawn: its ids mean
	// other sessions now.
	m3, f3, x3 := newTestModel(t, testDash(), false)
	press(t, m3, "a", "p", "p")
	f3.update(func(d *proto.Dash) { d.View.Hosts[0].Inst = "9:9" })
	press(t, m3, "enter")
	if m3.quitted || len(x3.calls) != 0 || !strings.Contains(m3.note, "restarted since it was listed") {
		t.Fatalf("restarted: note %q", m3.note)
	}
}

func TestPrevAndCurrent(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	press(t, m, "-")
	sw := f.actsOf(proto.OpSwitch)
	if len(sw) != 1 || sw[0].Target.Host != "bbbb" || sw[0].Target.Session != "$0" || sw[0].Client != testClient || sw[0].Nonce == "" {
		t.Fatalf("- hands off to the previous session: %+v", sw)
	}
	m2, _, x2 := newTestModel(t, testDash(), false)
	press(t, m2, ".")
	if c := x2.called("switch-client"); len(c) != 1 || c[0][4] != "$0" {
		t.Fatalf(". goes to the current session: %v", x2.calls)
	}
	// With a query they type.
	m3, f3, _ := newTestModel(t, testDash(), false)
	press(t, m3, "b", "-", ".")
	if string(m3.query) != "b-." || len(f3.actsOf(proto.OpSwitch)) != 0 {
		t.Fatalf("query %q", string(m3.query))
	}
}

func TestWindows(t *testing.T) {
	m, _, x := newTestModel(t, testDash(), false)
	press(t, m, "b", "r", "ctrl+w")
	if got := names(m); !slices.Equal(got, []string{"1:fish", "2:nvim"}) || cursorText(m) != "1:fish" {
		t.Fatalf("windows: %q on %q", got, cursorText(m))
	}
	if l, _ := m.promptText(); l != "windows B:bravo> " {
		t.Fatalf("prompt %q", l)
	}
	press(t, m, "ctrl+w")
	if string(m.query) != "br" || cursorText(m) != "B bravo" {
		t.Fatalf("^w back: query %q cursor %q", string(m.query), cursorText(m))
	}
	// ⏎ on a window of this server: switch-client, then select-window.
	press(t, m, "ctrl+c", "a", "l", "ctrl+w", "enter")
	want := []string{"switch-client", "-c", "/dev/ttys042", "-t", "$0", ";", "select-window", "-t", "@0"}
	if c := x.called("switch-client"); len(c) != 1 || !slices.Equal(c[0], want) || !m.quitted {
		t.Fatalf("calls %v", x.calls)
	}
}

func TestHandoff(t *testing.T) {
	tty := filepath.Join(t.TempDir(), "tty")
	os.WriteFile(tty, nil, 0o600)
	m, f, x := newTestModel(t, testDash(), false)
	x.out["display-message"] = tty + "\t$0\t@0\n"
	press(t, m, "b", "a", "n", "enter")
	sw := f.actsOf(proto.OpSwitch)
	if len(sw) != 1 || sw[0].Target.Session != "$4" || sw[0].Target.Inst != "2:2" || sw[0].Deadline == 0 || sw[0].ID == "" {
		t.Fatalf("switch %+v", sw)
	}
	want := []string{"detach-client", "-t", "/dev/ttys042", "-E", "exit 42"}
	if c := x.called("detach-client"); len(c) != 1 || !slices.Equal(c[0], want) {
		t.Fatalf("no loop ended the client: the dashboard detaches it: %v", x.calls)
	}
	if b, _ := os.ReadFile(tty); string(b) != relay.SyncBegin {
		t.Fatalf("frame hold on the client's tty: %q", b)
	}
	if !m.quitted {
		t.Fatal("the dashboard closes after a hand-off")
	}

	// The detach fails and the client is still there: release the hold.
	os.WriteFile(tty, nil, 0o600)
	m2, _, x2 := newTestModel(t, testDash(), false)
	x2.out["display-message"] = tty + "\t$0\t@0\n"
	x2.err["detach-client"] = errTest
	m2.c.Client = itoa(os.Getpid()) + ":1:/dev/ttys042"
	press(t, m2, "b", "a", "n", "enter")
	if b, _ := os.ReadFile(tty); string(b) != relay.SyncBegin+relay.SyncEnd || m2.quitted || !m2.noteErr {
		t.Fatalf("failed detach: tty %q quit %v note %q", b, m2.quitted, m2.note)
	}

	// TOWER_TEST_NOTTY: no hold, the detach all the same.
	os.WriteFile(tty, nil, 0o600)
	t.Setenv("TOWER_TEST_NOTTY", "1")
	m3, _, x3 := newTestModel(t, testDash(), false)
	x3.out["display-message"] = tty + "\t$0\t@0\n"
	press(t, m3, "b", "a", "n", "enter")
	if b, _ := os.ReadFile(tty); len(b) != 0 || len(x3.called("detach-client")) != 1 {
		t.Fatalf("notty: tty %q calls %v", b, x3.calls)
	}
}

func TestHandoffEnded(t *testing.T) {
	m, f, x := newTestModel(t, testDash(), false)
	f.setAnswer(func(proto.Request) proto.Ack { return proto.Ack{OK: true, Ended: true} })
	// The client is a process that ends 100ms after the answer.
	p, err := os.StartProcess("/bin/sleep", []string{"sleep", "0.1"}, &os.ProcAttr{})
	if err != nil {
		t.Fatal(err)
	}
	go p.Wait()
	m.c.Client = itoa(p.Pid) + ":1:/dev/ttys042"
	start := time.Now()
	press(t, m, "b", "a", "n", "enter")
	if len(x.called("detach-client")) != 0 || !m.quitted {
		t.Fatalf("ended: the loop ends the client, not the dashboard: %v", x.calls)
	}
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Fatalf("the dashboard waits for its client to go: returned after %v", d)
	}
}

func TestHandoffNeedsLoop(t *testing.T) {
	d := testDash()
	d.Owned, d.Loop = false, ""
	m, f, x := newTestModel(t, d, false)
	press(t, m, "b", "a", "n", "enter")
	if m.note != errNeedsLoop.Error() || len(f.actsOf(proto.OpSwitch)) != 0 || len(x.called("detach-client")) != 0 {
		t.Fatalf("note %q", m.note)
	}
	// A local switch still works.
	press(t, m, "ctrl+c", "a", "p", "p", "enter")
	if len(x.called("switch-client")) != 1 {
		t.Fatalf("local switch from a client no loop owns: %v", x.calls)
	}
}

func TestEnterRefusesUnreachable(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	press(t, m, "c", "h", "a", "r", "enter")
	if m.note != "C is down: timed out" || len(f.actsOf(proto.OpSwitch)) != 0 {
		t.Fatalf("note %q", m.note)
	}
	press(t, m, "ctrl+x")
	if m.note != "C is down: timed out" || len(f.actsOf(proto.OpKill)) != 0 {
		t.Fatalf("kill on a down host: %q", m.note)
	}
}

func TestPrompts(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	f.setAnswer(func(r proto.Request) proto.Ack {
		switch r.Op {
		case proto.OpRename:
			f.update(func(d *proto.Dash) { d.View.Hosts[1].Sessions[0].Name = r.Name })
		case proto.OpNew:
			ref := proto.Ref{Host: r.Target.Host, Inst: r.Target.Inst, Session: "$7"}
			f.update(func(d *proto.Dash) {
				h := d.View.HostByID(r.Target.Host)
				h.Sessions = append(h.Sessions, proto.Session{ID: "$7", Name: r.Name, Ago: 5_000_000})
			})
			return proto.Ack{OK: true, Ref: &ref, Note: "starting tmux on D…"}
		}
		return proto.Ack{OK: true}
	})
	press(t, m, "b", "r", "ctrl+r")
	if l, txt := m.promptText(); l != "rename B:bravo> " || txt != "bravo" {
		t.Fatalf("prompt %q %q", l, txt)
	}
	press(t, m, "ctrl+u", "b", "r", "a", "v", "e", "enter")
	r := f.actsOf(proto.OpRename)
	if len(r) != 1 || r[0].Name != "brave" || r[0].Kind != proto.KindSession || r[0].Target.Session != "$0" {
		t.Fatalf("rename %+v", r)
	}
	if m.note != "rename on B: done" || !slices.Contains(names(m), "B brave") || cursorText(m) != "B brave" {
		t.Fatalf("after rename: note %q rows %q cursor %q", m.note, names(m), cursorText(m))
	}
	// ^n on D's empty row: a new session there, and the cursor on it.
	press(t, m, "ctrl+c", "D", "ctrl+n")
	if l, _ := m.promptText(); l != "new session on D> " {
		t.Fatalf("prompt %q", l)
	}
	cmd := pressNoRun(m, "enter")
	if m.note != "starting tmux on D…" {
		t.Fatalf("a host with no server: %q", m.note)
	}
	drive(t, m, cmd)
	n := f.actsOf(proto.OpNew)
	if len(n) != 1 || n[0].Target.Host != "dddd" || n[0].Name != "" {
		t.Fatalf("new %+v", n)
	}
	if m.note != "new on D: done (starting tmux on D…)" || cursorText(m) != "D " {
		t.Fatalf("after new: note %q, cursor on %q, want D's new session", m.note, cursorText(m))
	}
	// esc leaves a prompt without acting.
	press(t, m, "ctrl+r", "x", "esc")
	if m.mode != modeList || len(f.actsOf(proto.OpRename)) != 1 || m.quitted {
		t.Fatal("esc in a prompt cancels it")
	}
}

func TestPreview(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	f.setAnswer(func(r proto.Request) proto.Ack {
		return proto.Ack{OK: true, Text: "pane of " + r.Target.Host + r.Target.Session + r.Target.Window}
	})
	drive(t, m, m.Init())
	header, body := m.preview(10, 100)
	if !strings.Contains(header, "A:alpha") || len(body) != 1 || !strings.Contains(body[0], "pane of aaaa$0@0") {
		t.Fatalf("preview %q %q", header, body)
	}
	// Moving shows the header at once; the capture follows.
	cmd := pressNoRun(m, "down")
	header, body = m.preview(10, 100)
	if !strings.Contains(header, "B:bravo") || !strings.Contains(header, "2:nvim") || body != nil {
		t.Fatalf("before the capture: %q %q", header, body)
	}
	// A capture for a row no longer selected is dropped, and the selected
	// row's asked for.
	pressNoRun(m, "down")
	drive(t, m, cmd)
	if _, body = m.preview(10, 100); len(body) != 1 || !strings.Contains(body[0], "pane of bbbb$4@5") {
		t.Fatalf("stale capture shown: %q", body)
	}
	caps := f.actsOf(proto.OpCapture)
	if len(caps) != 3 {
		t.Fatalf("%d captures, want 3", len(caps))
	}
}

func TestLive(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), true)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	// A change after a quiet spell is read at once.
	cmd := m.gotWatch(watchMsg{gen: 2})
	if !m.reading {
		t.Fatal("a change after a quiet spell is read at once")
	}
	// Another within 50ms waits for the read in flight, then the pace.
	now = now.Add(10 * time.Millisecond)
	m.gotWatch(watchMsg{gen: 3})
	if !m.dirty {
		t.Fatal("a change during a read is read after it")
	}
	f.update(func(d *proto.Dash) { d.View.Hosts[0].Sessions[0].Name = "aleph" })
	_ = cmd
	m.reading = false
	m.dirty = false
	m.gotWatch(watchMsg{gen: 4})
	if m.reading || !m.ticking {
		t.Fatal("within 50ms of the last read, the next waits on a timer")
	}
	// The real thing: the watch wakes, the rows follow without a key.
	m2, f2, _ := newTestModel(t, testDash(), true)
	done := make(chan struct{})
	go func() {
		drive(t, m2, m2.watch())
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	f2.update(func(d *proto.Dash) { d.View.Hosts[0].Sessions[0].Name = "aleph" })
	<-done
	if names(m2)[0] != "A aleph" {
		t.Fatalf("live: %q", names(m2))
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
