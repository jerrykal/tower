package ui

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// canvas is the screen as a grid of cells: the dashboard draws its
// regions into it, overlays on top, and turns it into one string of lines
// for Bubble Tea. Widths count terminal cells; a wide grapheme takes two
// cells, the second an empty continuation.
type canvas struct {
	w, h  int
	cells []cell
}

type cell struct {
	g  string // the grapheme; "" for the continuation of a wide one
	st style
}

func newCanvas(w, h int) *canvas {
	c := &canvas{w: max(w, 0), h: max(h, 0)}
	c.cells = make([]cell, c.w*c.h)
	for i := range c.cells {
		c.cells[i].g = " "
	}
	return c
}

func (c *canvas) at(x, y int) *cell { return &c.cells[y*c.w+x] }

func (c *canvas) inside(x, y int) bool { return x >= 0 && y >= 0 && x < c.w && y < c.h }

// set puts grapheme g of width gw at (x, y), keeping wide graphemes it
// overwrites whole.
func (c *canvas) set(x, y int, g string, gw int, st style) {
	if !c.inside(x, y) || gw <= 0 {
		return
	}
	if gw == 2 && x+1 >= c.w {
		g, gw = " ", 1
	}
	c.clear(x, y)
	*c.at(x, y) = cell{g: g, st: st}
	if gw == 2 {
		c.clear(x+1, y)
		*c.at(x+1, y) = cell{g: "", st: st}
	}
}

// clear makes room at (x, y): half of a wide grapheme left there becomes
// a space.
func (c *canvas) clear(x, y int) {
	cl := c.at(x, y)
	if cl.g == "" && x > 0 {
		c.at(x-1, y).g = " "
	} else if x+1 < c.w && c.at(x+1, y).g == "" && ansi.StringWidth(cl.g) == 2 {
		c.at(x+1, y).g = " "
	}
}

// put writes s at (x, y) in st, at most maxw cells (maxw < 0: to the
// edge), and returns the cells written.
func (c *canvas) put(x, y int, s string, st style, maxw int) int {
	if maxw < 0 {
		maxw = c.w - x
	}
	n := 0
	for s != "" {
		g, gw := ansi.FirstGraphemeCluster(s, ansi.GraphemeWidth)
		s = s[len(g):]
		if gw == 0 {
			continue
		}
		if n+gw > maxw {
			break
		}
		c.set(x+n, y, g, gw, st)
		n += gw
	}
	return n
}

// fill paints w cells from (x, y) as spaces in st.
func (c *canvas) fill(x, y, w int, st style) {
	for i := range w {
		c.set(x+i, y, " ", 1, st)
	}
}

// paint sets the background of w cells from (x, y), keeping what they
// show.
func (c *canvas) paint(x, y, w int, bg ansi.Color) {
	for i := range w {
		if c.inside(x+i, y) {
			c.at(x+i, y).st.bg = bg
		}
	}
}

// putFitted writes fitted text with a style per source grapheme index.
func (c *canvas) putFitted(x, y int, f fitted, styleAt func(i int) style) int {
	n := 0
	for _, p := range f {
		c.set(x+n, y, p.s, p.w, styleAt(p.at))
		n += p.w
	}
	return n
}

// putANSI writes a line that carries its own SGR sequences (a pane's
// capture) at (x, y), at most maxw cells. Other escape sequences are
// dropped.
func (c *canvas) putANSI(x, y int, s string, maxw int) {
	var st style
	n := 0
	for i := 0; i < len(s) && n < maxw; {
		if s[i] == 0x1b {
			i += escape(s[i:], &st)
			continue
		}
		if s[i] < 0x20 || s[i] == 0x7f {
			i++
			continue
		}
		end := strings.IndexByte(s[i:], 0x1b)
		if end < 0 {
			end = len(s) - i
		}
		text := s[i : i+end]
		for text != "" && n < maxw {
			g, gw := ansi.FirstGraphemeCluster(text, ansi.GraphemeWidth)
			text = text[len(g):]
			if gw == 0 || n+gw > maxw {
				if gw > 0 {
					n = maxw
				}
				continue
			}
			c.set(x+n, y, g, gw, st)
			n += gw
		}
		i += end
	}
}

// escape reads the escape sequence at the start of s, applying an SGR to
// st, and returns its length.
func escape(s string, st *style) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[':
		j := 2
		for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
			j++
		}
		if j == len(s) {
			return j
		}
		if s[j] == 'm' {
			applySGR(s[2:j], st)
		}
		return j + 1
	case ']', 'P', '_', '^':
		for j := 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return len(s)
	}
	return 2
}

// applySGR applies the parameters of one SGR sequence.
func applySGR(params string, st *style) {
	ps := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	if len(ps) == 0 {
		*st = style{}
		return
	}
	num := func(i int) int {
		if i >= len(ps) {
			return -1
		}
		n, err := strconv.Atoi(ps[i])
		if err != nil {
			return -1
		}
		return n
	}
	for i := 0; i < len(ps); i++ {
		p := num(i)
		switch {
		case p == 0:
			*st = style{}
		case p == 1:
			st.bold = true
		case p == 2:
			st.dim = true
		case p == 3:
			st.italic = true
		case p == 4:
			st.underline = true
		case p == 7:
			st.reverse = true
		case p == 22:
			st.bold, st.dim = false, false
		case p == 23:
			st.italic = false
		case p == 24:
			st.underline = false
		case p == 27:
			st.reverse = false
		case p >= 30 && p <= 37:
			st.fg = ansi.BasicColor(p - 30)
		case p == 39:
			st.fg = nil
		case p >= 40 && p <= 47:
			st.bg = ansi.BasicColor(p - 40)
		case p == 49:
			st.bg = nil
		case p >= 90 && p <= 97:
			st.fg = ansi.BasicColor(p - 90 + 8)
		case p >= 100 && p <= 107:
			st.bg = ansi.BasicColor(p - 100 + 8)
		case p == 38 || p == 48:
			var col ansi.Color
			switch num(i + 1) {
			case 5:
				if n := num(i + 2); n >= 0 {
					col = ansi.IndexedColor(n)
				}
				i += 2
			case 2:
				r, g, b := num(i+2), num(i+3), num(i+4)
				if r >= 0 && g >= 0 && b >= 0 {
					col = ansi.TrueColor(uint32(r&0xff)<<16 | uint32(g&0xff)<<8 | uint32(b&0xff))
				}
				i += 4
			}
			if col != nil {
				if p == 38 {
					st.fg = col
				} else {
					st.bg = col
				}
			}
		}
	}
}

// String is the canvas as lines with the fewest style changes.
func (c *canvas) String() string {
	var b strings.Builder
	b.Grow(c.w * c.h * 2)
	for y := range c.h {
		if y > 0 {
			b.WriteByte('\n')
		}
		var cur style
		for x := range c.w {
			cl := c.at(x, y)
			if cl.g == "" {
				continue
			}
			if cl.st != cur {
				b.WriteString(cl.st.sgr())
				cur = cl.st
			}
			b.WriteString(cl.g)
		}
		if cur != (style{}) {
			b.WriteString("\x1b[0m")
		}
	}
	return b.String()
}

// Plain is the canvas's text without styles, lines trimmed (tests).
func (c *canvas) Plain() string {
	var b strings.Builder
	for y := range c.h {
		var l strings.Builder
		for x := range c.w {
			l.WriteString(c.at(x, y).g)
		}
		b.WriteString(strings.TrimRight(l.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}
