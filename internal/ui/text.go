package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Fitting text into cells. Every cut counts terminal cells, so wide
// names keep the columns aligned, and every kept grapheme remembers its
// index in the source, so match highlights carry over.

// gr is one grapheme and its width.
type gr struct {
	s string
	w int
}

// graphemes splits s; zero-width ones are left out.
func graphemes(s string) []gr {
	var out []gr
	for s != "" {
		g, w := ansi.FirstGraphemeCluster(s, ansi.GraphemeWidth)
		s = s[len(g):]
		if w > 0 {
			out = append(out, gr{g, w})
		}
	}
	return out
}

func width(s string) int { return ansi.StringWidth(s) }

// piece is one grapheme of fitted text; at is its index in the source,
// or -1 for an ellipsis.
type piece struct {
	s  string
	w  int
	at int
}

// fitted is text cut to a width.
type fitted []piece

func (f fitted) width() int {
	n := 0
	for _, p := range f {
		n += p.w
	}
	return n
}

func (f fitted) String() string {
	var b strings.Builder
	for _, p := range f {
		b.WriteString(p.s)
	}
	return b.String()
}

var ellipsis = piece{s: glyphMore, w: 1, at: -1}

func grWidth(gs []gr) int {
	n := 0
	for _, g := range gs {
		n += g.w
	}
	return n
}

// pieces is gs[from:to] as pieces.
func pieces(gs []gr, from, to int) fitted {
	f := make(fitted, 0, to-from)
	for i := from; i < to; i++ {
		f = append(f, piece{gs[i].s, gs[i].w, i})
	}
	return f
}

// fitEnd cuts at the end: "train-ll…".
func fitEnd(gs []gr, room int) fitted {
	if grWidth(gs) <= room {
		return pieces(gs, 0, len(gs))
	}
	if room <= 0 {
		return nil
	}
	n, i := 0, 0
	for i < len(gs) && n+gs[i].w <= room-1 {
		n += gs[i].w
		i++
	}
	return append(pieces(gs, 0, i), ellipsis)
}

// fitMiddle keeps both ends: "sweep-lr-…-cosine-v2". When the cut would
// hide hit (the first matched grapheme, -1: none), the visible part
// slides to show it instead: "…warmup-cos…".
func fitMiddle(gs []gr, room int, hit int) fitted {
	if grWidth(gs) <= room {
		return pieces(gs, 0, len(gs))
	}
	if room <= 1 {
		return fitEnd(gs, room)
	}
	budget := room - 1
	headW := (budget + 1) / 2
	tailW := budget - headW
	i, n := 0, 0
	for i < len(gs) && n+gs[i].w <= headW {
		n += gs[i].w
		i++
	}
	j, m := len(gs), 0
	for j > i && m+gs[j-1].w <= tailW+(headW-n) {
		m += gs[j-1].w
		j--
	}
	if hit < 0 || hit < i || hit >= j {
		f := pieces(gs, 0, i)
		f = append(f, ellipsis)
		return append(f, pieces(gs, j, len(gs))...)
	}
	// Slide: start a little before the hit, cut the end.
	start := max(hit-2, 1)
	f := fitted{ellipsis}
	rest := fitEnd(gs[start:], room-1)
	for _, p := range rest {
		if p.at >= 0 {
			p.at += start
		}
		f = append(f, p)
	}
	return f
}

// fitWorktree fits a session named <repo><sep><branch> (a linked
// worktree's): the repo part shrinks first, so the branch survives
// ("dotf…_feat-tower-prototype", "d…_feat-tower-prototype"), then the
// branch loses its middle. repoN is the repo part's length in graphemes.
// When every hit is in the repo part, the name is cut at the end instead
// so they stay visible.
func fitWorktree(gs []gr, repoN int, room int, hits []int) fitted {
	if grWidth(gs) <= room || repoN <= 0 || repoN >= len(gs)-1 {
		return fitMiddle(gs, room, firstHit(hits))
	}
	if len(hits) > 0 && hits[len(hits)-1] < repoN {
		return fitEnd(gs, room)
	}
	rest := gs[repoN:] // the separator and the branch
	restW := grWidth(rest)
	for k := repoN - 1; k >= 1; k-- {
		if grWidth(gs[:k])+1+restW <= room {
			f := pieces(gs, 0, k)
			f = append(f, ellipsis)
			return append(f, pieces(gs, repoN, len(gs))...)
		}
	}
	head := append(pieces(gs, 0, 1), ellipsis, piece{gs[repoN].s, gs[repoN].w, repoN})
	left := room - head.width()
	branch := gs[repoN+1:]
	bh := -1
	for _, h := range hits {
		if h > repoN {
			bh = h - repoN - 1
			break
		}
	}
	for _, p := range fitMiddle(branch, left, bh) {
		if p.at >= 0 {
			p.at += repoN + 1
		}
		head = append(head, p)
	}
	return head
}

func firstHit(hits []int) int {
	if len(hits) == 0 {
		return -1
	}
	return hits[0]
}

// fitPath shortens a directory path as fish's prompt does: every
// directory but the last to its first character (two for a dotdir),
// "~/w/c/a/backend-api"; if that is still too long, only the last
// directory, "…/backend-api-gateway", cut further at its middle.
func fitPath(path string, room int, hits []int) fitted {
	gs := graphemes(path)
	if grWidth(gs) <= room {
		return pieces(gs, 0, len(gs))
	}
	// Component boundaries in graphemes.
	var starts []int
	starts = append(starts, 0)
	for i, g := range gs {
		if g.s == "/" {
			starts = append(starts, i+1)
		}
	}
	last := starts[len(starts)-1]
	var f fitted
	for k := 0; k < len(starts)-1; k++ {
		from, to := starts[k], starts[k+1]-1 // to: the slash
		keep := 1
		if to > from && gs[from].s == "." {
			keep = 2
		}
		if to-from == 0 {
			keep = 0
		}
		if gs[from].s == "~" {
			keep = to - from
		}
		f = append(f, pieces(gs, from, min(from+keep, to))...)
		f = append(f, piece{"/", 1, to})
	}
	f = append(f, pieces(gs, last, len(gs))...)
	if f.width() <= room {
		return f
	}
	tail := gs[last:]
	if grWidth(tail)+2 <= room {
		out := fitted{ellipsis, piece{"/", 1, last - 1}}
		return append(out, pieces(gs, last, len(gs))...)
	}
	bh := -1
	for _, h := range hits {
		if h >= last {
			bh = h - last
			break
		}
	}
	out := fitted{ellipsis, piece{"/", 1, last - 1}}
	for _, p := range fitMiddle(tail, room-2, bh) {
		if p.at >= 0 {
			p.at += last
		}
		out = append(out, p)
	}
	if out.width() > room {
		return fitEnd(gs, room)
	}
	return out
}

// inputView fits an input line into w cells keeping the text cursor (a
// rune index) in view: text scrolled off the left is marked "…". It
// returns the text to draw and the cursor's cell in it.
func inputView(text []rune, cursor, w int) (string, int) {
	cw := func(rs []rune) int { return width(string(rs)) }
	if cw(text) < w {
		return string(text), cw(text[:cursor])
	}
	start := 0
	for start < cursor && 1+cw(text[start:cursor])+1 > w {
		start++
	}
	if start == 0 && cw(text[:cursor])+1 <= w {
		return ansi.Truncate(string(text), w, glyphMore), cw(text[:cursor])
	}
	s := glyphMore + string(text[start:])
	return ansi.Truncate(s, w, glyphMore), 1 + cw(text[start:cursor])
}

// cutMiddleText is fitMiddle for plain strings.
func cutMiddleText(s string, room int) string { return fitMiddle(graphemes(s), room, -1).String() }
