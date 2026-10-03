package ui

import (
	"context"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jerrykal/tower/internal/proto"
)

// mode is what the list shows and what keys do.
type mode int

const (
	modeList    mode = iota // sessions of every host
	modeWindows             // one session's windows (^w)
	modePrompt              // a line of input for rename or new
)

type promptKind int

const (
	promptRename promptKind = iota
	promptNew
)

// prompt is a line of input asked for an action on one row.
type prompt struct {
	kind  promptKind
	row   row
	label string // "rename B:bravo", "new session on B"
	text  []rune
	back  mode
}

// hide is a row whose kill is in flight: it stays out of every rebuild
// until the answer, and after a successful answer until a view read
// started after it lands (by then the session is gone from the view).
type hide struct {
	clearAt int // 0 while in flight; else the first read seq that clears it
}

// readEvery paces view reads in a burst of changes.
const readEvery = 50 * time.Millisecond

// Model is the dashboard: the last view read, the rows derived from it and
// what the user is doing. Bubble Tea's update loop owns it; calls to
// towerd run as commands and come back as messages.
type Model struct {
	c    *Conn
	ctx  context.Context
	live bool // follow towerd's watch (TOWER_LIVE)
	dash bool // tower dash in the loop's picker: esc means "the last target"
	now  func() time.Time

	view   proto.Dash
	readAt time.Time
	have   bool

	rows   []row   // the mode's rows, hidden ones left out
	shown  []row   // rows the query matches
	hits   [][]int // match positions per shown row
	cols   columns
	cursor rowKey // the row under the cursor, by identity
	at     int    // its index in shown
	top    int    // first shown row on screen
	want   *rowKey

	query     []rune
	mode      mode
	winOf     rowKey // windows mode: the session
	saved     []rune // the list's query, kept while in windows mode
	savedKey  rowKey
	prompt    *prompt
	hidden    map[rowKey]hide
	note      string
	noteErr   bool
	busy      bool // ⏎ in flight
	here      proto.Ref
	width     int
	height    int
	reading   bool
	dirty     bool
	ticking   bool
	lastRead  time.Time
	seq       int // view reads started
	gen       uint64
	capKey    rowKey // the row whose capture is shown
	capWin    string // and the window it is of
	capText   string
	capErr    string
	capBusy   bool
	choice    *proto.Ref
	last      bool
	quitted   bool
	firstDraw func()
}

// newModel is a model over c, starting from view d when it was read
// already (have).
func newModel(ctx context.Context, c *Conn, d proto.Dash, have bool, live bool) *Model {
	m := &Model{c: c, ctx: ctx, live: live, now: time.Now, hidden: map[rowKey]hide{}, width: 80, height: 24}
	if have {
		m.view, m.have, m.readAt, m.gen = d, true, m.now(), d.Gen
		m.rebuild()
		m.toTop()
	}
	return m
}

// Messages from commands.
type (
	viewMsg struct {
		seq  int
		dash proto.Dash
		err  error
	}
	watchMsg struct {
		gen uint64
		err error
	}
	readTickMsg  struct{}
	watchRetryMsg struct{}
	actMsg       struct {
		op   string
		host string
		key  rowKey
		ack  proto.Ack
		err  error
	}
	captureMsg struct {
		key  rowKey
		win  string
		text string
		err  error
	}
	infoMsg  struct{ info *clientInfo }
	enterMsg struct {
		out outcome
		ref proto.Ref
		err error
	}
)

func (m *Model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if !m.have {
		cmds = append(cmds, m.read())
	}
	if m.live {
		cmds = append(cmds, m.watch())
	}
	if m.c.Client != "" && m.c.Tmux != nil && !m.c.Pick {
		cmds = append(cmds, m.readInfo())
	}
	cmds = append(cmds, m.wantCapture())
	return tea.Batch(cmds...)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.scroll()
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case viewMsg:
		return m, m.gotView(msg)
	case watchMsg:
		return m, m.gotWatch(msg)
	case readTickMsg:
		m.ticking = false
		return m, m.read()
	case watchRetryMsg:
		return m, m.watch()
	case actMsg:
		return m, m.gotAct(msg)
	case captureMsg:
		return m, m.gotCapture(msg)
	case infoMsg:
		m.here = proto.Ref{Host: m.view.Self, Session: msg.info.session, Window: msg.info.window}
		m.rebuild()
	case enterMsg:
		m.busy = false
		if msg.err != nil {
			m.setErr(msg.err.Error())
			return m, nil
		}
		if msg.out == outChoice {
			ref := msg.ref
			m.choice = &ref
		}
		return m, m.quit()
	}
	return m, nil
}

func (m *Model) quit() tea.Cmd {
	m.quitted = true
	return tea.Quit
}

func (m *Model) setNote(s string) { m.note, m.noteErr = s, false }
func (m *Model) setErr(s string)  { m.note, m.noteErr = s, true }

// --- view reads and the watch ---

// read starts a view read now, or marks one due when a read is in
// flight: one at a time, so answers land in order.
func (m *Model) read() tea.Cmd {
	if m.reading {
		m.dirty = true
		return nil
	}
	m.reading = true
	m.lastRead = m.now()
	m.seq++
	seq, c, ctx, args := m.seq, m.c, m.ctx, m.c.viewArgs()
	return func() tea.Msg {
		d, err := c.Towerd.View(ctx, args)
		return viewMsg{seq: seq, dash: d, err: err}
	}
}

// paced reads at most once per readEvery: a change after a quiet spell is
// read at once, a burst on a timer.
func (m *Model) paced() tea.Cmd {
	if m.reading {
		m.dirty = true
		return nil
	}
	wait := readEvery - m.now().Sub(m.lastRead)
	if wait <= 0 {
		return m.read()
	}
	if m.ticking {
		return nil
	}
	m.ticking = true
	return tea.Tick(wait, func(time.Time) tea.Msg { return readTickMsg{} })
}

func (m *Model) gotView(msg viewMsg) tea.Cmd {
	m.reading = false
	var cmds []tea.Cmd
	if msg.err != nil {
		if m.ctx.Err() != nil {
			return nil
		}
		m.setErr("towerd: " + msg.err.Error())
	} else {
		m.view, m.have, m.readAt = msg.dash, true, m.now()
		m.gen = max(m.gen, msg.dash.Gen)
		for k, h := range m.hidden {
			if h.clearAt > 0 && msg.seq >= h.clearAt {
				delete(m.hidden, k)
			}
		}
		first := len(m.shown) == 0
		m.rebuild()
		if m.want != nil {
			m.place(*m.want)
			m.want = nil
		} else if first {
			m.toTop()
		}
		cmds = append(cmds, m.wantCapture())
		if msg.dash.Gen < m.gen {
			m.dirty = true
		}
	}
	if m.dirty {
		m.dirty = false
		cmds = append(cmds, m.paced())
	}
	return tea.Batch(cmds...)
}

func (m *Model) watch() tea.Cmd {
	gen, c, ctx := m.gen, m.c, m.ctx
	return func() tea.Msg {
		g, err := c.Towerd.Watch(ctx, gen)
		return watchMsg{gen: g, err: err}
	}
}

func (m *Model) gotWatch(msg watchMsg) tea.Cmd {
	if m.ctx.Err() != nil || m.quitted {
		return nil
	}
	if msg.err != nil {
		// towerd restarting (an upgrade) or gone: try again shortly.
		return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return watchRetryMsg{} })
	}
	var read tea.Cmd
	if msg.gen != m.view.Gen {
		m.gen = max(m.gen, msg.gen)
		if msg.gen < m.view.Gen {
			m.gen = msg.gen // towerd restarted: its generations start again
		}
		read = m.paced()
	}
	return tea.Batch(read, m.watch())
}

func (m *Model) readInfo() tea.Cmd {
	c, ctx := m.c, m.ctx
	return func() tea.Msg {
		info, err := c.clientInfo(ctx)
		if err != nil {
			return nil
		}
		return infoMsg{info: info}
	}
}

// --- rows and the cursor ---

func (m *Model) loopID() string {
	if m.view.Loop != "" {
		return m.view.Loop
	}
	return m.c.Loop
}

// rebuild derives the rows from the view, leaves hidden rows out and
// filters by the query, keeping the cursor on its row.
func (m *Model) rebuild() {
	if !m.have {
		return
	}
	mk := marksFor(&m.view, m.loopID(), m.here)
	var all []row
	switch m.mode {
	case modeWindows:
		all = windowRows(&m.view, m.winOf, mk)
	case modePrompt:
		if m.prompt.back == modeWindows {
			all = windowRows(&m.view, m.winOf, mk)
		} else {
			all = sessionRows(&m.view, mk, m.now().Sub(m.readAt))
		}
	default:
		all = sessionRows(&m.view, mk, m.now().Sub(m.readAt))
	}
	m.rows = m.rows[:0]
	for _, r := range all {
		if _, hid := m.hidden[r.key]; !hid {
			m.rows = append(m.rows, r)
		}
	}
	m.cols = layout(m.rows)
	m.refilter()
}

// refilter matches the rows against the query, best matches first (see
// rank), and puts the cursor back on its row; when that row is gone, on
// the nearest row after it that is still there, else before it.
func (m *Model) refilter() {
	old, oldAt := m.shown, m.at
	type hit struct {
		r   row
		s   score
		pos []int
	}
	q := fold(m.query)
	var hits []hit
	for _, r := range m.rows {
		if s, pos, ok := rank(q, r.text, r.nameAt); ok {
			hits = append(hits, hit{r, s, pos})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int {
		switch {
		case a.s.less(b.s):
			return -1
		case b.s.less(a.s):
			return 1
		}
		return 0
	})
	m.shown, m.hits = make([]row, len(hits)), make([][]int, len(hits))
	for i, h := range hits {
		m.shown[i], m.hits[i] = h.r, h.pos
	}
	if i := m.index(m.cursor); i >= 0 {
		m.at = i
	} else {
		m.at = 0
		found := false
		for i := oldAt + 1; i < len(old) && !found; i++ {
			if j := m.index(old[i].key); j >= 0 {
				m.at, found = j, true
			}
		}
		for i := min(oldAt, len(old)) - 1; i >= 0 && !found; i-- {
			if j := m.index(old[i].key); j >= 0 {
				m.at, found = j, true
			}
		}
	}
	m.sync()
}

func (m *Model) index(k rowKey) int {
	for i := range m.shown {
		if m.shown[i].key == k {
			return i
		}
	}
	return -1
}

// sync makes the cursor key follow the index.
func (m *Model) sync() {
	if len(m.shown) == 0 {
		m.at = 0
		m.cursor = rowKey{}
		return
	}
	m.at = min(max(m.at, 0), len(m.shown)-1)
	m.cursor = m.shown[m.at].key
	m.scroll()
}

func (m *Model) toTop() {
	m.at = 0
	m.sync()
}

// place puts the cursor on k when it is shown.
func (m *Model) place(k rowKey) {
	if i := m.index(k); i >= 0 {
		m.at = i
		m.sync()
	}
}

func (m *Model) selected() *row {
	if len(m.shown) == 0 {
		return nil
	}
	return &m.shown[m.at]
}

func (m *Model) move(d int) tea.Cmd {
	m.at += d
	m.sync()
	m.setNote("")
	return m.wantCapture()
}

// --- the preview ---

// wantCapture asks for the selected row's pane when it is not the one
// shown. One capture is in flight at a time; its answer is dropped if the
// selection moved meanwhile, and the next one asked then.
func (m *Model) wantCapture() tea.Cmd {
	r := m.selected()
	if r == nil || r.sess == nil || m.capBusy || !r.host.Reachable() {
		return nil
	}
	t := rowTarget(r)
	t.ref.Window = paneWindow(r)
	if r.key == m.capKey && t.ref.Window == m.capWin {
		return nil
	}
	m.capBusy = true
	c, ctx, key := m.c, m.ctx, r.key
	return func() tea.Msg {
		text, err := c.capture(ctx, t)
		return captureMsg{key: key, win: t.ref.Window, text: text, err: err}
	}
}

// paneWindow is the window whose active pane the preview shows: the
// row's window, or its session's active window.
func paneWindow(r *row) string {
	if r.win != nil {
		return r.win.ID
	}
	for _, w := range r.sess.Windows {
		if w.Active {
			return w.ID
		}
	}
	return ""
}

func (m *Model) gotCapture(msg captureMsg) tea.Cmd {
	m.capBusy = false
	if r := m.selected(); r != nil && r.key == msg.key {
		m.capKey, m.capWin, m.capText, m.capErr = msg.key, msg.win, msg.text, ""
		if msg.err != nil {
			m.capText, m.capErr = "", msg.err.Error()
		}
		return nil
	}
	return m.wantCapture()
}

// rowTarget is the target a row names, as drawn.
func rowTarget(r *row) target {
	t := target{host: r.host, sess: r.sess, ref: proto.Ref{Host: r.host.ID, Name: r.host.Name, Inst: r.host.Inst}}
	if r.sess != nil {
		t.ref.Session, t.ref.Label = r.sess.ID, r.sess.Name
	}
	if r.win != nil {
		t.ref.Window = r.win.ID
	}
	return t
}
