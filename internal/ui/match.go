package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Matching. A column's search filters the way fzf reads to a user: the
// query's characters in order, ignoring case, spaces in the query
// ignored; the column keeps its own order. The finder (find.go) scores
// whole words instead. Both work on graphemes, so highlights land on what
// is drawn.

// ftext is text as it is compared: lower-case graphemes.
type ftext []string

func foldText(s string) ftext {
	gs := graphemes(s)
	out := make(ftext, len(gs))
	for i, g := range gs {
		out[i] = strings.ToLower(g.s)
	}
	return out
}

// foldQuery is a column's query as it is compared: no spaces.
func foldQuery(q []rune) ftext {
	var b strings.Builder
	for _, r := range q {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return foldText(b.String())
}

// inOrder finds q's graphemes in order in t: the match that ends
// earliest, tightened from its end, so "bra" in "b bravo" marks "bra".
func inOrder(q, t ftext) ([]int, bool) {
	if len(q) == 0 {
		return nil, true
	}
	end, qi := -1, 0
	for i, g := range t {
		if g == q[qi] {
			qi++
			if qi == len(q) {
				end = i
				break
			}
		}
	}
	if end < 0 {
		return nil, false
	}
	pos := make([]int, len(q))
	qi = len(q) - 1
	for i := end; i >= 0 && qi >= 0; i-- {
		if t[i] == q[qi] {
			pos[qi] = i
			qi--
		}
	}
	return pos, true
}

// filterMatch is a column's search on one name.
func filterMatch(q ftext, name string) ([]int, bool) {
	if len(q) == 0 {
		return nil, true
	}
	return inOrder(q, foldText(name))
}

// wordStartAt reports whether t[i] begins a word: the start, or after a
// grapheme that is not a letter or digit.
func wordStartAt(t ftext, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeRuneInString(t[i-1])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// indexAt is the first position of w in t at or after from, or -1.
func indexAt(t, w ftext, from int) int {
	for i := from; i+len(w) <= len(t); i++ {
		j := 0
		for j < len(w) && t[i+j] == w[j] {
			j++
		}
		if j == len(w) {
			return i
		}
	}
	return -1
}

// Scores of one finder word against one name: higher is better. A
// substring beats any scattered match; among substrings the whole name,
// then its start, then a word start, each nearer the front.
const (
	scoreSub     = 1000
	scoreExact   = 600
	scorePrefix  = 400
	scoreWord    = 200
	scoreScatter = 300
	scoreIndex   = 1500 // a window number next to a ':'
	scorePlainNo = 500  // a plain number naming a window by number
)

// wordMatch scores word w against t. sub reports a substring match.
func wordMatch(w, t ftext) (score int, pos []int, sub bool, ok bool) {
	if len(w) == 0 || len(t) == 0 {
		return 0, nil, false, false
	}
	best := -1
	for i := indexAt(t, w, 0); i >= 0; i = indexAt(t, w, i+1) {
		s := scoreSub - min(i, 100)
		switch {
		case i == 0 && len(w) == len(t):
			s += scoreExact
		case i == 0:
			s += scorePrefix
		case wordStartAt(t, i):
			s += scoreWord
		}
		if s > best {
			best = s
			pos = make([]int, len(w))
			for j := range pos {
				pos[j] = i + j
			}
		}
	}
	if best >= 0 {
		return best, pos, true, true
	}
	// Scattered: the tightest span, at most three times the word's
	// length.
	bestSpan, bestAt := -1, -1
	for i := range t {
		if t[i] != w[0] {
			continue
		}
		qi, end := 1, i
		for j := i + 1; j < len(t) && qi < len(w); j++ {
			if t[j] == w[qi] {
				qi++
				end = j
			}
		}
		if qi < len(w) && len(w) > 1 {
			break // no later start can match either
		}
		if span := end - i + 1; bestSpan < 0 || span < bestSpan {
			bestSpan, bestAt = span, i
		}
	}
	if bestSpan < 0 || bestSpan > 3*len(w) {
		return 0, nil, false, false
	}
	pos = make([]int, 0, len(w))
	qi := 0
	for j := bestAt; j < len(t) && qi < len(w); j++ {
		if t[j] == w[qi] {
			pos = append(pos, j)
			qi++
		}
	}
	s := scoreScatter - (bestSpan-len(w))*10 - min(bestAt, 50)
	if wordStartAt(t, bestAt) {
		s += 50
	}
	return max(s, 1), pos, false, true
}

// qword is one word of a finder query.
type qword struct {
	text  ftext
	num   int  // the word as a window number, or -1
	index bool // a number next to a ':': names a window by number only
}

// splitQuery splits a finder query at spaces and ':', in any order.
func splitQuery(q []rune) []qword {
	var out []qword
	s := string(q)
	i := 0
	for i < len(s) {
		if s[i] == ' ' || s[i] == ':' || s[i] == '\t' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] != ' ' && s[j] != ':' && s[j] != '\t' {
			j++
		}
		word := s[i:j]
		w := qword{text: foldText(word), num: -1}
		if n, ok := number(word); ok {
			w.num = n
			w.index = i > 0 && s[i-1] == ':' || j < len(s) && s[j] == ':'
		}
		out = append(out, w)
		i = j
	}
	return out
}

func number(s string) (int, bool) {
	if s == "" || len(s) > 6 {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
