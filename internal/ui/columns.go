package ui

import (
	"slices"
)

// The columns: hosts › sessions and dirs › windows. Each column filters
// by its own query and keeps its cursor on its remembered row; the next
// column lists what the cursor's row holds.

// list is one column as shown: the rows its query matches.
type list struct {
	items []item
	hits  [][]int
	at    int // the cursor's row; -1 when empty
	total int // rows before the query
}

func (l list) cur() *item {
	if l.at < 0 || l.at >= len(l.items) {
		return nil
	}
	return &l.items[l.at]
}

// filter keeps the items q matches, in their order.
func filter(items []item, q []rune) list {
	l := list{at: -1, total: len(items)}
	fq := foldQuery(q)
	for _, it := range items {
		if pos, ok := filterMatch(fq, it.name); ok {
			l.items = append(l.items, it)
			l.hits = append(l.hits, pos)
		}
	}
	return l
}

func (l *list) place(match func(*item) bool) bool {
	for i := range l.items {
		if match(&l.items[i]) {
			l.at = i
			return true
		}
	}
	return false
}

// hostList is the hosts column; its cursor on the remembered host, else
// the first match.
func (m *Model) hostList() list {
	if m.w == nil {
		return list{at: -1}
	}
	l := filter(m.w.hosts, m.cs[colHosts].in.text)
	if len(l.items) > 0 && !l.place(func(it *item) bool { return it.host.Name == m.sel.host }) {
		l.at = 0
	}
	return l
}

func (m *Model) curHost() *item {
	l := m.hostList()
	if it := l.cur(); it != nil {
		return m.w.host(it.host.Name)
	}
	return nil
}

// entryList is the sessions column for host h: its sessions, then its
// dirs.
func (m *Model) entryList(h *item) list {
	if h == nil {
		return list{at: -1}
	}
	l := filter(m.w.entries(h.host.Name, m.allDirs), m.cs[colSessions].in.text)
	if len(l.items) == 0 {
		return l
	}
	if k, ok := m.sel.entry[h.host.Name]; ok && l.place(func(it *item) bool { return it.key == k }) {
		return l
	}
	if l.place(func(it *item) bool { return it.cur }) {
		return l
	}
	l.at = 0
	return l
}

func (m *Model) curEntry() *item {
	l := m.entryList(m.curHost())
	return l.cur()
}

// windowList is the windows column for entry e (a session).
func (m *Model) windowList(e *item) list {
	if e == nil || e.kind != kSession {
		return list{at: -1}
	}
	l := filter(m.w.windows[e.key], m.cs[colWindows].in.text)
	if len(l.items) == 0 {
		return l
	}
	if id, ok := m.sel.window[e.key]; ok && l.place(func(it *item) bool { return it.win.ID == id }) {
		return l
	}
	if l.place(func(it *item) bool { return it.cur }) {
		return l
	}
	if l.place(func(it *item) bool { return it.win.Active }) {
		return l
	}
	l.at = 0
	return l
}

func (m *Model) curWindow() *item {
	l := m.windowList(m.curEntry())
	return l.cur()
}

// sessionWindow is the window ⏎ on session s lands on: the one remembered
// in the windows column, else the current one, else its active one.
func (m *Model) sessionWindow(s *item) *item {
	ws := m.w.windows[s.key]
	if id, ok := m.sel.window[s.key]; ok {
		for i := range ws {
			if ws[i].win.ID == id {
				return &ws[i]
			}
		}
	}
	for i := range ws {
		if ws[i].cur {
			return &ws[i]
		}
	}
	for i := range ws {
		if ws[i].win.Active {
			return &ws[i]
		}
	}
	if len(ws) > 0 {
		return &ws[0]
	}
	return nil
}

// columnList is column c's list as shown.
func (m *Model) columnList(c col) list {
	switch c {
	case colHosts:
		return m.hostList()
	case colSessions:
		return m.entryList(m.curHost())
	}
	return m.windowList(m.curEntry())
}

// moveIn moves column c's cursor by d rows (clamped) and remembers the
// row it lands on.
func (m *Model) moveIn(c col, d int) {
	l := m.columnList(c)
	if len(l.items) == 0 {
		return
	}
	i := min(max(l.at+d, 0), len(l.items)-1)
	m.remember(c, &l.items[i])
}

// moveTo puts column c's cursor on row i (clamped).
func (m *Model) moveTo(c col, i int) {
	l := m.columnList(c)
	if len(l.items) == 0 {
		return
	}
	m.remember(c, &l.items[min(max(i, 0), len(l.items)-1)])
}

func (m *Model) remember(c col, it *item) {
	switch c {
	case colHosts:
		m.sel.host = it.host.Name
	case colSessions:
		m.sel.entry[it.host.Name] = it.key
	case colWindows:
		m.sel.window[it.key.sessionKey()] = it.win.ID
	}
}

// selectCurrent selects where the client is (󰧟): its host, session and
// window; else the first host.
func (m *Model) selectCurrent() {
	for _, h := range m.w.hosts {
		for _, s := range m.w.sessions[h.host.Name] {
			if s.cur {
				m.selectItem(&s)
				return
			}
		}
	}
	if len(m.w.hosts) > 0 {
		m.sel.host = m.w.hosts[0].host.Name
	}
}

// selectItem selects a session, window or dir in the columns: host,
// entry and window.
func (m *Model) selectItem(it *item) {
	m.sel.host = it.host.Name
	switch it.kind {
	case kSession, kDir:
		m.sel.entry[it.host.Name] = it.key
	case kWindow:
		m.sel.entry[it.host.Name] = it.key.sessionKey()
		m.sel.window[it.key.sessionKey()] = it.win.ID
	}
}

// reveal selects it and clears the column queries that would hide it.
func (m *Model) reveal(it *item) {
	m.selectItem(it)
	if _, ok := filterMatch(foldQuery(m.cs[colHosts].in.text), it.host.Name); !ok {
		m.cs[colHosts].in = input{}
	}
	entry := it
	if it.kind == kWindow {
		entry = m.w.session(it.key)
	}
	if entry != nil {
		if _, ok := filterMatch(foldQuery(m.cs[colSessions].in.text), entry.name); !ok {
			m.cs[colSessions].in = input{}
		}
	}
	if it.kind == kWindow {
		if _, ok := filterMatch(foldQuery(m.cs[colWindows].in.text), it.name); !ok {
			m.cs[colWindows].in = input{}
		}
	}
}

// follow keeps the selection on rows that are still there after a
// rebuild: a host, session, dir or window that went hands its place to
// the nearest row after it in the old list that is still there, else
// before it.
func (m *Model) follow(old *world) {
	keyOfHost := func(it item) rowKey { return rowKey{Host: it.host.Name} }
	if m.sel.host != "" && m.w.host(m.sel.host) == nil {
		if n := neighbour(old.hosts, m.w.hosts, rowKey{Host: m.sel.host}, keyOfHost); n != nil {
			m.sel.host = n.host.Name
		}
	}
	h := m.sel.host
	if k, ok := m.sel.entry[h]; ok && m.w.find(k) == nil {
		before, now := old.entries(h, m.allDirs), m.w.entries(h, m.allDirs)
		// A session that went hands its place to another session, while
		// there is one: landing on a dir would turn the next ⏎ into a new
		// session.
		var n *item
		if gone := old.find(k); gone != nil {
			n = neighbour(ofKind(before, gone.kind), ofKind(now, gone.kind), k, keyOf)
		}
		if n == nil {
			n = neighbour(before, now, k, keyOf)
		}
		if n != nil {
			m.sel.entry[h] = n.key
		} else {
			delete(m.sel.entry, h)
		}
	}
	if e := m.curEntry(); e != nil && e.kind == kSession {
		if id, ok := m.sel.window[e.key]; ok {
			k := e.key
			k.Window = id
			if m.w.find(k) == nil {
				if n := neighbour(old.windows[e.key], m.w.windows[e.key], k, keyOf); n != nil {
					m.sel.window[e.key] = n.win.ID
				} else {
					delete(m.sel.window, e.key)
				}
			}
		}
	}
}

func keyOf(it item) rowKey { return it.key }

// ofKind is the items of one kind, in order.
func ofKind(its []item, k itemKind) []item {
	var out []item
	for _, it := range its {
		if it.kind == k {
			out = append(out, it)
		}
	}
	return out
}

// neighbour is the row of now nearest to k's place in before: after it,
// else before it.
func neighbour(before, now []item, k rowKey, key func(item) rowKey) *item {
	at := slices.IndexFunc(before, func(it item) bool { return key(it) == k })
	if at < 0 {
		return nil
	}
	find := func(kk rowKey) *item {
		for i := range now {
			if key(now[i]) == kk {
				return &now[i]
			}
		}
		return nil
	}
	for i := at + 1; i < len(before); i++ {
		if n := find(key(before[i])); n != nil {
			return n
		}
	}
	for i := at - 1; i >= 0; i-- {
		if n := find(key(before[i])); n != nil {
			return n
		}
	}
	return nil
}

// jump is - and .: select the previous or current session and window,
// without attaching; queries that would hide it are cleared.
func (m *Model) jump(prev bool) bool {
	for _, h := range m.w.hosts {
		for _, s := range m.w.sessions[h.host.Name] {
			if prev && s.prev || !prev && s.cur {
				target := &s
				if prev {
					if l := m.view.View.LoopByID(m.loopID()); l != nil && l.Prev.Window != "" {
						k := s.key
						k.Window = l.Prev.Window
						if w := m.w.find(k); w != nil {
							target = w
						}
					}
				} else if w := m.sessionWindow(&s); w != nil && w.cur {
					target = w
				}
				if m.mode == modeFind {
					m.findSelect(target)
				} else {
					m.reveal(target)
					if target.kind == kWindow && m.focus == colHosts {
						m.focus = colSessions
					}
				}
				return true
			}
		}
	}
	return false
}

// findSelect puts the finder's cursor on it: its row, or its session's.
func (m *Model) findSelect(it *item) {
	for _, k := range []rowKey{it.key, it.key.sessionKey()} {
		if m.find.index(k) >= 0 {
			m.find.cursor = k
			return
		}
	}
	// The query hides it: clear the query.
	m.find.in = input{}
	m.find.build(m.w, m.allDirs, true)
	for _, k := range []rowKey{it.key, it.key.sessionKey()} {
		if m.find.index(k) >= 0 {
			m.find.cursor = k
			return
		}
	}
}
