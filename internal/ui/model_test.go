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

func TestFinderRows(t *testing.T) {
	m, _, _ := newTestModel(t, testDash(), false)
	want := []string{"A  alpha", "B  bravo", "B  banana", "A  apple", "C  charlie"}
	if got := names(m); !slices.Equal(got, want) {
		t.Fatalf("rows %q, want %q (reachable first, then by recency across hosts)", got, want)
	}
	if m.mode != modeFind || !m.find.front || cursorText(m) != "A  alpha" {
		t.Fatalf("the dashboard opens in the finder on the top row: mode %v cursor %q", m.mode, cursorText(m))
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

func TestFinderQuery(t *testing.T) {
	m, _, _ := newTestModel(t, testDash(), false)
	press(t, m, "b", "a", "n")
	if got := names(m); !slices.Equal(got, []string{"B  banana"}) {
		t.Fatalf("ban: %q", got)
	}
	press(t, m, "backspace", "backspace", "backspace", "B", " ", "b", "R")
	if got := names(m); got[0] != "B  bravo" {
		t.Fatalf("B bR (host word, case ignored): %q", got)
	}
	press(t, m, "ctrl+c")
	if len(m.find.in.text) != 0 || len(names(m)) != 5 || m.quitted {
		t.Fatalf("^c clears the query first: %q quit=%v", m.find.in.String(), m.quitted)
	}
	press(t, m, "ctrl+c")
	if !m.quitted {
		t.Fatal("^c on an empty query quits where the dashboard opened")
	}
}

func TestFinderWindows(t *testing.T) {
	d := testDash()
	b := &d.View.Hosts[1]
	b.Sessions[0].Windows = append(b.Sessions[0].Windows,
		proto.Window{ID: "@8", Index: 3, Name: "tensorboard"}, proto.Window{ID: "@9", Index: 4, Name: "train"})
	b.Sessions[1].Windows = append(b.Sessions[1].Windows, proto.Window{ID: "@10", Index: 2, Name: "train-eval"})
	m, _, _ := newTestModel(t, d, false)
	// A window name: listed under its session, folded when the session's
	// own name does not match; the cursor on the best row.
	press(t, m, "t", "e", "n", "s")
	if got := names(m); !slices.Equal(got, []string{"B  bravo › 3:tensorboard"}) || cursorText(m) != got[0] {
		t.Fatalf("tens: %q on %q", got, cursorText(m))
	}
	press(t, m, "ctrl+u", "t", "r", "a", "i", "n")
	if got := names(m); !slices.Equal(got, []string{"B  bravo › 4:train", "B  banana › 2:train-eval"}) {
		t.Fatalf("train: %q", got)
	}
	// Session and window words in any order; a number next to ':' is a
	// window by its number.
	press(t, m, "ctrl+u")
	for _, k := range "nvim bravo" {
		press(t, m, string(k))
	}
	if got := names(m); !slices.Equal(got, []string{"B  bravo", "    └ 2:nvim"}) || cursorText(m) != "    └ 2:nvim" {
		t.Fatalf("nvim bravo: %q on %q", got, cursorText(m))
	}
	press(t, m, "ctrl+u")
	for _, k := range "B:bravo:3" {
		press(t, m, string(k))
	}
	if cursorText(m) != "    └ 3:tensorboard" {
		t.Fatalf("B:bravo:3: %q on %q", names(m), cursorText(m))
	}
	// tab shows every window of the session, the others dimmed.
	press(t, m, "tab")
	if got := names(m); len(got) != 5 || !m.find.rows[1].dim || m.find.rows[3].dim {
		t.Fatalf("tab: %q", got)
	}
	press(t, m, "tab")
	if got := names(m); len(got) != 2 {
		t.Fatalf("tab again folds: %q", got)
	}
}

func TestFinderRanking(t *testing.T) {
	d := testDash()
	a := &d.View.Hosts[0]
	a.Sessions = []proto.Session{
		{ID: "$1", Name: "s150", Ago: 10},
		{ID: "$2", Name: "s50", Ago: 20},
		{ID: "$3", Name: "cobra", Ago: 30},
		{ID: "$4", Name: "bravo-logs", Ago: 40},
	}
	m, _, _ := newTestModel(t, d, false)
	for q, want := range map[string]string{
		"s50":   "A  s50",
		"bra":   "A  bravo-logs", // a name's start, the most recent first, before inside a word
		"blogs": "A  bravo-logs",
		"cbr":   "A  cobra",
	} {
		press(t, m, "ctrl+u")
		for _, k := range q {
			press(t, m, string(k))
		}
		if cursorText(m) != want {
			t.Errorf("%s: cursor on %q, want %q (rows %q)", q, cursorText(m), want, names(m))
		}
	}
}

func TestFinderCursorKeepsItsRow(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	press(t, m, "down", "down")
	if cursorText(m) != "B  banana" {
		t.Fatalf("cursor on %q", cursorText(m))
	}
	// New sessions above it and a reorder: the cursor stays on banana.
	f.update(func(d *proto.Dash) {
		b := &d.View.Hosts[1]
		b.Sessions = append(b.Sessions, proto.Session{ID: "$9", Name: "blueberry", Ago: 0})
		b.Sessions[1].Ago = 2_000 // banana now more recent than bravo
	})
	drive(t, m, m.read())
	if got := names(m)[:3]; !slices.Equal(got, []string{"B  blueberry", "A  alpha", "B  banana"}) {
		t.Fatalf("rows %q", got)
	}
	if cursorText(m) != "B  banana" {
		t.Fatalf("after the update the cursor is on %q, want banana", cursorText(m))
	}
	// banana gone: the cursor goes to the row that followed it.
	f.update(func(d *proto.Dash) {
		b := &d.View.Hosts[1]
		b.Sessions = slices.DeleteFunc(b.Sessions, func(s proto.Session) bool { return s.Name == "banana" })
	})
	drive(t, m, m.read())
	if cursorText(m) != "B  bravo" {
		t.Fatalf("banana gone: cursor on %q, want its neighbour bravo", cursorText(m))
	}
	// A typed query puts the cursor on the best match.
	press(t, m, "a", "p", "p")
	if cursorText(m) != "A  apple" {
		t.Fatalf("query: cursor on %q", cursorText(m))
	}
}

// killFrom asks a kill with ^x and confirms it, returning the kill's
// command unrun.
func killFrom(t *testing.T, m *Model) func() {
	t.Helper()
	press(t, m, "ctrl+x")
	if m.mode != modeConfirm {
		t.Fatalf("^x asks first: mode %v note %q", m.mode, m.note.text)
	}
	cmd := pressNoRun(m, "y")
	return func() { drive(t, m, cmd) }
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
	// Any key but y keeps it.
	press(t, m, "ctrl+x", "n")
	if m.mode != modeFind || len(f.actsOf(proto.OpKill)) != 0 {
		t.Fatal("n cancels a kill")
	}
	kill := killFrom(t, m)
	if slices.Contains(names(m), "B  bravo") || cursorText(m) != "B  banana" {
		t.Fatalf("y hides the row at once and moves to its neighbour: %q, cursor %q", names(m), cursorText(m))
	}
	// Reloads while the kill is in flight keep it hidden.
	drive(t, m, m.read())
	if slices.Contains(names(m), "B  bravo") {
		t.Fatal("a reload brought back a row whose kill is in flight")
	}
	// The kill fails: the row comes back, with the error.
	go func() { release <- false }()
	kill()
	if !slices.Contains(names(m), "B  bravo") || m.note.kind != noteErr || !strings.Contains(m.note.text, "kill on B: no such session") {
		t.Fatalf("a failed kill brings the row back with the error: %q, note %q", names(m), m.note.text)
	}
	// Three kills in a row, each hidden at once.
	m.find.cursor = m.find.rows[0].key
	var kills []func()
	for range 3 {
		kills = append(kills, killFrom(t, m))
	}
	if got := names(m); !slices.Equal(got, []string{"A  apple", "C  charlie"}) {
		t.Fatalf("after three kills: %q", got)
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
	for _, k := range kills {
		k()
	}
	if got := names(m); !slices.Equal(got, []string{"A  apple", "C  charlie"}) {
		t.Fatalf("after the answers: %q", got)
	}
	if len(m.hidden) != 0 {
		t.Fatalf("hidden set left: %v", m.hidden)
	}
	if n := len(f.actsOf(proto.OpKill)); n != 4 {
		t.Fatalf("%d kills sent, want 4", n)
	}
	if m.note.text != "kill on B: done" {
		t.Fatalf("note %q", m.note.text)
	}
}

func TestKillSurvivesStaleRead(t *testing.T) {
	// The answer arrives while a read that started before it is in
	// flight: that read still shows the session, so the row must stay
	// hidden until the next one.
	m, _, _ := newTestModel(t, testDash(), false)
	killFrom(t, m) // A alpha
	readCmd := m.read()
	m.Update(actMsg{op: "kill", host: "A", key: rowKey{Host: "aaaa", Inst: "1:1", Session: "$0"}, ack: proto.Ack{OK: true}})
	m.Update(readCmd()) // the stale read lands; the session is still in the view
	if slices.Contains(names(m), "A  alpha") {
		t.Fatal("a read started before the answer brought the row back")
	}
	if h := m.hidden[rowKey{Host: "aaaa", Inst: "1:1", Session: "$0"}]; h.clearAt != 2 {
		t.Fatalf("hide %+v, want cleared by read 2", h)
	}
}

func TestKillQuestion(t *testing.T) {
	d := testDash()
	b := &d.View.Hosts[1]
	b.Sessions[1].Group = "banana"
	b.Sessions = append(b.Sessions, proto.Session{ID: "$5", Name: "banana 2", Group: "banana", Ago: 9_000_000,
		Windows: []proto.Window{{ID: "@5", Index: 1, Name: "top", Active: true}}})
	b.Sessions[0].Windows[0].Panes = 3
	m, f, _ := newTestModel(t, d, false)
	f.setAnswer(func(r proto.Request) proto.Ack {
		if r.Op == proto.OpPanes {
			return proto.Ack{OK: true, Panes: []proto.Pane{{ID: "%1", Command: "fish"}, {ID: "%2", Command: "python"},
				{ID: "%3", Command: "watch"}, {ID: "%4", Command: "nvim"}, {ID: "%5", Command: "-zsh"}}}
		}
		return proto.Ack{OK: true}
	})
	press(t, m, "b", "r", "a", "v", "o", "ctrl+x")
	if want := "kill session B:bravo? 2 windows · 4 panes · python, watch +1 running"; m.confirm.text() != want {
		t.Fatalf("question %q, want %q", m.confirm.text(), want)
	}
	press(t, m, "n", "ctrl+u", "b", "a", "n", "a", "n", "a", "ctrl+x")
	if q := m.confirm.text(); !strings.Contains(q, "detaches 2 other clients") || !strings.Contains(q, "grouped: its windows stay with banana 2") {
		t.Fatalf("question %q", q)
	}
	// A grouped session's last window: the group shares it, so every
	// member goes too.
	press(t, m, "n", "tab", "down", "ctrl+x")
	if q := m.confirm.text(); !strings.HasPrefix(q, "kill window B:banana:1:top?") || !strings.Contains(q, "its group goes too (banana, banana 2)") {
		t.Fatalf("question %q", q)
	}
}

func TestEnterOnGoneRow(t *testing.T) {
	m, f, x := newTestModel(t, testDash(), false)
	press(t, m, "a", "p", "p")
	// apple is killed elsewhere; with live updates off the row stays.
	f.update(func(d *proto.Dash) { d.View.Hosts[0].Sessions = d.View.Hosts[0].Sessions[:1] })
	press(t, m, "enter")
	if m.note.text != "selection is gone" || m.quitted || len(x.calls) != 0 {
		t.Fatalf("note %q quit %v tmux %v", m.note.text, m.quitted, x.calls)
	}
	// A renamed session is still reached, by id.
	m2, f2, x2 := newTestModel(t, testDash(), false)
	press(t, m2, "a", "p", "p")
	f2.update(func(d *proto.Dash) { d.View.Hosts[0].Sessions[1].Name = "pear" })
	press(t, m2, "enter")
	if !m2.quitted || !slices.Equal(x2.called("switch-client")[0], []string{"switch-client", "-c", "/dev/ttys042", "-t", "$1"}) {
		t.Fatalf("renamed: quit %v calls %v note %q", m2.quitted, x2.calls, m2.note.text)
	}
	// A server that restarted since the row was drawn.
	m3, f3, x3 := newTestModel(t, testDash(), false)
	press(t, m3, "a", "p", "p")
	f3.update(func(d *proto.Dash) { d.View.Hosts[0].Inst = "9:9" })
	press(t, m3, "enter")
	if m3.quitted || len(x3.calls) != 0 || !strings.Contains(m3.note.text, "restarted since it was listed") {
		t.Fatalf("restarted: note %q", m3.note.text)
	}
}

func TestPrevAndCurrent(t *testing.T) {
	// In the finder, - and . select the previous and the current
	// session; ⏎ then goes there.
	m, f, _ := newTestModel(t, testDash(), false)
	press(t, m, "-")
	if cursorText(m) != "B  bravo" || len(f.actsOf(proto.OpSwitch)) != 0 {
		t.Fatalf("- selects the previous session: %q", cursorText(m))
	}
	press(t, m, "enter")
	sw := f.actsOf(proto.OpSwitch)
	if len(sw) != 1 || sw[0].Target.Host != "bbbb" || sw[0].Target.Session != "$0" || sw[0].Client != testClient || sw[0].Nonce == "" {
		t.Fatalf("⏎ hands off to the previous session: %+v", sw)
	}
	m2, _, x2 := newTestModel(t, testDash(), false)
	press(t, m2, "down", ".", "enter")
	if c := x2.called("switch-client"); len(c) != 1 || c[0][4] != "$0" {
		t.Fatalf(". goes to the current session: %v", x2.calls)
	}
	// With a query they type.
	m3, _, _ := newTestModel(t, testDash(), false)
	press(t, m3, "b", "-", ".")
	if m3.find.in.String() != "b-." {
		t.Fatalf("query %q", m3.find.in.String())
	}
	// In the columns, - selects without attaching, and clears a query
	// that hides it.
	m4, f4, _ := newTestModel(t, testDash(), false)
	press(t, m4, "ctrl+l", "/", "z", "esc", "-")
	if h, e := m4.curHost(), m4.curEntry(); h.host.Name != "B" || e == nil || e.sess.Name != "bravo" || len(f4.actsOf(proto.OpSwitch)) != 0 {
		t.Fatalf("- in the columns: %v %v", h.host.Name, e)
	}
}

func TestColumns(t *testing.T) {
	m, _, x := newTestModel(t, testDash(), false)
	press(t, m, "ctrl+l")
	if m.mode != modeNormal || m.focus != colSessions || m.curHost().host.Name != "A" || m.curEntry().sess.Name != "alpha" {
		t.Fatalf("^l: the columns on the finder's row: mode %v focus %v", m.mode, m.focus)
	}
	// Hosts in their order: reachable by recency, then down, then off.
	var hosts []string
	for _, h := range m.hostList().items {
		hosts = append(hosts, h.host.Name)
	}
	if !slices.Equal(hosts, []string{"A", "B", "D", "C"}) {
		t.Fatalf("hosts %q", hosts)
	}
	// h to the hosts, j to B, l to its sessions: they remember their row.
	press(t, m, "h", "j", "l", "j")
	if e := m.curEntry(); e.sess.Name != "banana" {
		t.Fatalf("B's second session: %q", e.sess.Name)
	}
	press(t, m, "h", "k", "j")
	if e := m.curEntry(); e.sess.Name != "banana" {
		t.Fatalf("back on B: its remembered session, got %q", e.sess.Name)
	}
	// A query keeps filtering as the host changes, without forgetting.
	press(t, m, "l", "/", "b", "r", "esc")
	if e := m.curEntry(); e.sess.Name != "bravo" {
		t.Fatalf("query br on B: %q", e.sess.Name)
	}
	press(t, m, "esc")
	if e := m.curEntry(); e.sess.Name != "banana" || len(m.cs[colSessions].in.text) != 0 {
		t.Fatalf("esc clears the query: back on the remembered %q", e.sess.Name)
	}
	// The windows column: ⏎ on a window of this server.
	press(t, m, "h", "k", "l", "l", "enter")
	want := []string{"switch-client", "-c", "/dev/ttys042", "-t", "$0", ";", "select-window", "-t", "@0"}
	if c := x.called("switch-client"); len(c) != 1 || !slices.Equal(c[0], want) || !m.quitted {
		t.Fatalf("calls %v", x.calls)
	}
	// esc with nothing to clear quits.
	m2, _, _ := newTestModel(t, testDash(), false)
	press(t, m2, "ctrl+l", "esc")
	if !m2.quitted {
		t.Fatal("esc with no query quits")
	}
}

func TestDirs(t *testing.T) {
	d := testDash()
	a := &d.View.Hosts[0]
	a.Dirs = []proto.Dir{
		{Path: "~/src/herdr", Root: true, Git: &proto.Git{Branch: "main", Dirty: true}},
		{Path: "~/notes"},
		{Path: "~/src/alpha", Root: true},
	}
	m, f, x := newTestModel(t, d, false)
	f.setAnswer(func(r proto.Request) proto.Ack {
		if r.Op == proto.OpNew {
			ref := proto.Ref{Host: r.Target.Host, Inst: r.Target.Inst, Session: "$7"}
			f.update(func(d *proto.Dash) {
				h := d.View.HostByID(r.Target.Host)
				h.Sessions = append(h.Sessions, proto.Session{ID: "$7", Name: r.Name, Path: r.Dir})
			})
			return proto.Ack{OK: true, Ref: &ref}
		}
		return proto.Ack{OK: true}
	})
	// Git roots follow the reachable sessions in the finder; ^g lists
	// every entry.
	if got := names(m); !slices.Equal(got[4:], []string{"A  ~/src/herdr", "A  ~/src/alpha", "C  charlie"}) {
		t.Fatalf("rows %q", got)
	}
	press(t, m, "ctrl+g")
	if got := names(m); !slices.Contains(got, "A  ~/notes") {
		t.Fatalf("^g: %q", got)
	}
	press(t, m, "ctrl+g")
	// ⏎ on a dir: a session named after it, made there and attached.
	press(t, m, "h", "e", "r", "d", "enter")
	n := f.actsOf(proto.OpNew)
	if len(n) != 1 || n[0].Name != "herdr" || n[0].Dir != "~/src/herdr" || n[0].Target.Host != "aaaa" {
		t.Fatalf("new %+v", n)
	}
	if c := x.called("switch-client"); len(c) != 1 || c[0][4] != "$7" {
		t.Fatalf("attached: %v", x.calls)
	}
	// A taken name opens the prompt with the first free one.
	m2, f2, _ := newTestModel(t, d, false)
	press(t, m2, "s", "r", "c", "a", "l", "enter")
	if m2.mode != modePrompt || m2.prompt.in.String() != "alpha_2" || !strings.Contains(m2.prompt.err, "alpha is taken") {
		t.Fatalf("taken: mode %v prompt %+v", m2.mode, m2.prompt)
	}
	press(t, m2, "ctrl+u", "a", ".", "b", "enter")
	if n := f2.actsOf(proto.OpNew); len(n) != 1 || n[0].Name != "a_b" || n[0].Dir != "~/src/alpha" {
		t.Fatalf("named: %+v", n)
	}
}

func TestDup(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	f.setAnswer(func(r proto.Request) proto.Ack {
		if r.Op == proto.OpDup {
			ref := proto.Ref{Host: r.Target.Host, Inst: r.Target.Inst, Session: "$8"}
			f.update(func(d *proto.Dash) {
				h := d.View.HostByID(r.Target.Host)
				h.Sessions = append(h.Sessions, proto.Session{ID: "$8", Name: r.Name, Group: "bravo"})
			})
			return proto.Ack{OK: true, Ref: &ref}
		}
		return proto.Ack{OK: true}
	})
	press(t, m, "ctrl+l", "h", "j", "l", "D")
	dup := f.actsOf(proto.OpDup)
	if len(dup) != 1 || dup[0].Name != "bravo 2" || dup[0].Target.Session != "$0" {
		t.Fatalf("dup %+v", dup)
	}
	if sw := f.actsOf(proto.OpSwitch); len(sw) != 1 || sw[0].Target.Session != "$8" || !m.quitted {
		t.Fatalf("attached to the duplicate: %+v", sw)
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
	if b, _ := os.ReadFile(tty); string(b) != relay.SyncBegin+relay.SyncEnd || m2.quitted || m2.note.kind != noteErr {
		t.Fatalf("failed detach: tty %q quit %v note %q", b, m2.quitted, m2.note.text)
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
	if m.note.text != errNeedsLoop.Error() || len(f.actsOf(proto.OpSwitch)) != 0 || len(x.called("detach-client")) != 0 {
		t.Fatalf("note %q", m.note.text)
	}
	press(t, m, "ctrl+c", "a", "p", "p", "enter")
	if len(x.called("switch-client")) != 1 {
		t.Fatalf("local switch from a client no loop owns: %v", x.calls)
	}
}

func TestDownHost(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	// ⏎ on a down host's cached session retries the host now.
	press(t, m, "c", "h", "a", "r", "enter")
	if !strings.Contains(m.note.text, "C is down: timed out") || len(f.actsOf(proto.OpSwitch)) != 0 || f.retries != 1 || f.retried[0] != "C" {
		t.Fatalf("note %q retries %d %v", m.note.text, f.retries, f.retried)
	}
	press(t, m, "ctrl+x")
	if m.note.text != "C is down: timed out" || len(f.actsOf(proto.OpKill)) != 0 || m.mode != modeFind {
		t.Fatalf("kill on a down host: %q", m.note.text)
	}
	// A stalled host is refused, saying so.
	d := testDash()
	d.View.Hosts[1].Status = proto.StatusStalled
	m2, f2, _ := newTestModel(t, d, false)
	press(t, m2, "b", "a", "n", "enter")
	if m2.note.text != "B is not responding" || len(f2.actsOf(proto.OpSwitch)) != 0 {
		t.Fatalf("stalled: %q", m2.note.text)
	}
}

func TestPrompts(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	f.setAnswer(func(r proto.Request) proto.Ack {
		switch r.Op {
		case proto.OpRename:
			f.update(func(d *proto.Dash) { d.View.Hosts[1].Sessions[0].Name = r.Name })
		case proto.OpNew:
			if r.Kind == proto.KindWindow {
				// As towerd answers: the window's ref names its session.
				ref := proto.Ref{Host: r.Target.Host, Inst: r.Target.Inst, Session: r.Target.Session, Window: "@9", Label: r.Name}
				f.update(func(d *proto.Dash) {
					h := d.View.HostByID(r.Target.Host)
					for i := range h.Sessions {
						if h.Sessions[i].ID == r.Target.Session {
							h.Sessions[i].Windows = append(h.Sessions[i].Windows, proto.Window{ID: "@9", Index: 9, Name: r.Name})
						}
					}
				})
				return proto.Ack{OK: true, Ref: &ref}
			}
			ref := proto.Ref{Host: r.Target.Host, Inst: r.Target.Inst, Session: "$7"}
			f.update(func(d *proto.Dash) {
				h := d.View.HostByID(r.Target.Host)
				h.Sessions = append(h.Sessions, proto.Session{ID: "$7", Name: "0", Ago: 5_000_000})
				h.NoServer = false
			})
			return proto.Ack{OK: true, Ref: &ref, Note: "started tmux on D"}
		}
		return proto.Ack{OK: true}
	})
	press(t, m, "b", "r", "a", "v", "o", "ctrl+r")
	if m.mode != modePrompt || m.prompt.in.String() != "bravo" || m.prompt.about != "session B:bravo" {
		t.Fatalf("prompt %+v", m.prompt)
	}
	// A taken name keeps the prompt open; '.' and ':' become '_'.
	press(t, m, "ctrl+u", "b", "a", "n", "a", "n", "a", "enter")
	if m.mode != modePrompt || m.prompt.err != "banana is taken on B" {
		t.Fatalf("taken: %+v", m.prompt)
	}
	press(t, m, "ctrl+u", "b", ".", "r", ":", "enter")
	r := f.actsOf(proto.OpRename)
	if len(r) != 1 || r[0].Name != "b_r_" || r[0].Kind != proto.KindSession || r[0].Target.Session != "$0" {
		t.Fatalf("rename %+v", r)
	}
	if m.note.text != "rename on B: done" || m.w.sessions["B"][0].sess.Name != "b_r_" {
		t.Fatalf("after rename: note %q", m.note.text)
	}
	// n on D (no server): a new session, selected once it is listed.
	press(t, m, "ctrl+l", "h", "j", "j", "n")
	if m.mode != modePrompt || m.prompt.about != "on D in ~" {
		t.Fatalf("prompt %+v", m.prompt)
	}
	cmd := pressNoRun(m, "enter")
	if m.note.text != "starting tmux on D…" {
		t.Fatalf("a host with no server: %q", m.note.text)
	}
	drive(t, m, cmd)
	n := f.actsOf(proto.OpNew)
	if len(n) != 1 || n[0].Target.Host != "dddd" || n[0].Name != "" {
		t.Fatalf("new %+v", n)
	}
	if e := m.curEntry(); m.note.text != "new on D: done (started tmux on D)" || e == nil || e.sess.ID != "$7" || !e.isNew {
		t.Fatalf("after new: note %q, entry %+v", m.note.text, e)
	}
	// esc leaves a prompt without acting.
	press(t, m, "l", "r", "x", "esc")
	if m.mode != modeNormal || len(f.actsOf(proto.OpRename)) != 1 || m.quitted {
		t.Fatal("esc in a prompt cancels it")
	}
	// n in the windows column: a window in the session's directory.
	press(t, m, "h", "k", "k", "l", "l", "n", "w", "enter")
	if n := f.actsOf(proto.OpNew); len(n) != 2 || n[1].Kind != proto.KindWindow || n[1].Name != "w" || n[1].Target.Session != "$0" {
		t.Fatalf("new window %+v", n)
	}
	// The new window is selected once a read has it.
	if w := m.curWindow(); w == nil || w.win.ID != "@9" {
		t.Fatalf("after a new window the cursor is on %+v", w)
	}
}

func TestPreview(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	m.width, m.height = 130, 30
	f.setAnswer(func(r proto.Request) proto.Ack {
		return proto.Ack{OK: true, Text: "pane of " + r.Target.Host + r.Target.Session + r.Target.Window + "\n\n",
			Panes: []proto.Pane{{ID: "%1", Width: 80, Height: 24, Command: "fish", Path: "~/src", Active: true},
				{ID: "%2", Left: 81, Width: 40, Height: 24, Command: "htop"}}}
	})
	drive(t, m, m.Init())
	if s := screen(m); !strings.Contains(s, "pane of aaaa$0@0") || !strings.Contains(s, "%2") || !strings.Contains(s, "2 panes") {
		t.Fatalf("preview:\n%s", s)
	}
	// Moving shows the breadcrumb at once; the capture follows.
	cmd := pressNoRun(m, "down")
	if s := screen(m); !strings.Contains(s, "bravo") || !strings.Contains(s, "1:fish") || strings.Contains(s, "pane of bbbb") {
		t.Fatalf("before the capture:\n%s", s)
	}
	// An answer for a row no longer selected is kept, and the selected
	// row's asked for.
	pressNoRun(m, "down")
	drive(t, m, cmd)
	if s := screen(m); !strings.Contains(s, "pane of bbbb$4@5") {
		t.Fatalf("the selected row's capture:\n%s", s)
	}
	if caps := f.actsOf(proto.OpCapture); len(caps) != 3 {
		t.Fatalf("%d captures, want 3", len(caps))
	}
	// J K in the windows column pick a pane; ⏎ lands on it.
	m2, f2, x2 := newTestModel(t, testDash(), false)
	f2.setAnswer(f.answer)
	drive(t, m2, m2.Init())
	press(t, m2, "ctrl+l", "l", "J")
	if m2.sel.pane[m2.curWindow().key] != "%2" {
		t.Fatalf("J: %v", m2.sel.pane)
	}
	press(t, m2, "enter")
	if c := x2.called("switch-client"); len(c) != 1 || !slices.Contains(c[0], "select-pane") || c[0][len(c[0])-1] != "%2" {
		t.Fatalf("⏎ on a picked pane: %v", x2.calls)
	}
}

func TestLive(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), true)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	cmd := m.gotWatch(watchMsg{gen: 2})
	if !m.reading {
		t.Fatal("a change after a quiet spell is read at once")
	}
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
	m2, f2, _ := newTestModel(t, testDash(), true)
	done := make(chan struct{})
	go func() {
		drive(t, m2, m2.watch())
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	f2.update(func(d *proto.Dash) { d.View.Hosts[0].Sessions[0].Name = "aleph" })
	<-done
	if names(m2)[0] != "A  aleph" {
		t.Fatalf("live: %q", names(m2))
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// D reuses only a duplicate in the session's group: a session that merely
// has the duplicate's name is passed over.
func TestDupName(t *testing.T) {
	h := &proto.Host{Sessions: []proto.Session{
		{ID: "$1", Name: "proj"},
		{ID: "$2", Name: "proj 2"}, // made by hand, not grouped
	}}
	if n, found := dupName(h, &h.Sessions[0]); n != "proj 3" || found {
		t.Fatalf("%q %v", n, found)
	}
	h.Sessions = append(h.Sessions, proto.Session{ID: "$3", Name: "proj 3", Group: "proj"})
	h.Sessions[0].Group = "proj"
	if n, found := dupName(h, &h.Sessions[0]); n != "proj 3" || !found {
		t.Fatalf("the grouped duplicate: %q %v", n, found)
	}
	if got := freeName(h, "proj"); got != "proj_2" {
		t.Fatalf("a dir's session: %q", got)
	}
}
