package ui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/jerrykal/tower/internal/config"
)

// key handles one key press.
func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	if m.mode == modePrompt {
		return m.promptKey(k)
	}
	switch k.String() {
	case "enter":
		if r := m.selected(); r != nil {
			return m.enter(r.key)
		}
		return nil
	case "esc":
		if m.busy {
			return nil // a ⏎ in flight decides how the dashboard ends
		}
		m.last = m.dash
		return m.quit()
	case "ctrl+c":
		if len(m.query) > 0 {
			return m.setQuery(nil)
		}
		if m.busy {
			return nil // as esc: a ⏎ in flight decides how the dashboard ends
		}
		return m.quit()
	case "up", "ctrl+k", "ctrl+p":
		return m.move(-1)
	case "down", "ctrl+j":
		return m.move(1)
	case "pgup":
		return m.move(-m.listHeight())
	case "pgdown":
		return m.move(m.listHeight())
	case "ctrl+x":
		return m.kill()
	case "ctrl+r":
		return m.ask(promptRename)
	case "ctrl+n":
		return m.ask(promptNew)
	case "ctrl+w":
		return m.windows()
	case "backspace", "ctrl+h":
		if len(m.query) > 0 {
			return m.setQuery(m.query[:len(m.query)-1])
		}
		return nil
	case "ctrl+u":
		return m.setQuery(nil)
	}
	if k.Text == "" || k.Mod&(tea.ModCtrl|tea.ModAlt) != 0 {
		return nil
	}
	if len(m.query) == 0 && m.mode == modeList && (k.Text == "-" || k.Text == ".") {
		return m.jump(k.Text == "-")
	}
	return m.setQuery(append(m.query, []rune(k.Text)...))
}

// setQuery filters by q; the cursor goes to the first match, as in fzf.
func (m *Model) setQuery(q []rune) tea.Cmd {
	m.query = q
	m.refilter()
	m.toTop()
	m.setNote("")
	return m.wantCapture()
}

// enter is ⏎ on the row k: re-resolved against a fresh view, then acted
// on (see Conn.enter). Further ⏎ wait for its outcome.
func (m *Model) enter(k rowKey) tea.Cmd {
	if m.busy {
		return nil
	}
	m.busy = true
	if r := m.selected(); r != nil && r.key == k {
		m.setNote(m.enterText(r) + "…")
	}
	c, ctx := m.c, m.ctx
	return func() tea.Msg {
		out, ref, err := c.enter(ctx, k)
		return enterMsg{out: out, ref: ref, err: err}
	}
}

// jump is - and .: ⏎ on the previous or the current session.
func (m *Model) jump(prev bool) tea.Cmd {
	for i := range m.rows {
		r := &m.rows[i]
		if prev && r.prev || !prev && r.cur {
			return m.enter(r.key)
		}
	}
	if prev {
		m.setErr("no previous session")
	} else {
		m.setErr("no current session")
	}
	return nil
}

// kill is ^x: the row goes at once and stays hidden while the kill is in
// flight; the answer sets the note, and a failure brings the row back.
func (m *Model) kill() tea.Cmd {
	r := m.selected()
	if r == nil {
		return nil
	}
	if r.sess == nil {
		m.setErr("no session to kill on " + r.host.Name)
		return nil
	}
	if err := reachable(r.host); err != nil {
		m.setErr(err.Error())
		return nil
	}
	t := rowTarget(r)
	op, host, key := "kill", r.host.Name, r.key
	config.Mark("dash: kill")
	m.hidden[key] = hide{}
	m.rebuild()
	m.setNote(op + " on " + host + "…")
	c, ctx := m.c, m.ctx
	return tea.Batch(func() tea.Msg {
		ack, err := c.kill(ctx, t)
		return actMsg{op: op, host: host, key: key, ack: ack, err: err}
	}, m.wantCapture())
}

// gotAct takes an action's answer: the note, and a view read, since the
// rows read after an answer show its result.
func (m *Model) gotAct(msg actMsg) tea.Cmd {
	config.Mark("dash: " + msg.op + " answered")
	if msg.err != nil {
		delete(m.hidden, msg.key)
		m.setErr(failed(msg.op, msg.host, msg.err))
		m.rebuild()
		return m.wantCapture()
	}
	m.setNote(done(msg.op, msg.host, msg.ack.Note))
	if _, ok := m.hidden[msg.key]; ok {
		// A read in flight may have started before the answer: only the
		// next one is sure to show the session gone.
		m.hidden[msg.key] = hide{clearAt: m.seq + 1}
	}
	if msg.op == "new" && msg.ack.Ref != nil {
		k := rowKey{Host: msg.ack.Ref.Host, Inst: msg.ack.Ref.Inst, Session: msg.ack.Ref.Session}
		m.want = &k
	}
	return m.read()
}

// ask opens the prompt for rename (^r) or new (^n).
func (m *Model) ask(kind promptKind) tea.Cmd {
	r := m.selected()
	if r == nil {
		return nil
	}
	p := &prompt{kind: kind, row: *r, back: m.mode}
	switch kind {
	case promptRename:
		if r.sess == nil {
			m.setErr("no session to rename on " + r.host.Name)
			return nil
		}
		if r.win != nil {
			p.label = "rename " + r.host.Name + ":" + r.sess.Name + ":" + r.win.Name
			p.text = []rune(r.win.Name)
		} else {
			p.label = "rename " + r.host.Name + ":" + r.sess.Name
			p.text = []rune(r.sess.Name)
		}
	case promptNew:
		p.label = "new session on " + r.host.Name
	}
	if err := reachable(r.host); err != nil {
		m.setErr(err.Error())
		return nil
	}
	m.prompt = p
	m.mode = modePrompt
	m.setNote("")
	return nil
}

func (m *Model) promptKey(k tea.KeyPressMsg) tea.Cmd {
	p := m.prompt
	switch k.String() {
	case "esc", "ctrl+c":
		m.endPrompt()
		return nil
	case "enter":
		m.endPrompt()
		return m.runPrompt(p)
	case "backspace", "ctrl+h":
		if len(p.text) > 0 {
			p.text = p.text[:len(p.text)-1]
		}
		return nil
	case "ctrl+u":
		p.text = nil
		return nil
	}
	if k.Text != "" && k.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
		p.text = append(p.text, []rune(k.Text)...)
	}
	return nil
}

func (m *Model) endPrompt() {
	m.mode = m.prompt.back
	m.prompt = nil
}

// runPrompt sends the prompt's action.
func (m *Model) runPrompt(p *prompt) tea.Cmd {
	name := string(p.text)
	c, ctx := m.c, m.ctx
	h := p.row.host
	switch p.kind {
	case promptRename:
		if name == "" {
			m.setErr("rename: a name is needed")
			return nil
		}
		t := rowTarget(&p.row)
		m.setNote("rename on " + h.Name + "…")
		return func() tea.Msg {
			ack, err := c.rename(ctx, t, name)
			return actMsg{op: "rename", host: h.Name, ack: ack, err: err}
		}
	default:
		if n := startingNote(h); n != "" {
			m.setNote(n)
		} else {
			m.setNote("new on " + h.Name + "…")
		}
		return func() tea.Msg {
			ack, err := c.newSession(ctx, h, name)
			return actMsg{op: "new", host: h.Name, ack: ack, err: err}
		}
	}
}

// windows is ^w: the selected session's windows as rows, and back.
func (m *Model) windows() tea.Cmd {
	if m.mode == modeWindows {
		m.mode = modeList
		m.query = m.saved
		m.cursor = m.savedKey
		m.rebuild()
		m.setNote("")
		return m.wantCapture()
	}
	r := m.selected()
	if r == nil || r.sess == nil {
		return nil
	}
	m.saved, m.savedKey = m.query, r.key
	m.mode, m.winOf, m.query = modeWindows, r.key, nil
	m.rebuild()
	// Start on the session's active window.
	for i := range m.shown {
		if m.shown[i].win.Active {
			m.at = i
		}
	}
	m.sync()
	m.setNote("")
	return m.wantCapture()
}

// enterText is what ⏎ would do on r, for the status line.
func (m *Model) enterText(r *row) string {
	name := r.host.Name
	if r.sess != nil {
		name += ":" + r.sess.Name
	}
	if r.win != nil {
		name += ":" + r.win.Name
	}
	switch {
	case r.sess == nil:
		return "^n makes a session on " + r.host.Name
	case !r.host.Reachable():
		return r.host.Name + " is " + r.status
	case m.c.Pick:
		return "attach → " + name
	case r.local:
		return "switch-client → " + name
	case !m.view.Owned:
		return "hand-off to " + r.host.Name + " needs the attach loop"
	}
	return "hand-off → " + name
}
