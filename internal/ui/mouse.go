package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// The mouse: a click selects a row (and focuses its column), a second
// click on it within doubleClick does what ⏎ does, the wheel moves the
// cursor of the column under the pointer without moving focus. Prompts,
// confirmations, the picker and help ignore it.

const doubleClick = 400 * time.Millisecond

type lastClick struct {
	at  time.Time
	key rowKey
}

func (m *Model) mouseOK() bool {
	return m.mode == modeNormal || m.mode == modeSearch || m.mode == modeFind
}

// rowAt is the row under (x, y): the column and the row's index in its
// list, or -1.
func (m *Model) rowAt(x, y int) (col, int) {
	g := m.geo
	if g.small || y < g.bodyY || y >= g.bodyY+g.bodyH {
		return 0, -1
	}
	line := y - g.bodyY
	if g.finder {
		s := g.cols[0]
		if x < s.x || x >= s.x+s.w {
			return 0, -1
		}
		lineOf, _, _ := m.find.lines()
		for i, l := range lineOf {
			if l == m.find.top+line {
				return 0, i
			}
		}
		return 0, -1
	}
	for c := colHosts; c <= colWindows; c++ {
		s := g.cols[c]
		if x < s.x || x >= s.x+s.w {
			continue
		}
		l := m.columnList(c)
		at := m.cs[c].top + line
		if c == colSessions {
			lineOf, _, _ := m.entryLines(m.curHost(), &l)
			for i := range l.items {
				if lineOf(i) == at {
					return c, i
				}
			}
			return c, -1
		}
		if at < len(l.items) {
			return c, at
		}
		return c, -1
	}
	return 0, -1
}

func (m *Model) mouseClick(ms tea.Mouse) tea.Cmd {
	if ms.Button != tea.MouseLeft || !m.mouseOK() {
		return nil
	}
	c, i := m.rowAt(ms.X, ms.Y)
	if i < 0 {
		return nil
	}
	now := m.now()
	var key rowKey
	if m.geo.finder {
		key = m.find.rows[i].key
		m.find.cursor = key
	} else {
		l := m.columnList(c)
		key = l.items[i].key
		if c == colHosts {
			key = rowKey{Host: "host:" + l.items[i].host.Name}
		}
		m.focus = c
		m.moveTo(c, i)
	}
	double := key == m.click.key && now.Sub(m.click.at) < doubleClick
	m.click = lastClick{at: now, key: key}
	if double {
		m.click = lastClick{}
		if m.geo.finder {
			return m.enterFound()
		}
		return m.enterColumn()
	}
	return m.moved()
}

func (m *Model) mouseWheel(ms tea.Mouse) tea.Cmd {
	if !m.mouseOK() {
		return nil
	}
	d := 0
	switch ms.Button {
	case tea.MouseWheelUp:
		d = -1
	case tea.MouseWheelDown:
		d = 1
	default:
		return nil
	}
	g := m.geo
	if g.finder {
		m.find.move(d)
		return m.moved()
	}
	for c := colHosts; c <= colWindows; c++ {
		s := g.cols[c]
		if ms.X >= s.x && ms.X < s.x+s.w {
			m.moveIn(c, d)
			return m.moved()
		}
	}
	return nil
}
