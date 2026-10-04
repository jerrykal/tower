package ui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jerrykal/tower/internal/proto"
)

// mode is what keys do.
type mode int

const (
	modeNormal  mode = iota // the columns
	modeSearch              // typing filters the focused column
	modeFind                // the finder
	modePrompt              // a line of input: a name
	modeConfirm             // y/n
	modeAddHost             // the add-host picker
	modeHelp                // the key reference
)

// col is a column of the dashboard.
type col int

const (
	colHosts col = iota
	colSessions
	colWindows
)

var colNames = [3]string{"hosts", "sessions", "windows"}

// colState is one column's query (with its text cursor in search mode)
// and its scroll.
type colState struct {
	in  input
	top int
}

// selection is the columns' memory, by identity: the selected host, each
// host's selected session or dir, each session's selected window, and
// each window's picked pane. It changes only when a cursor moves.
type selection struct {
	host   string
	entry  map[string]rowKey
	window map[rowKey]string
	pane   map[rowKey]string
}

// hide is a row whose kill is in flight: it stays out of every rebuild
// until the answer, and after a successful answer until a view read
// started after it lands (by then the session is gone from the view).
type hide struct {
	clearAt int // 0 while in flight; else the first read seq that clears it
}

type noteKind int

const (
	noteInfo noteKind = iota // goes after noteTime
	noteBusy                 // stays until replaced: something is in flight
	noteErr                  // stays until the next key
)

// noteTime is how long a message stays in the footer.
const noteTime = 2400 * time.Millisecond

type message struct {
	text string
	kind noteKind
	id   int
}

// readEvery paces view reads in a burst of changes.
const readEvery = 50 * time.Millisecond

// spinEvery is a spinner's frame time.
const spinEvery = 100 * time.Millisecond

// Model is the dashboard: the last view read, what it lists, and what
// the user is doing. Bubble Tea's update loop owns it; calls to towerd,
// tmux and ssh run as commands and come back as messages.
type Model struct {
	c    *Conn
	ctx  context.Context
	live bool // follow towerd's watch (TOWER_LIVE)
	dash bool // tower dash in the loop's picker: esc means "the last target"
	now  func() time.Time

	view   proto.Dash
	readAt time.Time
	have   bool
	w      *world
	order  []string // the hosts' order, taken when the dashboard opens
	hidden map[rowKey]hide
	fresh  map[rowKey]bool // sessions made here, "new" until attached
	here   proto.Ref       // the client's own session (tmux), for a client no loop owns

	mode     mode
	back     mode // where a prompt, confirm, picker or help returns
	focus    col
	cs       [3]colState
	sel      selection
	allDirs  bool // ^g: every zoxide entry, not only git roots
	find     finder
	prompt   *prompt
	confirm  *confirm
	picker   *picker
	pendingG bool

	widths [3]int // the columns' natural widths; they only grow
	note   message
	noteN  int
	busy   bool // ⏎ in flight

	width, height int

	reading  bool
	dirty    bool
	ticking  bool
	lastRead time.Time
	seq      int // view reads started
	gen      uint64

	cap      capState
	checks   map[string]*hostCheck // add-host checks, by host name
	spinning bool
	spin     int

	click lastClick
	geo   geometry

	want      *rowKey // select this row once a view read has it (what new made)
	wantReads int

	choice    *proto.Ref
	last      bool
	quitted   bool
	firstDraw func()
}

// newModel is a model over c, starting from view d when it was read
// already (have). It opens in the finder, so typing a name and ⏎ moves
// there, as the picker it replaces did.
func newModel(ctx context.Context, c *Conn, d proto.Dash, have bool, live bool) *Model {
	m := &Model{c: c, ctx: ctx, live: live, now: time.Now, hidden: map[rowKey]hide{}, fresh: map[rowKey]bool{},
		width: 100, height: 30, mode: modeFind, focus: colSessions, checks: map[string]*hostCheck{}}
	m.find.front = true
	m.sel = selection{entry: map[string]rowKey{}, window: map[rowKey]string{}, pane: map[rowKey]string{}}
	m.cap.cache = map[capKey]*capture{}
	if have {
		m.view, m.have, m.readAt, m.gen = d, true, m.now(), d.Gen
		m.rebuild()
		m.find.toBest()
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
	readTickMsg   struct{}
	watchRetryMsg struct{}
	noteExpireMsg struct{ id int }
	spinMsg       struct{}
	actMsg        struct {
		op   string
		host string
		key  rowKey   // the row a kill hid
		also []rowKey // and its session (with its group's), for a session's last window
		ack  proto.Ack
		err  error
		then func(*Model, proto.Ack) tea.Cmd // after a success
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
	cmds = append(cmds, m.wantCapture(), m.spinIfNeeded())
	return tea.Batch(cmds...)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case tea.PasteMsg:
		return m, m.paste(msg.Content)
	case tea.MouseClickMsg:
		return m, m.mouseClick(tea.Mouse(msg))
	case tea.MouseWheelMsg:
		return m, m.mouseWheel(tea.Mouse(msg))
	case viewMsg:
		return m, m.gotView(msg)
	case watchMsg:
		return m, m.gotWatch(msg)
	case readTickMsg:
		m.ticking = false
		return m, m.read()
	case watchRetryMsg:
		return m, m.watch()
	case noteExpireMsg:
		if msg.id == m.note.id && m.note.kind == noteInfo {
			m.note = message{}
		}
	case spinMsg:
		m.spinning = false
		m.spin++
		return m, m.spinIfNeeded()
	case actMsg:
		return m, m.gotAct(msg)
	case captureMsg:
		return m, m.gotCapture(msg)
	case panesMsg:
		m.gotPanes(msg)
	case checkMsg:
		return m, m.gotCheck(msg)
	case hostEditMsg:
		return m, m.gotHostEdit(msg)
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

// setNote shows an outcome for noteTime.
func (m *Model) setNote(s string) tea.Cmd {
	m.noteN++
	m.note = message{text: s, kind: noteInfo, id: m.noteN}
	if s == "" {
		return nil
	}
	id := m.noteN
	return tea.Tick(noteTime, func(time.Time) tea.Msg { return noteExpireMsg{id: id} })
}

// setBusy shows what is in flight until its outcome replaces it.
func (m *Model) setBusy(s string) {
	m.noteN++
	m.note = message{text: s, kind: noteBusy, id: m.noteN}
}

// setErr shows an error until the next key.
func (m *Model) setErr(s string) {
	m.noteN++
	m.note = message{text: s, kind: noteErr, id: m.noteN}
}

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
		first := !m.have
		m.view, m.have, m.readAt = msg.dash, true, m.now()
		m.gen = max(m.gen, msg.dash.Gen)
		for k, h := range m.hidden {
			if h.clearAt > 0 && msg.seq >= h.clearAt {
				delete(m.hidden, k)
			}
		}
		m.rebuild()
		if first {
			m.find.toBest()
		}
		if m.want != nil {
			if it := m.w.find(*m.want); it != nil {
				m.selectItem(it)
				if m.mode == modeFind {
					m.findSelect(it)
				}
				m.want = nil
			} else if m.wantReads++; m.wantReads > 3 {
				m.want = nil
			}
		}
		cmds = append(cmds, m.wantCapture(), m.spinIfNeeded())
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

// spinIfNeeded runs the spinner while a host shows one.
func (m *Model) spinIfNeeded() tea.Cmd {
	if m.spinning || m.quitted || m.w == nil {
		return nil
	}
	need := false
	for _, h := range m.w.hosts {
		need = need || loading(h.host)
	}
	for _, c := range m.checks {
		need = need || c.running
	}
	if !need {
		return nil
	}
	m.spinning = true
	return tea.Tick(spinEvery, func(time.Time) tea.Msg { return spinMsg{} })
}

func (m *Model) loopID() string {
	if m.view.Loop != "" {
		return m.view.Loop
	}
	return m.c.Loop
}

// onHome reports whether the dashboard runs on its view's home, where
// the host list lives.
func (m *Model) onHome() bool {
	return m.view.Self != "" && (m.view.View.Home == "" || m.view.View.Home == m.view.Self)
}

// rebuild derives the lists from the view, leaves hidden rows out, and
// keeps every cursor on its row; a row that went hands its place to its
// nearest neighbour.
func (m *Model) rebuild() {
	if !m.have {
		return
	}
	if m.order == nil {
		m.order = hostOrder(&m.view)
	} else {
		known := map[string]bool{}
		for _, n := range m.order {
			known[n] = true
		}
		for _, h := range m.view.View.Hosts {
			if !known[h.Name] {
				m.order = append(m.order, h.Name)
			}
		}
	}
	old := m.w
	mk := marksFor(&m.view, m.loopID(), m.here)
	m.w = buildWorld(&m.view, mk, m.now().Sub(m.readAt), m.order, m.hidden, m.fresh)
	if old == nil {
		m.selectCurrent()
	} else {
		m.follow(old)
	}
	m.find.build(m.w, m.allDirs, false)
	m.grow()
}
