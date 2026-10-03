package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// fakeTowerd serves view, watch and act on a unix socket, speaking
// proto.Call and proto.Reply as towerd does.
type fakeTowerd struct {
	t       *testing.T
	mu      sync.Mutex
	dash    proto.Dash
	changed chan struct{}
	acts    []proto.Request
	views   []proto.ViewArgs
	watches int
	// answer answers an act; nil answers OK.
	answer func(proto.Request) proto.Ack
	client *client.Client
}

func newFakeTowerd(t *testing.T, d proto.Dash) *fakeTowerd {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	e := &config.Env{RunDir: dir, Tag: "t", StateDir: dir}
	l, err := net.Listen("unix", e.Socket())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTowerd{t: t, dash: d, changed: make(chan struct{}), client: &client.Client{Env: e, Version: "test"}}
	if f.dash.Gen == 0 {
		f.dash.Gen = 1
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c, done)
		}
	}()
	return f
}

func (f *fakeTowerd) serve(c net.Conn, done chan struct{}) {
	defer c.Close()
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var call proto.Call
	if err := json.Unmarshal(line, &call); err != nil {
		return
	}
	var reply proto.Reply
	var result any
	switch call.Op {
	case proto.CallView:
		var a proto.ViewArgs
		json.Unmarshal(call.Args, &a)
		f.mu.Lock()
		f.views = append(f.views, a)
		result = f.dash
		f.mu.Unlock()
	case proto.CallWatch:
		var a proto.WatchArgs
		json.Unmarshal(call.Args, &a)
		f.mu.Lock()
		f.watches++
		gen, ch := f.dash.Gen, f.changed
		f.mu.Unlock()
		if a.Gen == gen {
			// Wait for a change, the caller going, or the end of the test.
			gone := make(chan struct{})
			go func() { c.Read(make([]byte, 1)); close(gone) }()
			select {
			case <-ch:
			case <-gone:
				return
			case <-done:
				return
			case <-time.After(20 * time.Second):
			}
			f.mu.Lock()
			gen = f.dash.Gen
			f.mu.Unlock()
		}
		result = proto.WatchResult{Gen: gen}
	case proto.CallAct:
		var r proto.Request
		json.Unmarshal(call.Args, &r)
		f.mu.Lock()
		f.acts = append(f.acts, r)
		answer := f.answer
		f.mu.Unlock()
		a := proto.Ack{ID: r.ID, OK: true}
		if answer != nil {
			a = answer(r)
			a.ID = r.ID
		}
		result = a
	default:
		reply.Err = "unknown op " + call.Op
	}
	if result != nil {
		reply.Result, _ = json.Marshal(result)
	}
	b, _ := json.Marshal(reply)
	c.Write(append(b, '\n'))
}

// update changes the view and wakes watchers.
func (f *fakeTowerd) update(fn func(d *proto.Dash)) {
	f.mu.Lock()
	fn(&f.dash)
	f.dash.Gen++
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeTowerd) get() proto.Dash {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(f.dash)
	var d proto.Dash
	json.Unmarshal(b, &d)
	return d
}

func (f *fakeTowerd) actsOf(op string) []proto.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rs []proto.Request
	for _, r := range f.acts {
		if r.Op == op {
			rs = append(rs, r)
		}
	}
	return rs
}

func (f *fakeTowerd) setAnswer(fn func(proto.Request) proto.Ack) {
	f.mu.Lock()
	f.answer = fn
	f.mu.Unlock()
}

// fakeTmux records commands and answers them from a table.
type fakeTmux struct {
	mu    sync.Mutex
	calls [][]string
	out   map[string]string // first argument → stdout
	err   map[string]error
}

func (x *fakeTmux) Run(ctx context.Context, args ...string) (string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.calls = append(x.calls, append([]string(nil), args...))
	return x.out[args[0]], x.err[args[0]]
}

func (x *fakeTmux) called(cmd string) [][]string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var cs [][]string
	for _, c := range x.calls {
		if c[0] == cmd {
			cs = append(cs, c)
		}
	}
	return cs
}

// The world of most tests: this machine A (towerd id "aaaa") with alpha
// and apple, B up with bravo and banana, C down with a cached session, and
// D up with no sessions. The client 4242 is on A:alpha, owned by loop L,
// whose previous session is B:bravo.
func testDash() proto.Dash {
	return proto.Dash{
		Gen:   1,
		Self:  "aaaa",
		Loop:  "L",
		Home:  "aaaa",
		Owned: true,
		View: proto.View{
			Home: "aaaa",
			Hosts: []proto.Host{
				{ID: "aaaa", Name: "A", Status: proto.StatusLocal, Inst: "1:1", Sessions: []proto.Session{
					{ID: "$0", Name: "alpha", Ago: 1000, Attached: 1, Windows: []proto.Window{{ID: "@0", Index: 1, Name: "fish", Active: true}}},
					{ID: "$1", Name: "apple", Ago: 7_200_000, Windows: []proto.Window{{ID: "@1", Index: 1, Name: "vim", Active: true}}},
				}},
				{ID: "bbbb", Name: "B", Status: proto.StatusUp, Inst: "2:2", Sessions: []proto.Session{
					{ID: "$0", Name: "bravo", Ago: 60_000, Windows: []proto.Window{
						{ID: "@0", Index: 1, Name: "fish", Active: true},
						{ID: "@3", Index: 2, Name: "nvim", Bell: true},
					}},
					{ID: "$4", Name: "banana", Ago: 600_000, Attached: 2, Windows: []proto.Window{{ID: "@5", Index: 1, Name: "top", Active: true}}},
				}},
				{ID: "cccc", Name: "C", Status: proto.StatusDown, Reason: "timed out", Inst: "3:3", Sessions: []proto.Session{
					{ID: "$2", Name: "charlie", Ago: 100, Windows: []proto.Window{{ID: "@2", Index: 0, Name: "sh", Active: true}}},
				}},
				{ID: "dddd", Name: "D", Status: proto.StatusUp, Inst: "4:4", NoServer: true},
			},
			Loops: []proto.Loop{{ID: "L", Gen: 3,
				Cur:  proto.Ref{Host: "aaaa", Inst: "1:1", Session: "$0"},
				Prev: proto.Ref{Host: "bbbb", Inst: "2:2", Session: "$0"}}},
		},
	}
}

const testClient = "4242:1700000000:/dev/ttys042"

// newTestModel is a model over a fake towerd and tmux, with the view read
// already, live updates off unless live.
func newTestModel(t *testing.T, d proto.Dash, live bool) (*Model, *fakeTowerd, *fakeTmux) {
	t.Helper()
	f := newFakeTowerd(t, d)
	x := &fakeTmux{out: map[string]string{}, err: map[string]error{}}
	c := &Conn{Towerd: Calls{C: f.client}, Tmux: x, Client: testClient}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := newModel(ctx, c, f.get(), true, live)
	m.width, m.height = 100, 30
	return m, f, x
}

// drive executes cmd the way Bubble Tea would, feeding each message back to
// the model until nothing is left; a command that has not answered within
// wait (a watch, say) is dropped.
func drive(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	const wait = 300 * time.Millisecond
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		ch := make(chan tea.Msg, 1)
		go func() { ch <- c() }()
		var msg tea.Msg
		select {
		case msg = <-ch:
		case <-time.After(wait):
			continue
		}
		switch msg := msg.(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case tea.QuitMsg:
		default:
			_, next := m.Update(msg)
			queue = append(queue, next)
		}
	}
}

// press sends keys: a string of text, or names such as "enter", "ctrl+x".
func press(t *testing.T, m *Model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		_, cmd := m.Update(keyMsg(k))
		drive(t, m, cmd)
	}
}

// pressNoRun sends a key and returns its command unrun.
func pressNoRun(m *Model, k string) tea.Cmd {
	_, cmd := m.Update(keyMsg(k))
	return cmd
}

func keyMsg(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	}
	if c, ok := strings.CutPrefix(k, "ctrl+"); ok {
		return tea.KeyPressMsg{Code: rune(c[0]), Mod: tea.ModCtrl}
	}
	r := []rune(k)
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

// names are the shown rows as "host session" (or "idx:name").
func names(m *Model) []string {
	var s []string
	for _, r := range m.shown {
		s = append(s, r.text)
	}
	return s
}

func cursorText(m *Model) string {
	if r := m.selected(); r != nil {
		return r.text
	}
	return ""
}

var errTest = errors.New("test failure")
