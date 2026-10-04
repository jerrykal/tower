package ui

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jerrykal/tower/internal/proto"
)

// The layout preview: the selected window's panes at their real
// proportions, then the picked pane's contents as capture-pane -e prints
// them. Captures travel only on request, for the selected window: one in
// flight at a time; an answer for a window no longer selected is kept
// (moving back shows it at once) and the selected one asked for.

// capKey names one capture: a window (by its session's key and id) and a
// pane ("" for the active one).
type capKey struct {
	sess rowKey
	win  string
	pane string
}

type capture struct {
	text  string
	panes []proto.Pane
	err   string
}

type capState struct {
	busy  bool
	asked capKey // the last capture asked for
	cache map[capKey]*capture
	keys  []capKey                // oldest first
	panes map[rowKey][]proto.Pane // by window key: the panes last heard
}

const capKeep = 64

func (c *capState) put(k capKey, v *capture) {
	if _, ok := c.cache[k]; !ok {
		c.keys = append(c.keys, k)
	}
	c.cache[k] = v
	for len(c.keys) > capKeep {
		delete(c.cache, c.keys[0])
		c.keys = c.keys[1:]
	}
	if len(v.panes) > 0 {
		if c.panes == nil {
			c.panes = map[rowKey][]proto.Pane{}
		}
		wk := k.sess
		wk.Window = k.win
		c.panes[wk] = v.panes
	}
}

// panesOf are the panes last heard of a window.
func (c *capState) panesOf(win rowKey) []proto.Pane { return c.panes[win] }

type captureMsg struct {
	key   capKey
	text  string
	panes []proto.Pane
	err   error
}

// previewWindow is the window the preview shows: the finder's row's (a
// session at its remembered window), or the windows column's.
func (m *Model) previewWindow() *item {
	if m.w == nil {
		return nil
	}
	if m.mode == modeFind || m.back == modeFind && m.mode != modeNormal && m.mode != modeSearch {
		r := m.find.selected()
		switch {
		case r == nil || r.kind == fDir:
			return nil
		case r.win != nil:
			return r.win
		}
		return m.sessionWindow(r.it)
	}
	return m.curWindow()
}

// previewKey is the capture the preview wants.
func (m *Model) previewKey(w *item) capKey {
	return capKey{sess: w.key.sessionKey(), win: w.win.ID, pane: m.sel.pane[w.key]}
}

// wantCapture asks for the preview's capture when it is not the last one
// asked for.
func (m *Model) wantCapture() tea.Cmd {
	w := m.previewWindow()
	if w == nil || m.cap.busy || !w.host.Reachable() {
		return nil
	}
	k := m.previewKey(w)
	if k == m.cap.asked {
		return nil
	}
	m.cap.busy, m.cap.asked = true, k
	t := rowTarget(w)
	t.ref.Pane = k.pane
	c, ctx := m.c, m.ctx
	return func() tea.Msg {
		text, panes, err := c.capture(ctx, t)
		return captureMsg{key: k, text: text, panes: panes, err: err}
	}
}

func (m *Model) gotCapture(msg captureMsg) tea.Cmd {
	m.cap.busy = false
	v := &capture{text: msg.text, panes: msg.panes}
	if msg.err != nil {
		v = &capture{err: msg.err.Error()}
		if old := m.cap.cache[msg.key]; old != nil {
			v.panes = old.panes
		}
	}
	m.cap.put(msg.key, v)
	return m.wantCapture()
}

// drawPreview draws the preview column: its header line at headY, its
// body from y, h lines, w cells.
func (m *Model) drawPreview(cv *canvas, x, headY, y, w, h int) {
	win := m.previewWindow()
	var dir *item
	if m.mode == modeFind || m.back == modeFind {
		if r := m.find.selected(); r != nil && r.kind == fDir {
			dir = r.it
		}
	} else if e := m.curEntry(); e != nil && e.kind == kDir {
		dir = e
	}
	switch {
	case dir != nil:
		cv.put(x+1, headY, "dir", sSubtle, w-1)
		m.drawDirPreview(cv, x+1, y, w-2, h, dir)
		return
	case win == nil:
		cv.put(x+1, headY, "layout", sSubtle, w-1)
		if hi := m.previewHost(); hi != nil {
			m.drawHostPreview(cv, x+1, y, w-2, h, hi)
		}
		return
	}
	k := m.previewKey(win)
	got := m.cap.cache[k]
	panes := m.cap.panesOf(win.key)
	head := "layout · " + winLabel(win)
	switch n := max(len(panes), win.win.Panes); {
	case n > 1:
		head += " · " + strconv.Itoa(n) + " panes"
	case len(panes) == 1:
		head += " · single"
	}
	cv.put(x+1, headY, fitMiddle(graphemes(head), w-1, -1).String(), sSubtle, w-1)
	if !win.host.Reachable() {
		cv.put(x+1, y, win.host.Name+" is "+hostStatus(win.host), sMuted, w-1)
		return
	}
	bodyW := w - 2
	capY, capH := y, h
	if len(panes) > 0 && h >= 12 {
		diaH := min(max(h*2/5, 5), 14)
		m.drawPanes(cv, x+1, y, bodyW, diaH, panes, k.pane)
		capY, capH = y+diaH+1, h-diaH-1
	}
	label := "active pane"
	if k.pane != "" {
		label = "pane " + k.pane
		if i := slices.IndexFunc(panes, func(p proto.Pane) bool { return p.ID == k.pane }); i >= 0 && panes[i].Command != "" {
			label += " · " + panes[i].Command
		}
	}
	lw := cv.put(x+1, capY, label, sSubtle, bodyW)
	if m.mode == modeNormal && len(panes) > 1 && bodyW-lw > 20 {
		putRight(cv, x+1+bodyW, capY, "J K picks a pane", sMuted)
	}
	if capH < 3 {
		return
	}
	box(cv, x+1, capY+1, bodyW, capH-1, sRule)
	inner := capH - 3
	switch {
	case got == nil:
		cv.put(x+3, capY+2, glyphMore, sMuted, bodyW-4)
	case got.err != "":
		cv.put(x+3, capY+2, "capture: "+got.err, sErr, bodyW-4)
	default:
		lines := strings.Split(strings.TrimRight(got.text, "\n"), "\n")
		for len(lines) > 0 && strings.TrimSpace(stripSGR(lines[len(lines)-1])) == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > inner {
			lines = lines[len(lines)-inner:]
		}
		for i, l := range lines {
			cv.putANSI(x+2, capY+2+i, l, bodyW-2)
		}
	}
}

// previewHost is the host whose summary shows when nothing else does.
func (m *Model) previewHost() *item {
	if m.mode == modeFind {
		return nil
	}
	return m.curHost()
}

// box draws a square border.
func box(cv *canvas, x, y, w, h int, st style) {
	if w < 2 || h < 2 {
		return
	}
	cv.put(x, y, "┌"+strings.Repeat("─", w-2)+"┐", st, w)
	for i := 1; i < h-1; i++ {
		cv.put(x, y+i, "│", st, 1)
		cv.put(x+w-1, y+i, "│", st, 1)
	}
	cv.put(x, y+h-1, "└"+strings.Repeat("─", w-2)+"┘", st, w)
}

// drawPanes draws a window's panes at their real proportions; the picked
// pane (the active one unless J K picked another) in the accent colour.
func (m *Model) drawPanes(cv *canvas, x, y, w, h int, panes []proto.Pane, picked string) {
	tw, th := 0, 0
	for _, p := range panes {
		tw = max(tw, p.Left+p.Width)
		th = max(th, p.Top+p.Height)
	}
	if tw == 0 || th == 0 {
		return
	}
	scale := func(v, total, room int) int { return (v*room + total/2) / total }
	order := slices.Clone(panes)
	slices.SortStableFunc(order, func(a, b proto.Pane) int {
		pa := a.ID == picked || picked == "" && a.Active
		pb := b.ID == picked || picked == "" && b.Active
		switch {
		case pa && !pb:
			return 1
		case pb && !pa:
			return -1
		}
		return 0
	})
	for _, p := range order {
		x0 := scale(p.Left, tw+1, w)
		x1 := scale(p.Left+p.Width+1, tw+1, w)
		y0 := scale(p.Top, th+1, h)
		y1 := scale(p.Top+p.Height+1, th+1, h)
		bw, bh := max(x1-x0, 3), max(y1-y0, 3)
		bw, bh = min(bw, w-x0), min(bh, h-y0)
		if bw < 3 || bh < 2 {
			continue
		}
		on := p.ID == picked || picked == "" && p.Active
		st := sRule
		if on {
			st = sIris
		}
		box(cv, x+x0, y+y0, bw, bh, st)
		cv.put(x+x0+1, y+y0, "─ "+p.ID+" ", st, bw-2)
		size := strconv.Itoa(p.Width) + "×" + strconv.Itoa(p.Height)
		if p.Active {
			size = "active · " + size
		}
		if sw := width(size) + 2; sw <= bw-2 {
			cv.put(x+x0+bw-1-sw, y+y0+bh-1, " "+size+" ", st, sw)
		}
		inner := bw - 3
		if bh >= 3 && inner > 0 {
			cmdSt := sPlain
			if on {
				cmdSt = sPlain.Bold()
			}
			cv.put(x+x0+2, y+y0+1, fitMiddle(graphemes(p.Command), inner, -1).String(), cmdSt, inner)
		}
		if bh >= 4 && inner > 0 {
			cv.put(x+x0+2, y+y0+2, fitPath(p.Path, inner, nil).String(), sMuted, inner)
		}
	}
}

// drawDirPreview is a dir's, in a box: its path, branch and what ⏎ would
// make.
func (m *Model) drawDirPreview(cv *canvas, x, y, w, h int, d *item) {
	type line struct {
		text string
		st   style
	}
	lines := []line{{glyphDir + " " + fitPath(d.dir.Path, max(w-6, 1), nil).String(), sPlain.Bold()}}
	if g := gitText(d.dir.Git); g != "" {
		lines = append(lines, line{glyphBranch + " " + g, sGold})
	}
	if d.dir.Net {
		lines = append(lines, line{"on a network mount: not checked", sMuted})
	}
	name := freeName(d.host, dirSessionName(d.dir.Path))
	lines = append(lines, line{}, line{"⏎ new session " + name + " on " + d.host.Name, sSubtle})
	bh := min(len(lines)+2, h)
	if bh < 3 || w < 6 {
		return
	}
	box(cv, x, y, w, bh, sRule)
	for i := 0; i < bh-2; i++ {
		cv.put(x+2, y+1+i, lines[i].text, lines[i].st, w-4)
	}
}

// drawHostPreview is a host's summary.
func (m *Model) drawHostPreview(cv *canvas, x, y, w, h int, hi *item) {
	hh := hi.host
	lines := []string{hh.Name}
	if hh.OS != "" {
		lines = append(lines, "os      "+hh.OS)
	}
	if hh.Tmux != "" {
		lines = append(lines, "tmux    "+hh.Tmux)
	}
	if hh.Version != "" {
		lines = append(lines, "tower   "+hh.Version)
	}
	if s := hostStatus(hh); s != "" {
		lines = append(lines, "status  "+s)
	}
	if c := m.checks[hh.Name]; c != nil && c.failed() {
		lines = append(lines, "check   "+c.reason())
	}
	for i, l := range lines {
		if i >= h {
			break
		}
		st := sSubtle
		if i == 0 {
			st = sIris
		}
		cv.put(x, y+i, l, st, w)
	}
}

// stripSGR drops escape sequences from a line.
func stripSGR(s string) string {
	var b strings.Builder
	var st style
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i += escape(s[i:], &st)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
