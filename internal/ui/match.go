package ui

import "unicode"

// Matching the query against rows, the way fzf reads to a user: every
// character of the query, in order, ignoring case, with spaces in the
// query ignored. Of the rows that match, the ones that match best come
// first, so ⏎ on the top row takes the session the user named:
//
//  1. the session (or window) name is the query;
//  2. the name starts with the query;
//  3. the name contains the query, at a word start before inside a word,
//     earlier before later;
//  4. "host name" (spaces aside) contains the query, likewise;
//  5. the query's characters are scattered, a tighter span first.
//
// Ties keep the rows' own order (recency). So `s50` puts session s50
// before s150, and `bra` puts bravo before cobra.

// score orders matches: lower is better, field by field.
type score struct{ tier, a, b int }

func (s score) less(o score) bool {
	if s.tier != o.tier {
		return s.tier < o.tier
	}
	if s.a != o.a {
		return s.a < o.a
	}
	return s.b < o.b
}

// fold is the query as it is compared: lower case, no spaces.
func fold(query []rune) []rune {
	q := make([]rune, 0, len(query))
	for _, r := range query {
		if !unicode.IsSpace(r) {
			q = append(q, unicode.ToLower(r))
		}
	}
	return q
}

// rank matches the folded query q against text, whose name starts at rune
// nameAt (-1: no name). It returns the match's score and the rune
// positions in text that matched, for highlighting.
func rank(q []rune, text string, nameAt int) (score, []int, bool) {
	if len(q) == 0 {
		return score{}, nil, true
	}
	t := []rune(text)
	for i, r := range t {
		t[i] = unicode.ToLower(r)
	}
	span := func(from, n int) []int {
		p := make([]int, n)
		for i := range p {
			p[i] = from + i
		}
		return p
	}
	if nameAt >= 0 && nameAt <= len(t) {
		name := t[nameAt:]
		if i := index(name, q); i >= 0 {
			switch {
			case i == 0 && len(name) == len(q):
				return score{tier: 1}, span(nameAt, len(q)), true
			case i == 0:
				return score{tier: 2}, span(nameAt, len(q)), true
			}
			return score{3, wordStart(name, i), i}, span(nameAt+i, len(q)), true
		}
	}
	// "host name" with its spaces left out, mapped back to text.
	var bare []rune
	var at []int
	for i, r := range t {
		if !unicode.IsSpace(r) {
			bare = append(bare, r)
			at = append(at, i)
		}
	}
	if i := index(bare, q); i >= 0 {
		pos := make([]int, len(q))
		for j := range pos {
			pos[j] = at[i+j]
		}
		return score{4, wordStart(t, at[i]), at[i]}, pos, true
	}
	pos, ok := scatter(q, t)
	if !ok {
		return score{}, nil, false
	}
	return score{5, pos[len(pos)-1] - pos[0], pos[0]}, pos, true
}

// index is the first position of q in s, or -1.
func index(s, q []rune) int {
	for i := 0; i+len(q) <= len(s); i++ {
		j := 0
		for j < len(q) && s[i+j] == q[j] {
			j++
		}
		if j == len(q) {
			return i
		}
	}
	return -1
}

// wordStart is 0 when s[i] begins a word (the start, or after a character
// that is not a letter or digit), else 1.
func wordStart(s []rune, i int) int {
	if i == 0 || !unicode.IsLetter(s[i-1]) && !unicode.IsDigit(s[i-1]) {
		return 0
	}
	return 1
}

// scatter finds q's characters in order in t: the match that ends
// earliest, tightened from its end, so "bra" in "b bravo" marks "bra".
func scatter(q, t []rune) ([]int, bool) {
	end, qi := -1, 0
	for i, r := range t {
		if r == q[qi] {
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
