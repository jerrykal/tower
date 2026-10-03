package ui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Styles are plain SGR sequences, in true colour; Bubble Tea's renderer
// maps them to the terminal's colour profile. (Lip Gloss is not used: its
// package-level writer detects a colour profile as the process starts,
// which inside tmux runs `tmux info` through PATH, in every tower process
// whose stdout is a terminal.)
type style struct {
	fg, bg          ansi.Color
	bold, underline bool
}

func fg(c ansi.Color) style { return style{fg: c} }

func (s style) Bold() style                   { s.bold = true; return s }
func (s style) Underline() style              { s.underline = true; return s }
func (s style) Background(c ansi.Color) style { s.bg = c; return s }

// Render styles t, resetting after it.
func (s style) Render(t string) string {
	if t == "" {
		return ""
	}
	var st ansi.Style
	if s.fg != nil {
		st = st.ForegroundColor(s.fg)
	}
	if s.bg != nil {
		st = st.BackgroundColor(s.bg)
	}
	if s.bold {
		st = st.Bold()
	}
	if s.underline {
		st = st.Underline(true)
	}
	return st.Styled(t)
}

// Rosé Pine.
var (
	cMuted   = ansi.TrueColor(0x6e6a86)
	cSubtle  = ansi.TrueColor(0x908caa)
	cText    = ansi.TrueColor(0xe0def4)
	cLove    = ansi.TrueColor(0xeb6f92)
	cGold    = ansi.TrueColor(0xf6c177)
	cFoam    = ansi.TrueColor(0x9ccfd8)
	cRose    = ansi.TrueColor(0xebbcba)
	cIris    = ansi.TrueColor(0xc4a7e7)
	cOverlay = ansi.TrueColor(0x26233a)
)

var (
	sPlain  = fg(cText)
	sMuted  = fg(cMuted)
	sSubtle = fg(cSubtle)
	sPrompt = fg(cIris).Bold()
	sHit    = fg(cRose).Bold().Underline()
	sErr    = fg(cLove)
	sBar    = fg(cIris)
	sActive = fg(cFoam).Bold()
)

func width(s string) int { return ansi.StringWidth(s) }

// partStyle is how a cell kind looks; the host column takes the host's
// colour: rose for this machine, foam for others, muted when it is not up.
func partStyle(k part, r *row) style {
	switch k {
	case partHost:
		switch {
		case !r.host.Reachable():
			return sMuted
		case r.local:
			return fg(cRose)
		}
		return fg(cFoam)
	case partName:
		if !r.host.Reachable() {
			return sSubtle
		}
		return sPlain
	case partCount, partAge, partNone, partMark:
		return sMuted
	case partCur, partAct:
		return fg(cFoam)
	case partClients, partBell:
		return fg(cGold)
	case partStatus:
		return sErr
	}
	return sPlain
}

// layoutHeights splits the screen: the list, and the preview's capture
// lines (0: no preview). The rest is the prompt, two rules, the preview's
// header and the status line.
func (m *Model) layoutHeights() (list, capture int) {
	if m.height < 8 {
		return max(m.height-2, 1), 0
	}
	avail := m.height - 5
	list = min(max(len(m.shown), 1), max(avail*2/5, min(avail, 3)))
	return list, avail - list
}

func (m *Model) listHeight() int { l, _ := m.layoutHeights(); return l }

// scroll keeps the cursor on screen.
func (m *Model) scroll() {
	l := m.listHeight()
	if m.at < m.top {
		m.top = m.at
	}
	if m.at >= m.top+l {
		m.top = m.at - l + 1
	}
	m.top = max(0, min(m.top, len(m.shown)-l))
}

// View draws the dashboard.
func (m *Model) View() tea.View {
	if m.firstDraw != nil {
		m.firstDraw()
		m.firstDraw = nil
	}
	if m.quitted {
		return tea.NewView("")
	}
	w := max(m.width, 20)
	list, capLines := m.layoutHeights()
	var b strings.Builder

	// The prompt line.
	label, text := m.promptText()
	head := sPrompt.Render(label) + sPlain.Render(text)
	right := ""
	if m.view.Note != "" {
		right = sErr.Render(m.view.Note) + "  "
	}
	if m.mode != modePrompt {
		right += sMuted.Render(strconv.Itoa(len(m.shown)) + "/" + strconv.Itoa(len(m.rows)))
	}
	b.WriteString(spread(head, right, w))
	cursorX := width(label) + width(text)

	// The rows.
	for i := m.top; i < m.top+list; i++ {
		b.WriteByte('\n')
		if i < len(m.shown) {
			b.WriteString(m.drawRow(i, w))
		} else if i == 0 {
			b.WriteString(sMuted.Render(m.emptyText()))
		}
	}

	// The preview.
	if capLines > 0 {
		b.WriteString("\n" + sMuted.Render(strings.Repeat("─", w)))
		header, body := m.preview(capLines, w)
		b.WriteString("\n" + header)
		for _, l := range body {
			b.WriteString("\n" + l)
		}
		for i := len(body); i < capLines; i++ {
			b.WriteByte('\n')
		}
		b.WriteString("\n" + sMuted.Render(strings.Repeat("─", w)))
	}

	// The status line.
	b.WriteString("\n" + m.statusLine(w))

	v := tea.NewView(b.String())
	v.AltScreen = true
	v.Cursor = tea.NewCursor(min(cursorX, w-1), 0)
	v.Cursor.Blink = false
	return v
}

func (m *Model) promptText() (string, string) {
	switch m.mode {
	case modePrompt:
		return m.prompt.label + "> ", string(m.prompt.text)
	case modeWindows:
		return "windows " + m.winLabel() + "> ", string(m.query)
	}
	return "sessions> ", string(m.query)
}

func (m *Model) winLabel() string {
	if t, err := resolve(&m.view, m.winOf); err == nil && t.sess != nil {
		return t.host.Name + ":" + t.sess.Name
	}
	return m.winOf.Session
}

func (m *Model) emptyText() string {
	switch {
	case !m.have:
		return "  reading the view…"
	case len(m.rows) == 0 && m.mode == modeWindows:
		return "  " + errGone.Error()
	case len(m.rows) == 0:
		return "  no hosts"
	}
	return "  no match: ^c clears"
}

// drawRow draws shown row i: its cells in their columns, the query's
// matches marked, the cursor's row on a bar.
func (m *Model) drawRow(i, w int) string {
	r := &m.shown[i]
	sel := i == m.at
	hit := map[int]bool{}
	for _, p := range m.hits[i] {
		hit[p] = true
	}
	bg := func(s style) style {
		if sel {
			return s.Background(cOverlay)
		}
		return s
	}
	var b strings.Builder
	if sel {
		b.WriteString(sBar.Render("▌") + bg(sPlain).Render(" "))
	} else {
		b.WriteString("  ")
	}
	for j, c := range r.cells() {
		if j > 0 {
			b.WriteString(bg(sPlain).Render("  "))
		}
		base := partStyle(c.kind, r)
		if c.off < 0 || len(hit) == 0 {
			b.WriteString(bg(base).Render(c.text))
		} else {
			for k, ch := range []rune(c.text) {
				st := base
				if hit[c.off+k] {
					st = sHit
				}
				b.WriteString(bg(st).Render(string(ch)))
			}
		}
		if j < len(m.cols) {
			if pad := m.cols[j] - width(c.text); pad > 0 {
				b.WriteString(bg(sPlain).Render(strings.Repeat(" ", pad)))
			}
		}
	}
	line := ansi.Truncate(b.String(), w, "…")
	if sel {
		if pad := w - width(line); pad > 0 {
			line += bg(sPlain).Render(strings.Repeat(" ", pad))
		}
	}
	return line
}

// preview is the selected row's header, from the view at once, and its
// capture lines once they came.
func (m *Model) preview(lines, w int) (string, []string) {
	r := m.selected()
	if r == nil {
		return "", nil
	}
	if r.sess == nil {
		return sMuted.Render(ansi.Truncate("no sessions on "+r.host.Name+": ^n makes one", w, "…")), nil
	}
	var b strings.Builder
	b.WriteString(sPrompt.Render(r.host.Name+":"+r.sess.Name) + sMuted.Render("  ("+r.sess.ID+")"))
	shownWin := paneWindow(r)
	for _, win := range r.sess.Windows {
		t := strconv.Itoa(win.Index) + ":" + win.Name
		if win.ID == shownWin {
			b.WriteString("  " + sActive.Render(t))
		} else {
			b.WriteString("  " + sSubtle.Render(t))
		}
	}
	header := ansi.Truncate(b.String(), w, "…")
	switch {
	case !r.host.Reachable():
		return header, []string{sMuted.Render(r.host.Name + " is " + r.status)}
	case m.capKey != r.key || m.capWin != shownWin:
		return header, nil
	case m.capErr != "":
		return header, []string{sErr.Render(ansi.Truncate("capture: "+m.capErr, w, "…"))}
	}
	body := strings.Split(strings.TrimRight(m.capText, "\n"), "\n")
	if len(body) > lines {
		body = body[:lines]
	}
	for i, l := range body {
		body[i] = ansi.Truncate(l, w, "") + "\x1b[0m"
	}
	return header, body
}

const (
	hintList    = "⏎ attach  - prev  ^x kill  ^r rename  ^n new  ^w windows  esc quit"
	hintWindows = "⏎ attach  ^x kill  ^r rename  ^w sessions  esc quit"
	hintPrompt  = "⏎ ok  esc cancel"
)

func (m *Model) statusLine(w int) string {
	var left string
	switch {
	case m.note != "" && m.noteErr:
		left = sErr.Render(m.note)
	case m.note != "":
		left = sSubtle.Render(m.note)
	default:
		if r := m.selected(); r != nil && m.mode != modePrompt {
			left = sMuted.Render(m.enterText(r))
		}
	}
	hint := hintList
	switch m.mode {
	case modeWindows:
		hint = hintWindows
	case modePrompt:
		hint = hintPrompt
	}
	if width(left)+2+width(hint) > w {
		return ansi.Truncate(left, w, "…")
	}
	return spread(left, sMuted.Render(hint), w)
}

// spread puts left and right on one line of width w.
func spread(left, right string, w int) string {
	gap := w - width(left) - width(right)
	if gap < 1 {
		return ansi.Truncate(left+" "+right, w, "…")
	}
	return left + strings.Repeat(" ", gap) + right
}
