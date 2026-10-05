package ui

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/hosts"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/transport"
)

// fakeHosts is a host list in memory whose checks pass unless the host
// is named in fail.
type fakeHosts struct {
	slow    time.Duration // Add takes this long, past its context (an install runs on its own)
	mu      sync.Mutex
	list    []config.Host
	aliases []string
	fail    map[string]string
	edits   []string
}

func (f *fakeHosts) Load() ([]config.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.list), nil
}

func (f *fakeHosts) Aliases() []string { return f.aliases }

func (f *fakeHosts) Check(ctx context.Context, h config.Host, step func(hosts.Step)) []transport.Check {
	step(hosts.Step{Check: transport.Check{Name: "ssh", Detail: "connecting"}, Running: true})
	if why, ok := f.fail[h.Target()]; ok {
		c := []transport.Check{{Name: "ssh", OK: true}, {Name: "tmux", Detail: why}}
		step(hosts.Step{Check: c[1]})
		return c
	}
	return []transport.Check{{Name: "ssh", OK: true}, {Name: "tmux", OK: true}, {Name: "os", OK: true}, {Name: "tower", OK: true}}
}

func (f *fakeHosts) Add(ctx context.Context, h config.Host, step func(hosts.Step)) ([]transport.Check, error) {
	c := f.Check(ctx, h, step)
	time.Sleep(f.slow)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = append(f.list, h)
	f.edits = append(f.edits, "add "+h.Name+" "+h.Target())
	return c, nil
}

func (f *fakeHosts) edit(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, s)
	return nil
}

func (f *fakeHosts) Remove(name string) error      { return f.edit("rm " + name) }
func (f *fakeHosts) Rename(old, name string) error { return f.edit("rename " + old + " " + name) }
func (f *fakeHosts) SetOn(name string, on bool) error {
	if on {
		return f.edit("on " + name)
	}
	return f.edit("off " + name)
}

func TestAddHost(t *testing.T) {
	m, _, _ := newTestModel(t, testDash(), false)
	fh := &fakeHosts{list: []config.Host{{Name: "B"}, {Name: "C"}, {Name: "D"}}, aliases: []string{"B", "workbox", "nas", "gpu-node"},
		fail: map[string]string{"nas": "tmux is not installed on nas"}}
	m.c.Hosts = fh
	press(t, m, "ctrl+l", "a")
	if m.mode != modeAddHost || !slices.Equal(m.picker.cands, []string{"workbox", "nas", "gpu-node"}) {
		t.Fatalf("picker: %+v", m.picker)
	}
	// Typing filters; a query that is not an alias adds a raw row.
	press(t, m, "n", "a")
	if !slices.Equal(m.picker.shown, []string{"nas"}) || !m.picker.raw {
		t.Fatalf("na: %+v", m.picker)
	}
	// An alias whose checks fail: added, marked, the reason shown.
	press(t, m, "enter")
	drive(t, m, nil)
	c := m.checks["nas"]
	if c == nil || c.running || !c.failed() || !strings.Contains(m.note.text, "tmux is not installed on nas") {
		t.Fatalf("nas: %+v note %q", c, m.note.text)
	}
	if !slices.Contains(fh.edits, "add nas nas") {
		t.Fatalf("edits %q", fh.edits)
	}
	// A raw target asks for a name: empty takes the default shown.
	press(t, m, "a")
	for _, k := range "me@box.lan:2222" {
		press(t, m, string(k))
	}
	press(t, m, "down", "enter")
	if m.mode != modePrompt || m.prompt.hint != "⏎ for box" {
		t.Fatalf("raw: mode %v prompt %+v", m.mode, m.prompt)
	}
	// Names: unique in any case; an alias's name is its host's only.
	press(t, m, "b", "enter")
	if m.mode != modePrompt || !strings.Contains(m.prompt.err, "already exists") {
		t.Fatalf("taken: %+v", m.prompt)
	}
	press(t, m, "ctrl+u", "w", "o", "r", "k", "b", "o", "x", "enter")
	if m.mode != modePrompt || !strings.Contains(m.prompt.err, "ssh alias for another host") {
		t.Fatalf("an alias's name: %+v", m.prompt)
	}
	press(t, m, "ctrl+u", "enter")
	if !slices.Contains(fh.edits, "add box me@box.lan:2222") {
		t.Fatalf("edits %q", fh.edits)
	}
}

// A host whose checks end after their 15s (an install runs on a context
// of its own) still gets its result: the row stops checking.
func TestAddHostSlowerThanTheChecks(t *testing.T) {
	old := checkWait
	checkWait = 20 * time.Millisecond
	defer func() { checkWait = old }()
	for i := range 10 {
		m, _, _ := newTestModel(t, testDash(), false)
		fh := &fakeHosts{list: []config.Host{{Name: "B"}, {Name: "C"}, {Name: "D"}}, aliases: []string{"workbox"}, slow: 80 * time.Millisecond}
		m.c.Hosts = fh
		press(t, m, "ctrl+l", "a", "w", "o", "r", "k", "enter")
		drive(t, m, nil)
		if c := m.checks["workbox"]; c == nil || c.running {
			t.Fatalf("run %d: the row is still checking: %+v", i, c)
		}
	}
}

func TestHostEdits(t *testing.T) {
	m, _, _ := newTestModel(t, testDash(), false)
	fh := &fakeHosts{list: []config.Host{{Name: "B"}, {Name: "C"}, {Name: "D"}}}
	m.c.Hosts = fh
	press(t, m, "ctrl+l", "h")
	// The local server is always listed.
	press(t, m, "x")
	if m.mode != modeNormal || !strings.Contains(m.note.text, "always listed") {
		t.Fatalf("x on the local server: %q", m.note.text)
	}
	press(t, m, "j", "x")
	if m.mode != modeConfirm || m.confirm.text() != "remove host B from the list? its sessions keep running" {
		t.Fatalf("remove: %+v", m.confirm)
	}
	press(t, m, "y")
	press(t, m, "space")
	press(t, m, "r", "ctrl+u", "b", "e", "e", "enter")
	if want := []string{"rm B", "off B", "rename B bee"}; !slices.Equal(fh.edits, want) {
		t.Fatalf("edits %q, want %q", fh.edits, want)
	}
	// Not on the home: hosts are not edited here.
	d := testDash()
	d.Self = "bbbb"
	m2, _, _ := newTestModel(t, d, false)
	m2.c.Hosts = fh
	press(t, m2, "ctrl+l", "a")
	if m2.mode == modeAddHost || !strings.Contains(m2.note.text, "home's machine") {
		t.Fatalf("a on a remote: %q", m2.note.text)
	}
}

func TestSearchMode(t *testing.T) {
	m, _, _ := newTestModel(t, testDash(), false)
	press(t, m, "ctrl+l", "h", "/", "b")
	if m.mode != modeSearch || m.curHost().host.Name != "B" {
		t.Fatalf("search hosts: %v %q", m.mode, m.curHost().host.Name)
	}
	// tab: the next column with its own query; ^c clears it, esc keeps
	// every query.
	press(t, m, "tab", "n", "a")
	if m.focus != colSessions || m.curEntry().sess.Name != "banana" {
		t.Fatalf("search sessions: %v", m.curEntry())
	}
	press(t, m, "left", "x")
	if q := m.cs[colSessions].in.String(); q != "nxa" {
		t.Fatalf("text cursor: %q", q)
	}
	press(t, m, "ctrl+c")
	if len(m.cs[colSessions].in.text) != 0 || m.mode != modeSearch {
		t.Fatal("^c clears the column's query and stays")
	}
	press(t, m, "esc")
	if m.mode != modeNormal || m.cs[colHosts].in.String() != "b" {
		t.Fatalf("esc keeps queries: %q", m.cs[colHosts].in.String())
	}
	// Host counts become matches while the sessions column has a query.
	press(t, m, "/", "a", "p", "p", "esc")
	if s, st := m.hostRight(m.w.host("A")); s != "1" || st != sPlain {
		t.Fatalf("A's count with a sessions query: %q", s)
	}
	if s := screen(m); !strings.Contains(s, "no match · esc clears") {
		t.Fatalf("a query hiding every row says so:\n%s", s)
	}
}

func TestMouse(t *testing.T) {
	m, f, _ := newTestModel(t, testDash(), false)
	m.width, m.height = 120, 30
	screen(m) // lays it out
	// A click selects; a second within 400ms is ⏎.
	g := m.geo
	now := time.Unix(100, 0)
	m.now = func() time.Time { return now }
	press1 := func(x, y int) {
		_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		drive(t, m, cmd)
	}
	press1(4, g.bodyY+2)
	if cursorText(m) != "B  banana" {
		t.Fatalf("click: %q", cursorText(m))
	}
	now = now.Add(200 * time.Millisecond)
	press1(4, g.bodyY+2)
	if sw := f.actsOf(proto.OpSwitch); len(sw) != 1 || sw[0].Target.Session != "$4" {
		t.Fatalf("double click: %+v", sw)
	}
	// The wheel moves the column under the pointer, not the focus.
	m2, _, _ := newTestModel(t, testDash(), false)
	m2.width, m2.height = 120, 30
	press(t, m2, "ctrl+l")
	screen(m2)
	_, cmd := m2.Update(tea.MouseWheelMsg{X: m2.geo.cols[0].x + 2, Y: m2.geo.bodyY, Button: tea.MouseWheelDown})
	drive(t, m2, cmd)
	if m2.focus != colSessions || m2.curHost().host.Name != "B" {
		t.Fatalf("wheel: focus %v host %q", m2.focus, m2.curHost().host.Name)
	}
}

func TestHelp(t *testing.T) {
	m, _, _ := newTestModel(t, testDash(), false)
	m.width, m.height = 130, 40
	press(t, m, "?")
	if s := screen(m); m.mode != modeHelp || !strings.Contains(s, "any key  closes") || !strings.Contains(s, "find on every host") {
		t.Fatalf("help:\n%s", s)
	}
	press(t, m, "x")
	if m.mode != modeFind || m.quitted {
		t.Fatal("any key closes help, and does nothing else")
	}
}
