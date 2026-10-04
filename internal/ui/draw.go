package ui

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jerrykal/tower/internal/proto"
)

// View draws the dashboard.
func (m *Model) View() tea.View {
	if m.firstDraw != nil {
		m.firstDraw()
		m.firstDraw = nil
	}
	if m.quitted {
		return tea.NewView("")
	}
	cv, cx, cy := m.render()
	v := tea.NewView(cv.String())
	v.AltScreen = true
	if m.mode != modePrompt && m.mode != modeConfirm && m.mode != modeAddHost && m.mode != modeHelp {
		v.MouseMode = tea.MouseModeCellMotion
	}
	if cx >= 0 {
		v.Cursor = tea.NewCursor(cx, cy)
		v.Cursor.Blink = false
	}
	return v
}

// render draws the screen into a canvas and returns it with the text
// cursor's place (-1: none).
func (m *Model) render() (*canvas, int, int) {
	g := m.layout()
	m.geo = g
	cv := newCanvas(g.w, g.h)
	if g.small {
		cv.put(0, 0, "tower needs "+strconv.Itoa(minWidth)+"×"+strconv.Itoa(minHeight)+" (now "+
			strconv.Itoa(g.w)+"×"+strconv.Itoa(g.h)+")", sErr, g.w)
		if g.h > 1 {
			cv.put(0, 1, "q quits", sMuted, g.w)
		}
		return cv, -1, -1
	}
	cx, cy := m.drawTop(cv, g)
	cv.put(0, 1, strings.Repeat("─", g.w), sRule, g.w)
	m.drawCrumb(cv, g)
	m.drawFrame(cv, g)
	if g.finder {
		m.drawFinder(cv, g)
	} else {
		m.drawColumns(cv, g)
	}
	if p := g.cols[3]; p.w > 0 {
		m.drawPreview(cv, p.x, g.headY, g.bodyY, p.w, g.bodyH)
	}
	m.drawFooter(cv, g)
	switch m.mode {
	case modeHelp:
		m.drawHelp(cv, g)
	case modeAddHost:
		if x, y := m.drawPicker(cv, g); x >= 0 {
			cx, cy = x, y
		}
	}
	return cv, cx, cy
}

// pill draws a mode's label and returns the cells it took.
func pill(cv *canvas, x, y int, label string, bg style) int {
	n := cv.put(x, y, glyphPillL, fg(bg.fg), -1)
	n += cv.put(x+n, y, " "+label+" ", fg(cBase).Bold().On(bg.fg), -1)
	n += cv.put(x+n, y, glyphPillR, fg(bg.fg), -1)
	return n
}

// drawTop draws the mode line and returns the text cursor's place.
func (m *Model) drawTop(cv *canvas, g geometry) (int, int) {
	right := ""
	var rightSt style = sMuted
	switch m.mode {
	case modeFind:
		right = strconv.Itoa(m.find.found) + "/" + strconv.Itoa(m.find.total)
	case modeNormal, modeSearch:
		l := m.columnList(m.focus)
		right = strconv.Itoa(len(l.items)) + "/" + strconv.Itoa(l.total)
	case modeConfirm:
		right, rightSt = "y/n", sErr
	}
	rx := g.w - width(right) - 1
	cv.put(rx, 0, right, rightSt, -1)
	if m.view.Note != "" && m.mode != modeConfirm {
		note := cutMiddleText(m.view.Note, max(g.w/3, 10))
		rx -= width(note) + 2
		cv.put(rx, 0, note, sErr, -1)
	}
	x := 0
	cx, cy := -1, -1
	room := func() int { return max(rx-1-x, 0) }
	switch m.mode {
	case modeNormal, modeHelp:
		x += pill(cv, x, 0, "NORMAL", sIris) + 1
		x += cv.put(x, 0, glyphSearch+" ", sMuted, room())
		x += cv.put(x, 0, colNames[m.focus]+" ", sSubtle, room())
		if q := m.cs[m.focus].in.String(); q != "" {
			cv.put(x, 0, "/"+q, sGold, room())
		} else {
			cv.put(x, 0, "press / to search this column", sMuted, room())
		}
	case modeSearch:
		x += pill(cv, x, 0, "SEARCH", sGold) + 1
		x += cv.put(x, 0, glyphSearch+" ", sGold, room())
		x += cv.put(x, 0, colNames[m.focus]+" ", sSubtle, room())
		q := &m.cs[m.focus].in
		text, at := inputView(q.text, q.cur, room())
		cv.put(x, 0, text, sGold, room())
		cx, cy = x+at, 0
	case modeFind, modeConfirm, modePrompt, modeAddHost:
		if m.mode == modeFind {
			x += pill(cv, x, 0, "FIND", sRose) + 1
			x += cv.put(x, 0, glyphSearch+" ", sRose, room())
			q := &m.find.in
			if len(q.text) == 0 {
				cv.put(x, 0, "session, window or host · words in any order", sMuted, room())
				cx, cy = x, 0
			} else {
				text, at := inputView(q.text, len(q.text), room())
				cv.put(x, 0, text, sPlain, room())
				cx, cy = x+at, 0
			}
		}
		if m.mode == modeConfirm {
			x += pill(cv, x, 0, "CONFIRM", sErr) + 1
			cv.put(x, 0, cutMiddleText(m.confirm.text(), room()), sPlain, room())
		}
		if m.mode == modePrompt {
			p := m.prompt
			x += pill(cv, x, 0, p.pill, sFoam) + 1
			tail := ""
			if p.err != "" {
				tail = "  " + glyphRefused + " " + p.err
			} else if len(p.in.text) == 0 && p.hint != "" {
				tail = "  " + p.hint
			}
			about := "  " + p.about
			inW := max(room()-width(tail)-width(about), 8)
			text, at := inputView(p.in.text, p.in.cur, inW)
			n := cv.put(x, 0, text, sPlain, inW)
			cx, cy = x+at, 0
			x += max(n, 1)
			if p.err != "" {
				x += cv.put(x, 0, tail, sErr, room())
			} else {
				x += cv.put(x, 0, tail, sMuted, room())
			}
			cv.put(x, 0, about, sSubtle, room())
		}
		if m.mode == modeAddHost {
			pill(cv, x, 0, "ADD HOST", sFoam)
		}
	}
	return cx, cy
}

// drawFrame draws the rules and the dividers between columns.
func (m *Model) drawFrame(cv *canvas, g geometry) {
	top := []rune(strings.Repeat("─", g.w))
	bot := []rune(strings.Repeat("─", g.w))
	for i := 0; i < 4; i++ {
		c := g.cols[i]
		if c.w == 0 || c.x == 0 {
			continue
		}
		x := c.x - 1
		top[x], bot[x] = '┬', '┴'
		for y := g.headY; y < g.footY-1; y++ {
			cv.put(x, y, "│", sRule, 1)
		}
	}
	cv.put(0, 3, string(top), sRule, g.w)
	cv.put(0, g.footY-1, string(bot), sRule, g.w)
}

// scrollbar draws a thumb on the divider right of a column that
// overflows.
func scrollbar(cv *canvas, x, y, h, top, n int) {
	if n <= h || h <= 0 || x >= cv.w {
		return
	}
	size := max(h*h/n, 1)
	at := (h - size) * top / max(n-h, 1)
	for i := range size {
		cv.put(x, y+at+i, "┃", sSubtle, 1)
	}
}

// rowSel is how a row shows the cursor.
type rowSel int

const (
	selNone   rowSel = iota
	selFocus         // the focused column's cursor
	selMemory        // another column's remembered row
)

// frame paints a row's background and cursor bar.
func frame(cv *canvas, x, y, w int, sel rowSel) {
	switch sel {
	case selFocus:
		cv.paint(x, y, w, cOverlay)
		cv.put(x, y, glyphBar, fg(cIris).On(cOverlay), 1)
	case selMemory:
		cv.paint(x, y, w, cSurface)
		cv.put(x, y, glyphBar, fg(cHLHigh).On(cSurface), 1)
	}
}

// hitStyle is base, or the match style at a matched index.
func hitStyle(base style, hits []int) func(int) style {
	set := map[int]bool{}
	for _, h := range hits {
		set[h] = true
	}
	return func(i int) style {
		if i >= 0 && set[i] {
			return sHit
		}
		if i < 0 {
			return sMuted
		}
		return base
	}
}

// putRight writes s so that it ends at end (exclusive) and returns where
// it starts.
func putRight(cv *canvas, end, y int, s string, st style) int {
	x := end - width(s)
	cv.put(x, y, s, st, -1)
	return x
}

// --- the columns ---

func (m *Model) drawColumns(cv *canvas, g geometry) {
	if m.w == nil {
		cv.put(2, g.bodyY, "reading the view…", sMuted, -1)
		return
	}
	hl := m.hostList()
	h := hl.cur()
	el := m.entryList(m.hostItem(h))
	e := el.cur()
	wl := m.windowList(e)
	m.drawHosts(cv, g, g.cols[0], &hl)
	m.drawEntries(cv, g, g.cols[1], m.hostItem(h), &el)
	m.drawWindows(cv, g, g.cols[2], e, &wl)
}

// hostItem is the world's item for a listed host.
func (m *Model) hostItem(h *item) *item {
	if h == nil {
		return nil
	}
	return m.w.host(h.host.Name)
}

func (m *Model) colHeader(cv *canvas, g geometry, c col, s span, extra string) {
	st, kst := sSubtle.Bold(), sMuted
	if m.focus == c {
		st, kst = sIris.Bold(), sKey
	}
	end := s.x + s.w - 1
	if extra != "" {
		// A cached list says so on the right, in red: what it shows may be
		// gone.
		end = putRight(cv, end, g.headY, extra, sErr) - 1
	}
	x := s.x + 1
	x += cv.put(x, g.headY, "["+strconv.Itoa(int(c)+1)+"] ", kst, max(end-x, 0))
	x += cv.put(x, g.headY, colNames[c], st, max(end-x, 0))
	if q := m.cs[c].in.String(); q != "" {
		cv.put(x, g.headY, " /"+q, sGold, max(end-x, 0))
	}
}

func (m *Model) emptyText(c col) string {
	if len(m.cs[c].in.text) > 0 {
		if m.mode == modeSearch {
			return "no match · ^c clears"
		}
		return "no match · esc clears"
	}
	return ""
}

func (m *Model) rowSelFor(c col, i, at int) rowSel {
	if i != at {
		return selNone
	}
	if m.focus == c && (m.mode == modeNormal || m.mode == modeSearch) {
		return selFocus
	}
	return selMemory
}

// hostRightText is what a host row shows on its right: its session count
// (or matches), a spinner, its last-seen age, off, or a failed check.
func (m *Model) hostRightText(it *item) string {
	s, _ := m.hostRight(it)
	return s
}

func (m *Model) hostRight(it *item) (string, style) {
	h := it.host
	if c := m.checks[h.Name]; c != nil {
		if c.running {
			return spinner[m.spin%len(spinner)], sGold
		}
		if c.failed() {
			return glyphWarn, sErr.Bold()
		}
	}
	switch {
	case loading(h):
		return spinner[m.spin%len(spinner)], sGold
	case h.Status == proto.StatusOff:
		return "off", sMuted
	case h.Status == proto.StatusFailed || h.Status == proto.StatusDup:
		return glyphWarn, sErr.Bold()
	case !h.Reachable():
		if h.Seen > 0 {
			return age(time.Duration(h.Seen) * time.Millisecond), sErr
		}
		return glyphWarn, sErr
	}
	if len(m.cs[colSessions].in.text) > 0 && m.w != nil {
		n := len(filter(m.w.entries(h.Name, m.allDirs), m.cs[colSessions].in.text).items)
		if !h.Reachable() {
			return strconv.Itoa(n), sErr // matches in a cached list
		}
		return strconv.Itoa(n), sGold
	}
	return strconv.Itoa(len(h.Sessions)), sMuted
}

func (m *Model) drawHosts(cv *canvas, g geometry, s span, l *list) {
	m.colHeader(cv, g, colHosts, s, "")
	if s.w < 6 {
		return
	}
	n := len(l.items)
	var pending []*hostCheck
	for name, c := range m.checks {
		if m.w.host(name) == nil && c.running {
			pending = append(pending, c)
		}
	}
	total := n + len(pending)
	if len(m.w.hosts) == 1 {
		total++
	}
	at := max(l.at, 0)
	m.cs[colHosts].top = scrollTo(m.cs[colHosts].top, at, g.bodyH, total)
	top := m.cs[colHosts].top
	if n == 0 && len(m.w.hosts) > 0 {
		cv.put(s.x+2, g.bodyY, m.emptyText(colHosts), sMuted, s.w-2)
	}
	matching := len(m.cs[colSessions].in.text) > 0
	y := g.bodyY
	for i := top; i < n && y < g.bodyY+g.bodyH; i++ {
		it := &l.items[i]
		h := it.host
		end := s.x + s.w - 1
		rt, rst := m.hostRight(it)
		rx := putRight(cv, end, y, rt, rst)
		if it.bell {
			rx = putRight(cv, rx-1, y, glyphBell, sGold)
		} else if it.act {
			rx = putRight(cv, rx-1, y, glyphAct, sFoam)
		}
		logo := sFoam
		name := sPlain
		switch {
		case h.Status == proto.StatusOff:
			logo, name = sMuted, sMuted
		case !h.Reachable():
			logo, name = sErr, sMuted // down: the logo says so
		case it.local:
			logo = sRose
		}
		if matching && rt == "0" {
			name = sMuted
		}
		sel := m.rowSelFor(colHosts, i, l.at)
		if sel == selFocus {
			name = name.Bold()
		}
		x := s.x + 2
		x += cv.put(x, y, osGlyph(h.OS), logo, 1) + 1
		room := rx - 1 - x
		if it.cur {
			room -= 2
		}
		x += cv.putFitted(x, y, fitMiddle(graphemes(h.Name), max(room, 1), firstHit(l.hits[i])), hitStyle(name, l.hits[i]))
		if it.cur {
			cv.put(x+1, y, glyphCur, sFoam, 1)
		}
		frame(cv, s.x, y, s.w, sel)
		y++
	}
	for _, c := range pending {
		if y >= g.bodyY+g.bodyH {
			break
		}
		putRight(cv, s.x+s.w-1, y, spinner[m.spin%len(spinner)], sGold)
		cv.put(s.x+2, y, glyphServer, sMuted, 1)
		cv.put(s.x+4, y, cutMiddleText(c.host.Name, s.w-8), sSubtle, s.w-8)
		y++
	}
	if len(m.w.hosts) == 1 && y < g.bodyY+g.bodyH && m.hostEditable(nil) == "" {
		y++
		if y < g.bodyY+g.bodyH {
			cv.put(s.x+2, y, "a", sKey, 1)
			cv.put(s.x+4, y, "add a host", sMuted, s.w-5)
		}
	}
	scrollbar(cv, s.x+s.w, g.bodyY, g.bodyH, top, total)
}

// entryLines are the sessions column's lines: sessions, then (when the
// host has dirs) a blank, the dirs header and a blank, then the dirs.
// line(i) is entry i's line.
func (m *Model) entryLines(h *item, l *list) (line func(int) int, header int, total int) {
	ns := 0
	for ns < len(l.items) && l.items[ns].kind == kSession {
		ns++
	}
	header = -1
	if h != nil && len(m.w.dirs[h.host.Name]) > 0 {
		header = ns + 1
	}
	line = func(i int) int {
		if header >= 0 && i >= ns {
			return i + 3
		}
		return i
	}
	total = len(l.items)
	if header >= 0 {
		total += 3
	}
	return line, header, total
}

func (m *Model) drawEntries(cv *canvas, g geometry, s span, h *item, l *list) {
	extra := ""
	if h != nil && !h.host.Reachable() && len(h.host.Sessions) > 0 {
		extra = "cached"
		if h.host.Seen > 0 {
			extra += " " + age(time.Duration(h.host.Seen)*time.Millisecond)
		}
	}
	m.colHeader(cv, g, colSessions, s, extra)
	if s.w < 8 || h == nil {
		return
	}
	line, header, total := m.entryLines(h, l)
	at := line(max(l.at, 0))
	m.cs[colSessions].top = scrollTo(m.cs[colSessions].top, at, g.bodyH, total)
	top := m.cs[colSessions].top
	if len(l.items) == 0 {
		msg, mst := m.emptyText(colSessions), sMuted
		hint := func(key, what string) {
			cv.put(s.x+2, g.bodyY+1, key, sKey, s.w-2)
			cv.put(s.x+3+width(key), g.bodyY+1, what, sMuted, s.w-3-width(key))
		}
		switch {
		case msg != "":
		case loading(h.host):
			msg, mst = spinner[m.spin%len(spinner)]+" loading sessions…", sGold
		case h.host.Reachable():
			msg = "no sessions"
			hint("n", "makes one")
		case h.host.Status == proto.StatusOff:
			msg = "turned off"
			hint("space", "on the host turns it on")
		default:
			msg, mst = glyphRefused+" "+hostStatus(h.host), sErr
			hint("⏎", "on the host retries")
		}
		cv.put(s.x+2, g.bodyY, msg, mst, s.w-2)
	}
	// Marker slots, as wide as the widest in the list.
	clientsW, flags, ageW := 0, false, 0
	for i := range l.items {
		it := &l.items[i]
		if it.kind != kSession {
			continue
		}
		if it.others > 0 {
			clientsW = max(clientsW, width(glyphClients+" "+strconv.Itoa(it.others)))
		}
		flags = flags || it.bell || it.act
		ageW = max(ageW, width(m.ageText(it)))
	}
	for i := range l.items {
		y := g.bodyY + line(i) - top
		if y < g.bodyY || y >= g.bodyY+g.bodyH {
			continue
		}
		it := &l.items[i]
		end := s.x + s.w - 1
		sel := m.rowSelFor(colSessions, i, l.at)
		if it.kind == kDir {
			m.drawDir(cv, s.x, y, end, it, l.hits[i], sel == selFocus)
		} else {
			m.drawSession(cv, s.x, y, end, it, l.hits[i], clientsW, flags, ageW, sel == selFocus)
		}
		frame(cv, s.x, y, s.w, sel)
	}
	if header >= 0 {
		if y := g.bodyY + header - top; y >= g.bodyY && y < g.bodyY+g.bodyH {
			label := "dirs · git roots"
			if m.allDirs {
				label = "dirs · every zoxide entry"
			}
			cv.put(s.x+2, y, label, sMuted.Bold(), s.w-6)
			putRight(cv, s.x+s.w-1, y, "^g", sMuted)
		}
	}
	scrollbar(cv, s.x+s.w, g.bodyY, g.bodyH, top, total)
}

func (m *Model) ageText(it *item) string {
	if it.isNew {
		return "new"
	}
	return age(it.ago)
}

func (m *Model) drawSession(cv *canvas, x, y, end int, it *item, hits []int, clientsW int, flags bool, ageW int, cursor bool) {
	ast := sSubtle
	if !it.host.Reachable() {
		ast = sMuted
	}
	a := m.ageText(it)
	if it.isNew {
		ast = sFoam
	}
	rx := end - ageW
	cv.put(rx+ageW-width(a), y, a, ast, -1)
	if flags {
		rx -= 2
		switch {
		case it.bell:
			cv.put(rx, y, glyphBell, sGold, 1)
		case it.act:
			cv.put(rx, y, glyphAct, sFoam, 1)
		}
	}
	if clientsW > 0 {
		rx -= clientsW + 1
		if it.others > 0 {
			cv.put(rx, y, glyphClients+" "+strconv.Itoa(it.others), sGold, -1)
		}
	}
	nx := x + 2
	icon := fg(cPine)
	if cursor {
		icon = sIris
	}
	nx += cv.put(nx, y, glyphSession, icon, 1) + 1
	room := rx - 1 - nx
	if it.sess.Group != "" {
		room -= 2
	}
	if it.cur {
		room -= 2
	}
	m.drawName(cv, nx, y, max(room, 1), it, hits, cursor)
}

// drawName draws a session's name fitted to room: a linked worktree's
// repo part dimmed and shrunk first.
func (m *Model) drawName(cv *canvas, x, y, room int, it *item, hits []int, cursor bool) int {
	base := sPlain
	if !it.host.Reachable() {
		base = sSubtle
	}
	if cursor {
		base = base.Bold()
	}
	gs := graphemes(it.sess.Name)
	repo := worktreeRepo(it)
	var f fitted
	if repo > 0 {
		f = fitWorktree(gs, repo, room, hits)
	} else {
		f = fitMiddle(gs, room, firstHit(hits))
	}
	hs := hitStyle(base, hits)
	n := cv.putFitted(x, y, f, func(i int) style {
		st := hs(i)
		if i >= 0 && i < repo && st == base {
			return sMuted
		}
		return st
	})
	if it.sess.Group != "" {
		n += cv.put(x+n, y, " "+glyphGroup, sMuted, 2)
	}
	if it.cur {
		n += cv.put(x+n, y, " "+glyphCur, sFoam, 2)
	}
	return n
}

func (m *Model) drawDir(cv *canvas, x, y, end int, it *item, hits []int, cursor bool) {
	d := it.dir
	rx := end
	if b := gitText(d.Git); b != "" {
		rx = putRight(cv, end, y, b, sMuted)
		if d.Git.Dirty {
			cv.put(end-1, y, "*", sGold, 1)
		}
	}
	// Dirs sit below the sessions in weight: subtle, a non-git dir muted.
	base, icon := sSubtle, sSubtle
	if !d.Root || !it.host.Reachable() {
		base, icon = sMuted, sMuted
	}
	if cursor {
		base, icon = base.Bold(), sIris
	}
	nx := x + 2
	nx += cv.put(nx, y, glyphDir, icon, 1) + 1
	cv.putFitted(nx, y, fitPath(d.Path, max(rx-1-nx, 1), hits), hitStyle(base, hits))
}

func (m *Model) drawWindows(cv *canvas, g geometry, s span, e *item, l *list) {
	m.colHeader(cv, g, colWindows, s, "")
	if s.w < 6 {
		return
	}
	switch {
	case e == nil:
		return
	case e.kind == kDir:
		cv.put(s.x+2, g.bodyY, "no session yet", sMuted, s.w-2)
		return
	case len(l.items) == 0:
		cv.put(s.x+2, g.bodyY, m.emptyText(colWindows), sMuted, s.w-2)
		return
	}
	idxW, splitW, flags := 1, 0, false
	for i := range l.items {
		w := l.items[i].win
		idxW = max(idxW, width(strconv.Itoa(w.Index)))
		if w.Panes > 1 {
			splitW = max(splitW, 2+width(strconv.Itoa(w.Panes)))
		}
		flags = flags || w.Bell || w.Activity
	}
	m.cs[colWindows].top = scrollTo(m.cs[colWindows].top, max(l.at, 0), g.bodyH, len(l.items))
	top := m.cs[colWindows].top
	for i := top; i < len(l.items) && i-top < g.bodyH; i++ {
		y := g.bodyY + i - top
		it := &l.items[i]
		w := it.win
		sel := m.rowSelFor(colWindows, i, l.at)
		rx := s.x + s.w - 1
		if splitW > 0 {
			rx -= splitW
			if w.Panes > 1 {
				cv.put(rx, y, glyphSplit+" "+strconv.Itoa(w.Panes), sMuted, -1)
			}
		}
		if flags {
			rx -= 2
			switch {
			case w.Bell:
				cv.put(rx, y, glyphBell, sGold, 1)
			case w.Activity:
				cv.put(rx, y, glyphAct, sFoam, 1)
			}
		}
		x := s.x + 2
		idx := strconv.Itoa(w.Index)
		ist := sMuted
		if sel == selFocus {
			ist = sIris.Bold()
		}
		cv.put(x+idxW-width(idx), y, idx, ist, -1)
		x += idxW + 1
		x += cv.put(x, y, glyphWindow, sMuted, 1) + 1
		room := rx - 1 - x
		if it.cur {
			room -= 2
		}
		base := sPlain
		if !it.host.Reachable() {
			base = sSubtle
		}
		if sel == selFocus {
			base = base.Bold()
		}
		x += cv.putFitted(x, y, fitMiddle(graphemes(w.Name), max(room, 1), firstHit(l.hits[i])), hitStyle(base, l.hits[i]))
		if it.cur {
			cv.put(x+1, y, glyphCur, sFoam, 1)
		}
		frame(cv, s.x, y, s.w, sel)
	}
	scrollbar(cv, s.x+s.w, g.bodyY, g.bodyH, top, len(l.items))
}

// --- the finder ---

func (m *Model) drawFinder(cv *canvas, g geometry) {
	s := g.cols[0]
	head := "every host"
	n := 0
	if m.w != nil {
		for _, h := range m.w.hosts {
			if loading(h.host) {
				n++
			}
		}
	}
	if n > 0 {
		head += " · " + strconv.Itoa(n) + " loading"
	}
	x := s.x + 1
	x += cv.put(x, g.headY, "[f] ", sKey, -1)
	cv.put(x, g.headY, head, sPlain.Bold(), s.w-x)
	f := &m.find
	if len(f.rows) == 0 {
		msg := "no sessions"
		switch {
		case m.w == nil:
			msg = "reading the view…"
		case len(f.in.text) > 0:
			msg = "no match · ^c clears"
		}
		cv.put(s.x+2, g.bodyY, msg, sMuted, s.w-2)
		return
	}
	at := max(f.at(), 0)
	f.top = scrollTo(f.top, at, g.bodyH, len(f.rows))
	hostW := m.findHostWidth()
	clientsW, winsW, flags, ageW := 0, 0, false, 0
	for i := range f.rows {
		r := &f.rows[i]
		if r.kind != fSession {
			continue
		}
		if r.it.others > 0 {
			clientsW = max(clientsW, width(glyphClients+" "+strconv.Itoa(r.it.others)))
		}
		if n := len(r.it.sess.Windows); n > 1 {
			winsW = max(winsW, width(glyphWindow+" "+strconv.Itoa(n)))
		}
		flags = flags || r.it.bell || r.it.act
		ageW = max(ageW, width(m.ageText(r.it)))
	}
	for i := f.top; i < len(f.rows) && i-f.top < g.bodyH; i++ {
		y := g.bodyY + i - f.top
		r := &f.rows[i]
		end := s.x + s.w - 1
		reach := r.it.host.Reachable()
		cursor := i == at
		nx := s.x + 2 + hostW + 2
		if r.kind == fSession || r.fold || r.kind == fDir {
			hst := sFoam
			switch {
			case !reach:
				hst = sMuted
			case r.it.local:
				hst = sRose
			}
			cv.putFitted(s.x+2, y, fitMiddle(graphemes(r.it.host.Name), hostW, firstHit(r.hostHits)), hitStyle(hst, r.hostHits))
		}
		switch r.kind {
		case fSession:
			rx := end - ageW
			a := m.ageText(r.it)
			ast := sSubtle
			if !reach {
				ast = sMuted
			}
			cv.put(rx+ageW-width(a), y, a, ast, -1)
			if flags {
				rx -= 2
				switch {
				case r.it.bell:
					cv.put(rx, y, glyphBell, sGold, 1)
				case r.it.act:
					cv.put(rx, y, glyphAct, sFoam, 1)
				}
			}
			if winsW > 0 {
				rx -= winsW + 1
				if n := len(r.it.sess.Windows); n > 1 {
					cv.put(rx, y, glyphWindow+" "+strconv.Itoa(n), sMuted, -1)
				}
			}
			if clientsW > 0 {
				rx -= clientsW + 1
				if r.it.others > 0 {
					cv.put(rx, y, glyphClients+" "+strconv.Itoa(r.it.others), sGold, -1)
				}
			}
			room := rx - 1 - nx
			if r.it.sess.Group != "" {
				room -= 2
			}
			if r.it.cur {
				room -= 2
			}
			m.drawName(cv, nx, y, max(room, 1), r.it, r.nameHits, cursor)
		case fWindow:
			w := r.win.win
			rx := end
			if w.Panes > 1 {
				rx = putRight(cv, end, y, glyphSplit+" "+strconv.Itoa(w.Panes), sMuted)
			}
			switch {
			case w.Bell:
				rx = putRight(cv, rx-1, y, glyphBell, sGold)
			case w.Activity:
				rx = putRight(cv, rx-1, y, glyphAct, sFoam)
			}
			x := nx
			base := sPlain
			if r.dim || !reach {
				base = sMuted
			}
			if cursor {
				base = base.Bold()
			}
			if r.fold {
				x += cv.putFitted(x, y, fitMiddle(graphemes(r.it.sess.Name), max((rx-x)/2, 4), -1), func(int) style { return sSubtle })
				x += cv.put(x, y, " "+glyphCrumbSep+" ", sMuted, -1)
			} else {
				x += cv.put(x, y, glyphSubRow+" ", sMuted, -1)
			}
			idx := strconv.Itoa(w.Index)
			ist := sMuted
			switch {
			case r.numHit:
				ist = sHit
			case cursor:
				ist = sIris.Bold()
			}
			x += cv.put(x, y, idx, ist, -1)
			x += cv.put(x, y, ":", sMuted, -1)
			x += cv.putFitted(x, y, fitMiddle(graphemes(w.Name), max(rx-1-x, 1), firstHit(r.winHits)), hitStyle(base, r.winHits))
			if r.win.cur {
				cv.put(x+1, y, glyphCur, sFoam, 1)
			}
		case fMore:
			x := nx + cv.put(nx, y, glyphSubRow+" +"+strconv.Itoa(r.more)+" more", sMuted, end-nx)
			cv.put(x, y, " · tab", sMuted, end-x)
		case fDir:
			m.drawDir(cv, nx-2, y, end, r.it, r.nameHits, cursor)
		}
		sel := selNone
		if i == at {
			sel = selFocus
		}
		frame(cv, s.x, y, s.w, sel)
	}
	scrollbar(cv, s.x+s.w, g.bodyY, g.bodyH, f.top, len(f.rows))
}

// --- the breadcrumb ---

// crumbSubject is what the breadcrumb names: host, session or dir, and
// window.
func (m *Model) crumbSubject() (h *item, e *item, w *item) {
	if m.w == nil {
		return nil, nil, nil
	}
	if m.geo.finder {
		r := m.find.selected()
		if r == nil {
			return nil, nil, nil
		}
		h = m.w.host(r.it.host.Name)
		if r.kind == fDir {
			return h, r.it, nil
		}
		w = r.win
		if w == nil {
			w = m.sessionWindow(r.it)
		}
		return h, r.it, w
	}
	h = m.hostItem(m.hostList().cur())
	e = m.entryList(h).cur()
	return h, e, m.windowList(e).cur()
}

func (m *Model) linkState(h *item) (full, icon string, st style) {
	hh := h.host
	if c := m.checks[hh.Name]; c != nil {
		if c.running {
			sp := spinner[m.spin%len(spinner)]
			return sp + " " + c.step.Name + ": " + c.step.Detail, sp, sGold
		}
		if c.failed() {
			return glyphWarn + " " + c.reason() + " · ⏎ checks again", glyphWarn, sErr
		}
	}
	switch hh.Status {
	case proto.StatusLocal:
		return glyphLink + " local server", glyphLink, sRose
	case proto.StatusUp:
		s := glyphLink + " connected"
		if hh.RTT > 0 {
			s += " · " + strconv.FormatInt(hh.RTT, 10) + "ms"
		}
		return s, glyphLink, sFoam
	case proto.StatusConnecting:
		sp := spinner[m.spin%len(spinner)]
		return sp + " connecting…", sp, sGold
	case proto.StatusInstalling:
		sp := spinner[m.spin%len(spinner)]
		return sp + " installing tower…", sp, sGold
	case proto.StatusStalled:
		return glyphUnlink + " not responding", glyphUnlink, sErr
	case proto.StatusOff:
		return glyphUnlink + " turned off · space turns it on", glyphUnlink, sMuted
	case proto.StatusDown:
		s := glyphUnlink + " unreachable"
		if hh.Seen > 0 {
			s += " · last seen " + age(time.Duration(hh.Seen)*time.Millisecond) + " ago"
		}
		if hh.Reason != "" {
			s += " · " + hh.Reason
		}
		return s + " · ⏎ retries", glyphUnlink, sErr
	}
	return glyphUnlink + " " + hostStatus(hh), glyphUnlink, sErr
}

func (m *Model) drawCrumb(cv *canvas, g geometry) {
	h, e, w := m.crumbSubject()
	if h == nil {
		return
	}
	full, icon, lst := m.linkState(h)
	hostPart := osGlyph(h.host.OS) + " " + h.host.Name
	var sess, win, branch, group, clients, pane string
	var dirPath string
	if e != nil {
		switch e.kind {
		case kSession:
			sess = e.sess.Name
			branch = gitText(e.sess.Git)
			if peers := m.w.groupPeers(e); len(peers) > 0 {
				group = "grouped with " + peers[0]
			}
			if e.others > 0 {
				clients = "also on " + plural(e.others, "other client")
			}
		case kDir:
			dirPath = e.dir.Path
			branch = gitText(e.dir.Git)
		}
	}
	if w != nil {
		win = winLabel(w)
		if g.cols[3].w == 0 {
			if p := m.sel.pane[w.key]; p != "" {
				pane = p
			}
		}
	}
	avail := g.w - 2
	build := func(link string, dropClients, dropGroup, dropBranch bool, nameRoom int) int {
		n := width(hostPart) + 2
		if sess != "" {
			n += 2 + min(width(sess), nameRoom) + 2
		}
		if dirPath != "" {
			n += 2 + min(width(dirPath), nameRoom) + 2
		}
		if win != "" {
			n += 2 + min(width(win), nameRoom) + 2
		}
		if pane != "" {
			n += 3 + width(pane)
		}
		if branch != "" && !dropBranch {
			n += 2 + width(branch) + 3
		}
		if group != "" && !dropGroup {
			n += width(group) + 3
		}
		if clients != "" && !dropClients {
			n += width(clients) + 3
		}
		return n + width(link) + 1
	}
	link := full
	dropC, dropG, dropB := false, false, false
	nameRoom := 1 << 20
	steps := []func(){
		func() { link = icon },
		func() { dropC = true },
		func() { dropG = true },
		func() { dropB = true },
	}
	for _, step := range steps {
		if build(link, dropC, dropG, dropB, nameRoom) <= avail {
			break
		}
		step()
	}
	if over := build(link, dropC, dropG, dropB, nameRoom) - avail; over > 0 {
		longest := max(width(sess), width(win), width(dirPath))
		nameRoom = max(longest-over, 6)
	}
	y := g.crumbY
	x := 1
	x += cv.put(x, y, hostPart, sIris.Bold(), -1) + 2
	if sess != "" {
		x += cv.put(x, y, glyphSession+" ", sFoam, -1)
		x += cv.put(x, y, fitMiddle(graphemes(sess), nameRoom, -1).String(), sPlain.Bold(), -1) + 2
	}
	if dirPath != "" {
		x += cv.put(x, y, glyphDir+" ", fg(cPine), -1)
		x += cv.put(x, y, fitPath(dirPath, nameRoom, nil).String(), sPlain.Bold(), -1) + 2
	}
	if win != "" {
		x += cv.put(x, y, glyphWindow+" ", sMuted, -1)
		x += cv.put(x, y, fitMiddle(graphemes(win), nameRoom, -1).String(), sPlain, -1)
		if pane != "" {
			x += cv.put(x, y, " · "+pane, sIris, -1)
		}
		x += 2
	}
	if branch != "" && !dropB {
		x += cv.put(x, y, " "+glyphBranch+" "+branch, sGold, -1) + 2
	}
	if group != "" && !dropG {
		x += cv.put(x, y, " "+glyphGroup+" "+group, sMuted, -1) + 2
	}
	if clients != "" && !dropC {
		cv.put(x, y, " "+glyphClients+" "+clients, sGold, -1)
	}
	putRight(cv, g.w-1, y, ansi.Truncate(link, avail/2, glyphMore), lst)
}

// --- the footer ---

type hint struct{ key, label string }

func (m *Model) hints() []hint {
	switch m.mode {
	case modeSearch:
		return []hint{{"type", "filter"}, {"^j ^k", "move"}, {"tab", "column"}, {"⏎", "attach"}, {"esc", "done"}, {"?", "help"}}
	case modeFind:
		esc := "back"
		if m.find.front {
			esc = "quit"
		}
		return []hint{{"^j ^k", "move"}, {"⏎", "attach"}, {"tab", "windows"}, {"^l", "columns"}, {"^x", "kill"},
			{"^g", "dirs"}, {"^c", "clear"}, {"esc", esc}}
	case modePrompt:
		return []hint{{"⏎", "ok"}, {"esc", "cancel"}, {"^u", "clear"}}
	case modeConfirm:
		return []hint{{"y", "yes"}, {"any other key", "no"}}
	case modeAddHost:
		return []hint{{"^j ^k", "move"}, {"⏎", "add"}, {"esc", "cancel"}}
	case modeHelp:
		return []hint{{"any key", "closes"}}
	}
	hs := []hint{{"h l", "column"}, {"j k", "move"}}
	editable := m.hostEditable(nil) == ""
	switch m.focus {
	case colHosts:
		h := m.curHost()
		enter := "open"
		if h != nil {
			switch {
			case m.checks[h.host.Name] != nil && m.checks[h.host.Name].failed():
				enter = "check again"
			case h.host.Status == proto.StatusDown || h.host.Status == proto.StatusFailed:
				enter = "reconnect"
			}
		}
		hs = append(hs, hint{"⏎", enter})
		if editable {
			hs = append(hs, hint{"a", "add"})
			if h != nil && !h.local {
				hs = append(hs, hint{"r", "rename"}, hint{"x", "remove"}, hint{"space", "on/off"})
			}
		}
	case colSessions:
		e := m.curEntry()
		switch {
		case e == nil:
			hs = append(hs, hint{"n", "new"})
		case e.kind == kDir:
			hs = append(hs, hint{"⏎", "new session"}, hint{"r", "named session"})
		default:
			enter := "attach"
			if !e.host.Reachable() {
				enter = "reconnect"
			}
			hs = append(hs, hint{"⏎", enter}, hint{"-", "prev"}, hint{".", "current"}, hint{"n", "new"},
				hint{"r", "rename"}, hint{"x", "kill"}, hint{"D", "dup"})
		}
		hs = append(hs, hint{"^g", "dirs"})
	case colWindows:
		hs = append(hs, hint{"⏎", "attach"}, hint{"J K", "pane"}, hint{"n", "new window"}, hint{"r", "rename"}, hint{"x", "kill"})
	}
	return append(hs, hint{"/", "search"}, hint{"f", "find"}, hint{"?", "help"})
}

func (m *Model) drawFooter(cv *canvas, g geometry) {
	y := g.footY
	var right string
	rst := sSubtle
	switch {
	case m.note.text != "":
		right = m.note.text
		if m.note.kind == noteErr {
			rst = sErr
		}
	case m.pendingG:
		right, rst = "g", sGold
	case m.mode == modeNormal:
		right, rst = "q quit", sMuted
	}
	rx := g.w - 1
	if m.note.text != "" {
		// A message is a pill: errors on love, the rest on iris.
		bg := cIris
		if m.note.kind == noteErr {
			bg = cLove
		}
		right = " " + cutMiddleText(right, max(g.w-6, 1)) + " "
		rx = putRight(cv, g.w-1, y, right, fg(cBase).Bold().On(bg)) - 2
	} else if right != "" {
		right = cutMiddleText(right, max(g.w-4, 1))
		rx = putRight(cv, g.w-1, y, right, rst) - 2
	}
	hs := m.hints()
	widthOf := func(h hint) int { return width(h.key) + 2 + 1 + width(h.label) + 2 }
	total := 0
	for _, h := range hs {
		total += widthOf(h)
	}
	// Drop whole hints from the right, keeping ? help.
	for total > rx && len(hs) > 1 {
		i := len(hs) - 1
		if hs[i].key == "?" {
			i--
		}
		total -= widthOf(hs[i])
		hs = append(hs[:i], hs[i+1:]...)
	}
	if total > rx {
		return
	}
	x := 1
	for _, h := range hs {
		x += cv.put(x, y, " "+h.key+" ", sCap, -1) + 1
		x += cv.put(x, y, h.label, sSubtle, -1) + 2
	}
}
