package ui

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jerrykal/tower/internal/proto"
)

// A killed session hands the cursor to another session, not to the dir
// after it: the next ⏎ would make a session instead of switching.
func TestKillKeepsCursorOnSessions(t *testing.T) {
	d := testDash()
	d.View.Hosts[0].Dirs = []proto.Dir{{Path: "~/src/herdr", Root: true}}
	m, f, _ := newTestModel(t, d, false)
	f.setAnswer(func(r proto.Request) proto.Ack {
		if r.Op == proto.OpKill {
			f.update(func(d *proto.Dash) {
				a := &d.View.Hosts[0]
				a.Sessions = slices.DeleteFunc(a.Sessions, func(s proto.Session) bool { return s.ID == r.Target.Session })
			})
		}
		return proto.Ack{OK: true}
	})
	press(t, m, "ctrl+l") // the columns, on A alpha
	press(t, m, "j")
	if e := m.curEntry(); e == nil || e.kind != kSession || e.sess.Name != "apple" {
		t.Fatalf("on %+v, want A apple", e)
	}
	killFrom(t, m)()
	if e := m.curEntry(); e == nil || e.kind != kSession || e.sess.Name != "alpha" {
		t.Fatalf("after the kill on %+v, want A alpha", e)
	}
}

// ⏎ or tab on "+N more" opens the session's windows with the cursor on
// the first window the row stood for.
func TestFinderMoreLandsOnHidden(t *testing.T) {
	d := testDash()
	b := &d.View.Hosts[1]
	b.Sessions[0].Windows = nil
	for i := range 6 {
		b.Sessions[0].Windows = append(b.Sessions[0].Windows,
			proto.Window{ID: "@" + strconv.Itoa(20+i), Index: i + 1, Name: "train" + strconv.Itoa(i+1)})
	}
	for _, k := range []string{"enter", "tab"} {
		m, _, _ := newTestModel(t, d, false)
		press(t, m, "b", "r", "a", "v", "o", " ", "t", "r", "a", "i", "n")
		for i := 0; i < 10 && m.find.selected() != nil && m.find.selected().kind != fMore; i++ {
			press(t, m, "down")
		}
		if r := m.find.selected(); r == nil || r.kind != fMore {
			t.Fatalf("no more row: %q", names(m))
		}
		press(t, m, k)
		if c := cursorText(m); c != "    └ 4:train4" {
			t.Fatalf("%s on +N more: cursor %q in %q", k, c, names(m))
		}
	}
}

// D on a duplicate names the next one after the group's origin.
func TestDupOfDup(t *testing.T) {
	h := &proto.Host{Sessions: []proto.Session{
		{ID: "$1", Name: "train", Group: "train"},
		{ID: "$2", Name: "train 2", Group: "train"},
	}}
	if n, found := dupName(h, &h.Sessions[1]); n != "train 3" || found {
		t.Fatalf("a duplicate's duplicate: %q %v", n, found)
	}
	if n, found := dupName(h, &h.Sessions[0]); n != "train 2" || !found {
		t.Fatalf("the origin's: %q %v", n, found)
	}
}

// Killing a grouped session's last window takes every member: their
// rows go at once.
func TestKillGroupsLastWindow(t *testing.T) {
	d := testDash()
	b := &d.View.Hosts[1]
	b.Sessions[1].Group = "banana"
	b.Sessions = append(b.Sessions, proto.Session{ID: "$5", Name: "banana 2", Group: "banana", Ago: 9_000_000,
		Windows: []proto.Window{{ID: "@5", Index: 1, Name: "top", Active: true}}})
	m, f, _ := newTestModel(t, d, false)
	release := make(chan struct{})
	f.setAnswer(func(r proto.Request) proto.Ack {
		if r.Op == proto.OpKill {
			<-release
		}
		return proto.Ack{OK: true}
	})
	press(t, m, "b", "a", "n", "a", "n", "a", "tab", "down")
	if c := cursorText(m); !strings.Contains(c, "1:top") {
		t.Fatalf("cursor %q in %q", c, names(m))
	}
	kill := killFrom(t, m)
	for _, n := range names(m) {
		if strings.Contains(n, "banana") {
			t.Fatalf("a member stayed while its last window is killed: %q", names(m))
		}
	}
	close(release)
	kill()
}

// Keys that do nothing say why.
func TestRefusedKeysSayWhy(t *testing.T) {
	d := testDash()
	d.View.Hosts[0].Dirs = []proto.Dir{{Path: "~/src/herdr", Root: true}}
	m, _, _ := newTestModel(t, d, false)
	press(t, m, "h", "e", "r", "d", "ctrl+l", "3")
	if m.note.text != "a dir has no windows" {
		t.Fatalf("3 on a dir: %q", m.note.text)
	}
	m2, _, _ := newTestModel(t, testDash(), false)
	press(t, m2, "ctrl+l", "3", "/", "z", "z", "enter")
	if m2.note.text != "no window matches · ^c clears" {
		t.Fatalf("⏎ on no window while searching: %q", m2.note.text)
	}
	// esc leaves the search with its query; then esc clears it.
	press(t, m2, "esc", "enter")
	if m2.note.text != "no window matches · esc clears" {
		t.Fatalf("⏎ on no window: %q", m2.note.text)
	}
}

// Too small to draw: only q, esc and ^c act.
func TestTooSmallIgnoresKeys(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	m.width, m.height = 30, 6
	press(t, m, "ctrl+x", "y")
	if m.mode == modeConfirm || len(f.actsOf(proto.OpKill)) != 0 {
		t.Fatal("a key acted on a screen too small to draw")
	}
	press(t, m, "q")
	if !m.quitted {
		t.Fatal("q quits")
	}
}

// esc after g cancels the g; space on a host being checked says so.
func TestPendingAndChecking(t *testing.T) {
	m, _, _ := newTestModel(t, testDash(), false)
	press(t, m, "ctrl+l", "g", "esc")
	if m.quitted || m.pendingG {
		t.Fatal("esc after g quit instead of cancelling it")
	}
	m.checks["B"] = &hostCheck{running: true}
	press(t, m, "h", "h")
	for i := 0; i < 5 && m.curHost() != nil && m.curHost().host.Name != "B"; i++ {
		press(t, m, "j")
	}
	press(t, m, "space")
	if m.note.text != "B is being checked" {
		t.Fatalf("space on a host being checked: %q", m.note.text)
	}
}
