package ui

import "unicode"

// match reports whether query matches text the way fzf's default matching
// reads to a user: every character of the query, in order, anywhere in
// text, ignoring case; spaces in the query are ignored. It returns the
// rune positions in text that matched, for highlighting.
//
// Of the possible matches it prefers the one that ends earliest and then
// starts latest, so "bra" in "B bravo" marks "bra", not the host's "B".
func match(query []rune, text string) ([]int, bool) {
	q := make([]rune, 0, len(query))
	for _, r := range query {
		if !unicode.IsSpace(r) {
			q = append(q, unicode.ToLower(r))
		}
	}
	if len(q) == 0 {
		return nil, true
	}
	t := []rune(text)
	for i, r := range t {
		t[i] = unicode.ToLower(r)
	}
	// Forward: the earliest end of a match.
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
	// Backward from that end: the latest start, which keeps the match
	// tight.
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
