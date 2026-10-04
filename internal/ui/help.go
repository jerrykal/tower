package ui

import (
	"strings"
)

// The key reference (?) and the add-host picker, drawn over the columns.

type helpLine struct{ key, what string }

// markerStyle draws the legend's markers in their own colours.
var markerStyle = map[string]style{
	glyphCur: sFoam, glyphBell: sGold, glyphAct: sFoam,
	glyphClients + " N": sGold, glyphWindow + " N": sMuted, glyphSplit + " N": sMuted,
	glyphGroup: sMuted, glyphBranch + " main*": sGold, "2h": sErr, glyphWarn: sErr.Bold(), "off": sMuted,
}

var helpLeft = []struct {
	title string
	lines []helpLine
}{
	{"columns", []helpLine{
		{"h l  ^h ^l  tab", "switch column"},
		{"1 2 3", "hosts / sessions / windows"},
		{"j k  ^j ^k", "move"},
		{"gg G  ^d ^u", "first / last · half page"},
		{"/ i", "search this column"},
		{"f", "find on every host"},
		{"⏎", "attach · host: open · dir: new"},
		{"J K", "pick the pane ⏎ lands on"},
		{"n", "new session / window"},
		{"r  ^r", "rename · dir: named session"},
		{"x  ^x", "kill session / window"},
		{"D", "duplicate as grouped session"},
		{"- .", "select previous / current"},
		{"a", "add a host"},
		{"x r space", "hosts: remove · rename · on/off"},
		{"^g", "git roots / every zoxide dir"},
		{"esc  ^c", "clear column, all, then quit"},
		{"q", "quit"},
	}},
	{"finder", []helpLine{
		{"type", "match host, session or window"},
		{"tab", "all of a session's windows"},
		{"⏎  ^l", "attach · show in columns"},
		{"^x  ^g", "kill · dirs"},
		{"esc", "back (quit where it opened)"},
	}},
}

var helpRight = []struct {
	title string
	lines []helpLine
}{
	{"search", []helpLine{
		{"type", "filter the focused column"},
		{"^j ^k  ↓ ↑", "move"},
		{"^h ^l  tab", "switch column (own query)"},
		{"← → ^a ^e", "move the text cursor"},
		{"⌫ ^d ^u", "delete back / fwd / to start"},
		{"⏎ ^r ^x ^g", "as in the columns"},
		{"^c", "clear this column's query"},
		{"esc", "back, keeping queries"},
	}},
	{"markers", []helpLine{
		{glyphCur, "you are attached here"},
		{glyphBell, "bell"},
		{glyphAct, "activity"},
		{glyphClients + " N", "N other clients attached"},
		{glyphWindow + " N", "N windows (finder)"},
		{glyphSplit + " N", "N panes"},
		{glyphGroup, "grouped session"},
		{glyphBranch + " main*", "branch · * uncommitted"},
		{"2h", "offline · cached 2h ago"},
		{glyphWarn, "host check failed"},
		{"off", "host turned off"},
	}},
}

func (m *Model) drawHelp(cv *canvas, g geometry) {
	render := func(secs []struct {
		title string
		lines []helpLine
	}) []helpLine {
		var out []helpLine
		for i, s := range secs {
			if i > 0 {
				out = append(out, helpLine{})
			}
			out = append(out, helpLine{key: "\x00" + s.title})
			out = append(out, s.lines...)
		}
		return out
	}
	left, right := render(helpLeft), render(helpRight)
	const keyW, colW = 16, 48
	two := g.w >= 2*colW+4
	lines := left
	if two {
		lines = left
		if len(right) > len(lines) {
			lines = append(lines, make([]helpLine, len(right)-len(left))...)
		}
	} else {
		lines = append(append(append([]helpLine(nil), left...), helpLine{}), right...)
	}
	bw := min(g.w-2, colW+4)
	if two {
		bw = min(g.w-2, 2*colW+4)
	}
	bh := min(len(lines)+2, g.h-2)
	x0, y0 := (g.w-bw)/2, max((g.h-bh)/2, 1)
	for y := y0; y < y0+bh; y++ {
		cv.fill(x0, y, bw, style{})
	}
	box(cv, x0, y0, bw, bh, sIris)
	cv.put(x0+2, y0, " keys ", sIris, -1)
	putRight(cv, x0+bw-2, y0+bh-1, " any key closes ", sMuted)
	draw := func(x, y int, l helpLine) {
		if strings.HasPrefix(l.key, "\x00") {
			cv.put(x, y, l.key[1:], sPlain.Bold(), colW)
			return
		}
		kst, ok := markerStyle[l.key]
		if !ok {
			kst = sPlain.Bold()
		}
		cv.put(x, y, l.key, kst, keyW)
		cv.put(x+keyW, y, l.what, sSubtle, colW-keyW)
	}
	for i := 0; i < bh-2 && i < len(lines); i++ {
		draw(x0+2, y0+1+i, lines[i])
		if two && i < len(right) {
			draw(x0+2+colW, y0+1+i, right[i])
		}
	}
}

// drawPicker draws the add-host picker and returns its text cursor.
func (m *Model) drawPicker(cv *canvas, g geometry) (int, int) {
	p := m.picker
	if p == nil {
		return -1, -1
	}
	bw := min(g.w-4, 60)
	rows := p.rows()
	bh := min(rows+7, g.h-2)
	x0, y0 := (g.w-bw)/2, max((g.h-bh)/2, 1)
	for y := y0; y < y0+bh; y++ {
		cv.fill(x0, y, bw, style{})
	}
	box(cv, x0, y0, bw, bh, sIris)
	cv.put(x0+2, y0, " add host ", sIris, -1)
	inner := bw - 4
	text, at := inputView(p.in.text, len(p.in.text), inner)
	cv.put(x0+2, y0+1, text, sPlain, inner)
	cx, cy := x0+2+at, y0+1
	cv.put(x0+2, y0+2, "ssh config aliases · or type any ssh target", sMuted, inner)
	room := bh - 6
	top := scrollTo(0, p.at, room, rows)
	for i := top; i < rows && i-top < room; i++ {
		y := y0 + 4 + i - top
		if i < len(p.shown) {
			cv.putFitted(x0+4, y, fitMiddle(graphemes(p.shown[i]), inner-2, firstHit(p.hits[i])), hitStyle(sPlain, p.hits[i]))
		} else {
			cv.put(x0+4, y, "+ ssh "+strings.TrimSpace(p.in.String()), sFoam, inner-2)
		}
		if i == p.at {
			frame(cv, x0+2, y, inner, selFocus)
		}
	}
	if rows == 0 {
		cv.put(x0+4, y0+4, "no ssh aliases left to add · type a target", sMuted, inner-2)
	}
	cv.put(x0+2, y0+bh-2, "checks: ssh · tmux 3.2+ · os · tower binary", sMuted, inner)
	return cx, cy
}
