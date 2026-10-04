package ui

import (
	tea "charm.land/bubbletea/v2"
)

// key handles one key press in the current mode.
func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	if m.note.kind == noteErr {
		m.note = message{} // an error stays until the next key
	}
	switch m.mode {
	case modeHelp:
		m.mode = m.back
		return nil
	case modePrompt:
		return m.promptKey(k)
	case modeConfirm:
		return m.confirmKey(k)
	case modeAddHost:
		return m.pickerKey(k)
	case modeFind:
		return m.findKey(k)
	case modeSearch:
		return m.searchKey(k)
	}
	return m.normalKey(k)
}

// paste puts pasted text, as one line, into whatever takes text.
func (m *Model) paste(s string) tea.Cmd {
	s = oneLine(s)
	switch m.mode {
	case modeFind:
		m.find.in.cur = len(m.find.in.text)
		m.find.in.insert(s)
		return m.requery()
	case modeNormal:
		m.mode = modeSearch
		m.cs[m.focus].in.cur = len(m.cs[m.focus].in.text)
		fallthrough
	case modeSearch:
		m.cs[m.focus].in.insert(s)
		return m.wantCapture()
	case modePrompt:
		m.prompt.typeText(s)
	case modeAddHost:
		m.picker.in.insert(s)
		m.picker.refilter()
	}
	return nil
}

// leave is esc and ^c with nothing left to clear: the dashboard closes,
// unless a ⏎ is in flight (its outcome decides how it ends). In tower
// dash outside tmux, esc means "attach to the last target".
func (m *Model) leave(esc bool) tea.Cmd {
	if m.busy {
		return nil
	}
	m.last = esc && m.dash
	return m.quit()
}

// --- normal mode: the columns ---

func (m *Model) normalKey(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	if m.pendingG {
		m.pendingG = false
		if s == "g" {
			m.moveTo(m.focus, 0)
			return m.moved()
		}
	}
	switch s {
	case "h", "ctrl+h", "left":
		m.focusBy(-1, false)
	case "l", "ctrl+l", "right":
		m.focusBy(1, false)
	case "tab":
		m.focusBy(1, true)
	case "shift+tab":
		m.focusBy(-1, true)
	case "j", "ctrl+j", "down":
		m.moveIn(m.focus, 1)
		return m.moved()
	case "k", "ctrl+k", "up":
		m.moveIn(m.focus, -1)
		return m.moved()
	case "g":
		m.pendingG = true
	case "G", "end":
		m.moveTo(m.focus, 1<<30)
		return m.moved()
	case "home":
		m.moveTo(m.focus, 0)
		return m.moved()
	case "ctrl+d", "pgdown":
		step := max(m.bodyHeight()/2, 1)
		if s == "pgdown" {
			step = max(m.bodyHeight()-1, 1)
		}
		m.moveIn(m.focus, step)
		return m.moved()
	case "ctrl+u", "pgup":
		step := max(m.bodyHeight()/2, 1)
		if s == "pgup" {
			step = max(m.bodyHeight()-1, 1)
		}
		m.moveIn(m.focus, -step)
		return m.moved()
	case "/", "i":
		m.mode = modeSearch
		m.cs[m.focus].in.cur = len(m.cs[m.focus].in.text)
	case "f":
		return m.openFinder()
	case "1", "2", "3":
		c := col(s[0] - '1')
		if c == colWindows && !m.hasWindows() {
			return nil
		}
		m.focus = c
		return m.wantCapture()
	case "enter":
		return m.enterColumn()
	case "J", "K":
		return m.pickPane(s == "J")
	case "D":
		return m.dupSelected()
	case "-", ".":
		if !m.jump(s == "-") {
			if s == "-" {
				m.setErr("no previous session")
			} else {
				m.setErr("no current session")
			}
			return nil
		}
		return m.moved()
	case "n":
		return m.askNew()
	case "r", "ctrl+r":
		return m.askRename()
	case "x", "ctrl+x":
		return m.askKill()
	case "a":
		return m.openPicker()
	case "space":
		return m.toggleHost()
	case "ctrl+g":
		return m.toggleDirs()
	case "esc", "ctrl+c":
		if q := &m.cs[m.focus].in; len(q.text) > 0 {
			*q = input{}
			return m.moved()
		}
		cleared := false
		for i := range m.cs {
			if len(m.cs[i].in.text) > 0 {
				m.cs[i].in = input{}
				cleared = true
			}
		}
		if cleared {
			return m.moved()
		}
		return m.leave(s == "esc")
	case "q":
		return m.leave(false)
	case "?":
		m.back, m.mode = m.mode, modeHelp
	}
	return nil
}

// moved is what follows a cursor move: the preview's capture.
func (m *Model) moved() tea.Cmd { return m.wantCapture() }

// hasWindows reports whether the windows column has anything: the
// selected entry is a session.
func (m *Model) hasWindows() bool {
	e := m.curEntry()
	return e != nil && e.kind == kSession
}

// focusBy moves the focus a column left or right; wrap (tab) goes round.
// The windows column is skipped for a dir.
func (m *Model) focusBy(d int, wrap bool) {
	n := 3
	if !m.hasWindows() {
		n = 2
	}
	c := int(m.focus) + d
	switch {
	case wrap:
		c = (c + n) % n
	case c < 0:
		c = 0
	case c >= n:
		c = n - 1
	}
	m.focus = col(c)
}

// --- search mode: typing filters the focused column ---

func (m *Model) searchKey(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	q := &m.cs[m.focus].in
	switch s {
	case "esc":
		m.mode = modeNormal
		for i := range m.cs {
			m.cs[i].in.cur = len(m.cs[i].in.text)
		}
		return nil
	case "ctrl+c":
		*q = input{}
		return m.moved()
	case "ctrl+j", "down", "ctrl+n":
		m.moveIn(m.focus, 1)
		return m.moved()
	case "ctrl+k", "up", "ctrl+p":
		m.moveIn(m.focus, -1)
		return m.moved()
	case "ctrl+l", "tab":
		m.focusBy(1, s == "tab")
		return m.moved()
	case "ctrl+h", "shift+tab":
		m.focusBy(-1, s == "shift+tab")
		return m.moved()
	case "enter":
		return m.enterColumn()
	case "ctrl+r":
		return m.askRename()
	case "ctrl+x":
		return m.askKill()
	case "ctrl+g":
		return m.toggleDirs()
	}
	if q.edit(s, true) {
		return m.moved()
	}
	if t := typed(k); t != "" {
		q.insert(t)
		return m.moved()
	}
	return nil
}

// --- the finder ---

// openFinder replaces the columns with the finder, its query empty.
func (m *Model) openFinder() tea.Cmd {
	m.mode = modeFind
	m.find.front = false
	m.find.in = input{}
	m.find.expanded = rowKey{}
	m.find.build(m.w, m.allDirs, true)
	if e := m.curEntry(); e != nil {
		m.findSelect(e)
	}
	return m.moved()
}

// requery rebuilds the finder for a changed query: the cursor goes to the
// best row of the top session.
func (m *Model) requery() tea.Cmd {
	m.find.expanded = rowKey{}
	if m.w != nil {
		m.find.build(m.w, m.allDirs, true)
	}
	return m.moved()
}

// closeFinder goes back to the columns, the finder's row selected there.
func (m *Model) closeFinder(selectRow bool) tea.Cmd {
	if selectRow {
		if r := m.find.selected(); r != nil {
			it := r.it
			if r.win != nil {
				it = r.win
			}
			m.reveal(it)
			m.focus = colSessions
			if r.win != nil {
				m.focus = colWindows
			}
		}
	}
	m.mode = modeNormal
	m.find.front = false
	return m.moved()
}

func (m *Model) findKey(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	f := &m.find
	switch s {
	case "esc":
		if f.front {
			return m.leave(true)
		}
		return m.closeFinder(false)
	case "ctrl+c":
		if len(f.in.text) > 0 {
			f.in = input{}
			return m.requery()
		}
		if f.front {
			return m.leave(false)
		}
		return m.closeFinder(false)
	case "ctrl+j", "down", "ctrl+n":
		f.move(1)
		return m.moved()
	case "ctrl+k", "up", "ctrl+p":
		f.move(-1)
		return m.moved()
	case "pgdown":
		f.move(max(m.bodyHeight()-1, 1))
		return m.moved()
	case "pgup":
		f.move(-max(m.bodyHeight()-1, 1))
		return m.moved()
	case "enter":
		return m.enterFound()
	case "tab":
		return m.expand()
	case "ctrl+l":
		return m.closeFinder(true)
	case "ctrl+x":
		return m.askKill()
	case "ctrl+g":
		return m.toggleDirs()
	case "ctrl+r":
		return m.askRename()
	}
	if s == "backspace" || s == "ctrl+u" {
		f.in.edit(s, false)
		return m.requery()
	}
	t := typed(k)
	if t == "" {
		return nil
	}
	if len(f.in.text) == 0 {
		switch t {
		case "-", ".":
			if !m.jump(t == "-") {
				if t == "-" {
					m.setErr("no previous session")
				} else {
					m.setErr("no current session")
				}
				return nil
			}
			return m.moved()
		case "?":
			m.back, m.mode = m.mode, modeHelp
			return nil
		}
	}
	f.in.cur = len(f.in.text)
	f.in.insert(t)
	return m.requery()
}

// expand is tab: every window of the selected session, or folded back.
func (m *Model) expand() tea.Cmd {
	r := m.find.selected()
	if r == nil || r.it == nil || r.it.kind != kSession {
		return nil
	}
	sk := r.it.key
	if m.find.expanded == sk {
		m.find.expanded = rowKey{}
		m.find.build(m.w, m.allDirs, false)
		m.find.cursor = sk
	} else {
		m.find.expanded = sk
		m.find.build(m.w, m.allDirs, false)
		if m.find.index(m.find.cursor) < 0 {
			m.find.cursor = sk
		}
	}
	return m.moved()
}

// toggleDirs is ^g: git roots only, or every zoxide entry.
func (m *Model) toggleDirs() tea.Cmd {
	m.allDirs = !m.allDirs
	m.rebuild()
	if m.allDirs {
		return m.setNote("dirs: every zoxide entry")
	}
	return m.setNote("dirs: git roots")
}
