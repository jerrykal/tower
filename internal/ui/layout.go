package ui

import (
	"slices"
	"strconv"
)

// Fitting the columns to the screen. Each column's width is the 90th
// percentile of its rows' widths over everything the dashboard knows
// (every host, every session and dir, every window), so one long name is
// left to the long-name fitting rather than widening the column for
// everyone. Widths are taken as the dashboard opens and only grow while
// it stays open; scrolling, filtering and moving never move a divider,
// except the focused column borrowing a few cells from the preview when
// its own rows are cut.

const (
	minHosts, maxHosts  = 16, 30
	minSess, maxSess    = 24, 60
	minWin, maxWin      = 18, 44
	minPreview          = 40
	wideAt              = 120 // narrower, the columns drop the preview
	widen               = 8   // what the focused column may borrow
	minFindList         = 44
	minWidth, minHeight = 40, 8
)

// span is a column's place: x and width.
type span struct{ x, w int }

// geometry is where everything is drawn.
type geometry struct {
	w, h   int
	small  bool
	crumbY int
	headY  int
	bodyY  int
	bodyH  int
	footY  int
	finder bool
	cols   [4]span // hosts, sessions, windows, preview; the finder: [0] the list, [3] the preview
}

func (m *Model) layout() geometry {
	g := geometry{w: max(m.width, 1), h: max(m.height, 1)}
	if g.w < minWidth || g.h < minHeight {
		g.small = true
		return g
	}
	g.crumbY = 2
	g.headY = 4
	g.bodyY = 6
	if g.h < 12 {
		g.bodyY = 5
	}
	g.footY = g.h - 1
	g.bodyH = g.footY - 1 - g.bodyY
	g.finder = m.mode == modeFind || m.back == modeFind && m.mode != modeNormal && m.mode != modeSearch
	if g.finder {
		if g.w >= minFindList+1+minPreview {
			lw := min(max(m.findNatural(), minFindList), g.w-1-minPreview)
			g.cols[0] = span{0, lw}
			g.cols[3] = span{lw + 1, g.w - lw - 1}
		} else {
			g.cols[0] = span{0, g.w}
		}
		return g
	}
	h := min(max(m.widths[0], minHosts), maxHosts)
	s := min(max(m.widths[1], minSess), maxSess)
	wn := min(max(m.widths[2], minWin), maxWin)
	pv := 0
	if g.w >= wideAt {
		for over := h + s + wn + 3 + minPreview - g.w; over > 0; over-- {
			switch {
			case s >= wn && s > minSess:
				s--
			case wn > minWin:
				wn--
			case s > minSess:
				s--
			case h > minHosts:
				h--
			default:
				over = 0
			}
		}
		pv = g.w - h - s - wn - 3
		if extra := min(widen, m.cutOff(m.focus, [3]int{h, s, wn}[m.focus], g.bodyH), pv-minPreview); extra > 0 {
			switch m.focus {
			case colHosts:
				h += extra
			case colSessions:
				s += extra
			default:
				wn += extra
			}
			pv -= extra
		}
	} else {
		h = min(h, max(g.w/5, 10))
		rest := g.w - h - 2
		if rest >= s+minWin {
			wn = rest - s
		} else {
			s = rest * s / (s + wn)
			wn = rest - s
		}
	}
	g.cols[0] = span{0, h}
	g.cols[1] = span{h + 1, s}
	g.cols[2] = span{h + s + 2, wn}
	if pv > 0 {
		g.cols[3] = span{h + s + wn + 3, pv}
	}
	return g
}

func (m *Model) bodyHeight() int {
	g := m.layout()
	return max(g.bodyH, 1)
}

// cutOff is how many cells column c's visible rows (h lines) lack at
// width w.
func (m *Model) cutOff(c col, w, h int) int {
	if m.w == nil {
		return 0
	}
	l := m.columnList(c)
	need := 0
	top := m.cs[c].top
	for i := top; i < len(l.items) && i < top+h; i++ {
		need = max(need, m.natural(c, &l.items[i]))
	}
	return max(need-w, 0)
}

// natural is a row's width uncut, as drawn.
func (m *Model) natural(c col, it *item) int {
	switch c {
	case colHosts:
		n := 4 + width(it.host.Name) + 1 + width(m.hostRightText(it)) + 1
		if it.cur {
			n += 2
		}
		if it.bell || it.act {
			n += 2
		}
		return n
	case colSessions:
		if it.kind == kDir {
			return 4 + width(it.dir.Path) + 2 + width(gitText(it.dir.Git)) + 1
		}
		n := 4 + width(it.sess.Name) + 1 + width(age(it.ago)) + 1
		if it.sess.Group != "" {
			n += 2
		}
		if it.cur {
			n += 2
		}
		if it.others > 0 {
			n += width(glyphClients+" "+strconv.Itoa(it.others)) + 1
		}
		if it.bell || it.act {
			n += 2
		}
		return n
	}
	n := 2 + width(strconv.Itoa(it.win.Index)) + 3 + width(it.win.Name) + 1
	if it.cur {
		n += 2
	}
	if it.bell || it.act {
		n += 2
	}
	if it.win.Panes > 1 {
		n += 3 + width(strconv.Itoa(it.win.Panes))
	}
	return n
}

// grow widens the columns' natural widths to what the world now needs.
func (m *Model) grow() {
	var ws [3][]int
	for i := range m.w.hosts {
		h := &m.w.hosts[i]
		ws[0] = append(ws[0], m.natural(colHosts, h))
		for _, e := range m.w.entries(h.host.Name, true) {
			ws[1] = append(ws[1], m.natural(colSessions, &e))
		}
		for _, s := range m.w.sessions[h.host.Name] {
			for _, wi := range m.w.windows[s.key] {
				ws[2] = append(ws[2], m.natural(colWindows, &wi))
			}
		}
	}
	for c := range ws {
		m.widths[c] = max(m.widths[c], p90(ws[c]))
	}
}

// p90 is the 90th percentile of vs (0 for none).
func p90(vs []int) int {
	if len(vs) == 0 {
		return 0
	}
	slices.Sort(vs)
	i := (len(vs)*9+9)/10 - 1
	return vs[min(max(i, 0), len(vs)-1)]
}

// findNatural is the finder list's width: its widest row.
func (m *Model) findNatural() int {
	hw := m.findHostWidth()
	n := 0
	for i := range m.find.rows {
		r := &m.find.rows[i]
		w := 2 + 2 + hw + 2 + 14
		switch r.kind {
		case fSession:
			w += 2 + width(r.it.sess.Name) + 4
		case fWindow:
			w += width(winLabel(r.win)) + 2
			if r.fold {
				w += 2 + width(r.it.sess.Name) + 3
			}
		case fDir:
			w += width(r.it.dir.Path) + 2 + width(gitText(r.it.dir.Git))
		}
		n = max(n, w)
	}
	return n
}

// findHostWidth is the finder's host column.
func (m *Model) findHostWidth() int {
	n := 4
	if m.w != nil {
		for _, h := range m.w.hosts {
			n = max(n, width(h.host.Name))
		}
	}
	return min(n, 16)
}

// scrollTo keeps the cursor's line two lines from the edges of a column
// of h lines, n lines in all, and returns the new top.
func scrollTo(top, at, h, n int) int {
	if h <= 0 {
		return 0
	}
	margin := min(2, (h-1)/2)
	if at-margin < top {
		top = at - margin
	}
	if at+margin >= top+h {
		top = at + margin - h + 1
	}
	return max(0, min(top, n-h))
}
