package ui

import (
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// The model's side of every action: what a key does to the rows at once,
// and the command that asks towerd (or tmux, or the host list).

// focused is the row the focused column's cursor is on.
func (m *Model) focused() *item {
	switch m.focus {
	case colHosts:
		return m.curHost()
	case colSessions:
		return m.curEntry()
	}
	return m.curWindow()
}

// target is what an action acts on: the finder's row, or the focused
// column's.
func (m *Model) target() *item {
	if m.mode == modeFind || m.back == modeFind && (m.mode == modePrompt || m.mode == modeConfirm) {
		if r := m.find.selected(); r != nil {
			if r.win != nil {
				return r.win
			}
			return r.it
		}
		return nil
	}
	return m.focused()
}

// --- ⏎ ---

func (m *Model) enterColumn() tea.Cmd {
	switch m.focus {
	case colHosts:
		return m.enterHost(m.curHost())
	case colSessions:
		e := m.curEntry()
		if e == nil {
			if h := m.curHost(); h != nil {
				return m.enterHost(h)
			}
			return nil
		}
		return m.enterItem(e)
	}
	if w := m.curWindow(); w != nil {
		return m.enterItem(w)
	}
	if len(m.cs[colWindows].in.text) > 0 {
		m.setErr("no window matches · esc clears")
	}
	return nil
}

func (m *Model) enterFound() tea.Cmd {
	r := m.find.selected()
	if r == nil {
		return nil
	}
	switch r.kind {
	case fMore:
		return m.expand()
	case fWindow:
		return m.enterItem(r.win)
	}
	return m.enterItem(r.it)
}

// enterItem is ⏎ on a session (at its remembered window), a window (with
// its picked pane) or a dir (a new session there).
func (m *Model) enterItem(it *item) tea.Cmd {
	if it.host.Status == proto.StatusDown || it.host.Status == proto.StatusFailed {
		return m.retryHost(it.host)
	}
	switch it.kind {
	case kSession:
		k := it.key
		if id, ok := m.sel.window[k]; ok {
			k.Window = id
		}
		return m.enter(k, m.sel.pane[k])
	case kWindow:
		return m.enter(it.key, m.sel.pane[it.key])
	case kDir:
		return m.openDir(it)
	}
	return m.enterHost(it)
}

// enterHost opens a host: its sessions; a host that is down retries now,
// one whose check failed is checked again.
func (m *Model) enterHost(h *item) tea.Cmd {
	if h == nil {
		return nil
	}
	if c := m.checks[h.host.Name]; c != nil && !c.running && c.failed() {
		return m.recheck(c)
	}
	switch h.host.Status {
	case proto.StatusDown, proto.StatusFailed:
		return m.retryHost(h.host)
	case proto.StatusOff:
		m.setErr(h.host.Name + " is turned off: space turns it on")
		return nil
	}
	if m.mode == modeNormal || m.mode == modeSearch {
		m.focus = colSessions
		return m.moved()
	}
	return nil
}

// retryHost is ⏎ on a host that is down: that host is tried again now,
// its backoff afresh.
func (m *Model) retryHost(h *proto.Host) tea.Cmd {
	if !m.onHome() {
		m.setErr(h.Name + " is " + hostStatus(h))
		return nil
	}
	note := h.Name + " is " + hostStatus(h) + " · retrying now"
	m.setBusy(note)
	c, ctx, name := m.c, m.ctx, h.Name
	return func() tea.Msg {
		return hostEditMsg{note: note, err: c.Towerd.Retry(ctx, name)}
	}
}

// enter is ⏎ on the row k: re-resolved against a fresh view, then acted
// on (see Conn.enter). Further ⏎ wait for its outcome.
func (m *Model) enter(k rowKey, pane string) tea.Cmd {
	if m.busy {
		return nil
	}
	m.busy = true
	if it := m.w.find(k); it != nil {
		m.setBusy(m.enterText(it) + "…")
	}
	c, ctx := m.c, m.ctx
	return func() tea.Msg {
		out, ref, err := c.enter(ctx, k, pane)
		return enterMsg{out: out, ref: ref, err: err}
	}
}

// enterText is what ⏎ would do on it, for the footer.
func (m *Model) enterText(it *item) string {
	name := it.host.Name
	switch {
	case it.sess != nil:
		name += ":" + it.sess.Name
		if it.win != nil {
			name += ":" + winLabel(it)
		}
	case it.dir != nil:
		return "new session in " + it.dir.Path
	}
	switch {
	case !it.host.Reachable():
		return it.host.Name + " is " + hostStatus(it.host)
	case m.c.Pick:
		return "attach → " + name
	case it.local:
		return "switch-client → " + name
	case !m.view.Owned:
		return "hand-off to " + it.host.Name + " needs the attach loop"
	}
	return "hand-off → " + name
}

// --- new sessions and windows ---

// openDir is ⏎ on a dir: a session named after it, made there and
// attached. A name already taken opens the prompt with the first free
// one.
func (m *Model) openDir(d *item) tea.Cmd {
	if err := reachable(d.host); err != nil {
		m.setErr(err.Error())
		return nil
	}
	name := dirSessionName(d.dir.Path)
	if free := freeName(d.host, name); free != name {
		return m.ask(&prompt{kind: pDirName, pill: "NEW SESSION", about: "in " + d.dir.Path, it: *d, names: true,
			err: name + " is taken on " + d.host.Name}, free)
	}
	return m.makeIn(d, name)
}

// makeIn makes a session named name in dir d's directory and attaches it.
func (m *Model) makeIn(d *item, name string) tea.Cmd {
	if m.busy {
		return nil
	}
	m.busy = true
	m.setBusy("new session " + name + " in " + d.dir.Path + "…")
	c, ctx, h, dir := m.c, m.ctx, *d.host, d.dir.Path
	return func() tea.Msg {
		ack, err := c.newSession(ctx, &h, name, dir)
		if err != nil {
			return enterMsg{err: errors.New(failed("new", h.Name, err))}
		}
		if ack.Ref == nil {
			return enterMsg{err: errors.New("new on " + h.Name + ": no session made")}
		}
		out, ref, err := c.enter(ctx, refKey(*ack.Ref), "")
		return enterMsg{out: out, ref: ref, err: err}
	}
}

// refKey is a ref's row key.
func refKey(r proto.Ref) rowKey {
	return rowKey{Host: r.Host, Inst: r.Inst, Session: r.Session, Window: r.Window}
}

// dupSelected is D: the session duplicated as a grouped session, "<name>
// 2", and attached; an existing "<name> 2" is attached as it is.
func (m *Model) dupSelected() tea.Cmd {
	it := m.target()
	if it != nil && it.kind == kWindow {
		it = m.w.session(it.key)
	}
	if it == nil || it.kind != kSession {
		m.setErr("D duplicates a session")
		return nil
	}
	if err := reachable(it.host); err != nil {
		m.setErr(err.Error())
		return nil
	}
	name, found := dupName(it.host, it.sess)
	if found {
		for _, s := range m.w.sessions[it.host.Name] {
			if s.sess.Name == name {
				return m.enter(s.key, "")
			}
		}
	}
	if m.busy {
		return nil
	}
	m.busy = true
	m.setBusy("duplicate " + it.host.Name + ":" + it.sess.Name + "…")
	c, ctx, ref, host := m.c, m.ctx, rowTarget(it).ref, it.host.Name
	return func() tea.Msg {
		ack, err := c.dup(ctx, ref, name)
		if err != nil {
			return enterMsg{err: errors.New(failed("duplicate", host, err))}
		}
		if ack.Ref == nil {
			return enterMsg{err: errors.New("duplicate on " + host + ": no session made")}
		}
		out, r, err := c.enter(ctx, refKey(*ack.Ref), "")
		return enterMsg{out: out, ref: r, err: err}
	}
}

// --- kill ---

// kill sends the kill of it; the row goes at once and stays hidden while
// the kill is in flight; the answer sets the note, and a failure brings
// the row back. A session's last window takes the session with it.
func (m *Model) kill(it *item) tea.Cmd {
	t := rowTarget(it)
	op, host, key := "kill", it.host.Name, it.key
	var also []rowKey
	if it.kind == kWindow && len(m.w.windows[it.key.sessionKey()]) <= 1 {
		// The group shares its windows: every member goes with the last.
		also = append(also, it.key.sessionKey())
		if s := m.w.session(it.key); s != nil {
			for _, p := range m.w.groupPeerItems(s) {
				also = append(also, p.key)
			}
		}
		for _, k := range also {
			m.hidden[k] = hide{}
		}
	}
	config.Mark("dash: kill")
	m.hidden[key] = hide{}
	m.rebuild()
	m.setBusy(op + " on " + host + "…")
	c, ctx := m.c, m.ctx
	return tea.Batch(func() tea.Msg {
		ack, err := c.kill(ctx, t)
		return actMsg{op: op, host: host, key: key, also: also, ack: ack, err: err}
	}, m.wantCapture())
}

// gotAct takes an action's answer: the note, then a view read, since the
// rows read after an answer show its result.
func (m *Model) gotAct(msg actMsg) tea.Cmd {
	config.Mark("dash: " + msg.op + " answered")
	if msg.err != nil {
		for _, k := range append([]rowKey{msg.key}, msg.also...) {
			delete(m.hidden, k)
		}
		m.setErr(failed(msg.op, msg.host, msg.err))
		m.rebuild()
		return m.wantCapture()
	}
	note := m.setNote(done(msg.op, msg.host, msg.ack.Note))
	for _, k := range append([]rowKey{msg.key}, msg.also...) {
		if _, ok := m.hidden[k]; ok {
			// A read in flight may have started before the answer: only
			// the next one is sure to show the row gone.
			m.hidden[k] = hide{clearAt: m.seq + 1}
		}
	}
	var then tea.Cmd
	if msg.then != nil {
		then = msg.then(m, msg.ack)
	}
	return tea.Batch(note, then, m.read())
}

// --- the host list ---

// hostEditMsg is a host list edit's outcome.
type hostEditMsg struct {
	note string
	err  error
}

func (m *Model) gotHostEdit(msg hostEditMsg) tea.Cmd {
	if msg.err != nil {
		m.setErr(msg.err.Error())
		return nil
	}
	return tea.Batch(m.setNote(msg.note), m.read())
}

// hostEditable says why the host h cannot be edited here, or "".
func (m *Model) hostEditable(h *item) string {
	switch {
	case m.c.Hosts == nil || !m.onHome():
		return "hosts are edited on the home's machine"
	case h != nil && h.local:
		return "the local server is always listed"
	}
	return ""
}

// editHost runs a host list edit off the update loop.
func (m *Model) editHost(note string, fn func(HostList) error) tea.Cmd {
	hl := m.c.Hosts
	return func() tea.Msg {
		return hostEditMsg{note: note, err: fn(hl)}
	}
}

// toggleHost is space: the host turned off or on.
func (m *Model) toggleHost() tea.Cmd {
	if m.focus != colHosts {
		return nil
	}
	h := m.curHost()
	if h == nil {
		return nil
	}
	if c := m.checks[h.host.Name]; c != nil && c.running {
		m.setErr(h.host.Name + " is being checked")
		return nil
	}
	if why := m.hostEditable(h); why != "" {
		m.setErr(why)
		return nil
	}
	on := h.host.Status == proto.StatusOff
	name := h.host.Name
	verb := "off"
	if on {
		verb = "on"
	}
	return m.editHost("turned "+verb+" "+name, func(l HostList) error { return l.SetOn(name, on) })
}

// --- panes ---

// pickPane is J and K: the pane ⏎ lands on, cycling through the window's
// panes.
func (m *Model) pickPane(next bool) tea.Cmd {
	w := m.target()
	if w == nil || w.kind != kWindow {
		if m.focus != colWindows {
			return nil
		}
		m.setErr("J K pick a pane in the windows column")
		return nil
	}
	panes := m.cap.panesOf(w.key)
	if len(panes) == 0 {
		m.setErr("no panes known for " + winLabel(w) + " yet")
		return nil
	}
	cur := m.sel.pane[w.key]
	i := 0
	for j, p := range panes {
		if p.ID == cur || cur == "" && p.Active {
			i = j
		}
	}
	if next {
		i = (i + 1) % len(panes)
	} else {
		i = (i - 1 + len(panes)) % len(panes)
	}
	if panes[i].Active {
		delete(m.sel.pane, w.key)
	} else {
		m.sel.pane[w.key] = panes[i].ID
	}
	return m.moved()
}

// shells are commands a pane runs when nothing else does.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true,
	"mksh": true, "tcsh": true, "csh": true, "nu": true, "xonsh": true, "elvish": true, "pwsh": true, "login": true, "tmux": true}

// running are the panes' commands that are not shells.
func running(panes []proto.Pane) []string {
	var out []string
	for _, p := range panes {
		c := strings.TrimPrefix(p.Command, "-")
		if c == "" || shells[c] {
			continue
		}
		out = append(out, p.Command)
	}
	return out
}
