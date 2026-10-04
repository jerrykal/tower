package ui

import (
	"cmp"
	"slices"
	"strconv"
)

// The finder: every host's sessions in one list, for "where is train?"
// without walking host by host. Each session appears once with the
// windows that match a query word under it; zoxide dirs follow the
// sessions of reachable hosts; unreachable hosts' cached sessions and
// dirs come last, since ⏎ there can only reconnect. A run of dirs, and
// the sessions after one, open with a header (lines).

type frowKind uint8

const (
	fSession frowKind = iota
	fWindow
	fMore
	fDir
)

// frow is one row of the finder.
type frow struct {
	kind  frowKind
	key   rowKey // the session's, window's or dir's; a more row: its session's with Window "+"
	it    *item  // the session or dir (a window row: its session)
	win   *item  // window rows and folded ones
	fold  bool   // "session › idx:window" on one line
	dim   bool   // a window the query does not match, shown by tab
	more  int    // a more row: windows not listed
	score int
	band  int

	hostHits, nameHits, winHits []int
	numHit                      bool // the window's number matched
}

// finder is the finder's state.
type finder struct {
	in       input // the query; its text cursor stays at its end
	rows     []frow
	cursor   rowKey
	top      int
	expanded rowKey // the session tab opened, if any
	found    int    // sessions and dirs the query matched
	total    int
	front    bool // the dashboard opened in the finder: esc closes it
}

// winMatch is a window a query lists under its session.
type winMatch struct {
	it     *item
	score  int
	hits   []int
	numHit bool
}

// sessMatch is how a query matches one session.
type sessMatch struct {
	total       int // ranks the session
	own         int // the session row's score: words on its host or name
	hostHits    []int
	nameHits    []int
	nameMatched bool
	wins        []winMatch // listed windows, best first
	rowScore    map[*item]int
}

const maxWindowRows = 3

// matchSession matches the words against a session, its host and its
// windows: each word goes to what it matches best, and every word must
// match something.
func matchSession(words []qword, it *item, wins []item, leader bool) (sessMatch, bool) {
	var sm sessMatch
	if len(words) == 0 {
		return sm, true
	}
	host := foldText(it.host.Name)
	name := foldText(it.sess.Name)
	type wstate struct {
		score  int
		hits   []int
		numHit bool
		listed bool
	}
	ws := make([]wstate, len(wins))
	rowScore := make([]int, len(wins))
	wtexts := make([]ftext, len(wins))
	for i := range wins {
		wtexts[i] = foldText(wins[i].win.Name)
	}
	for _, w := range words {
		var hS, sS int
		var hPos, sPos []int
		var anySub bool
		if !w.index {
			var hSub, sSub bool
			hS, hPos, hSub, _ = wordMatch(w.text, host)
			sS, sPos, sSub, _ = wordMatch(w.text, name)
			anySub = hSub || sSub
		}
		type one struct {
			s   int
			pos []int
			sub bool
			num bool
		}
		per := make([]one, len(wins))
		bestWin := 0
		if leader {
			for i := range wins {
				var o one
				if w.num >= 0 && wins[i].win.Index == w.num {
					o.num = true
					if w.index {
						o.s = scoreIndex
					} else {
						o.s = scorePlainNo
					}
				}
				if !w.index {
					if s, pos, sub, ok := wordMatch(w.text, wtexts[i]); ok && s > o.s {
						o = one{s: s, pos: pos, sub: sub}
					}
				}
				anySub = anySub || o.sub
				per[i] = o
				bestWin = max(bestWin, o.s)
			}
		}
		best := max(hS, sS, bestWin)
		if best <= 0 {
			return sm, false
		}
		sm.total += best
		if sS > 0 {
			sm.nameMatched = true
		}
		switch {
		case sS > 0 && sS >= hS && sS >= bestWin:
			sm.own += sS
			sm.nameHits = append(sm.nameHits, sPos...)
		case hS > 0 && hS >= bestWin:
			sm.own += hS
			sm.hostHits = append(sm.hostHits, hPos...)
		}
		hsBest := max(hS, sS)
		for i := range wins {
			o := per[i]
			rowScore[i] += max(o.s, hsBest)
			if o.s > 0 && (o.sub || o.num || !anySub) {
				ws[i].listed = true
				ws[i].score += o.s
				ws[i].hits = append(ws[i].hits, o.pos...)
				ws[i].numHit = ws[i].numHit || o.num
			}
		}
	}
	sm.rowScore = map[*item]int{}
	for i := range wins {
		sm.rowScore[&wins[i]] = rowScore[i]
		if ws[i].listed {
			sm.wins = append(sm.wins, winMatch{it: &wins[i], score: rowScore[i], hits: ws[i].hits, numHit: ws[i].numHit})
		}
	}
	slices.SortStableFunc(sm.wins, func(a, b winMatch) int { return cmp.Compare(b.score, a.score) })
	return sm, true
}

// matchDir matches the words against a dir's host and path.
func matchDir(words []qword, it *item) (int, []int, []int, bool) {
	host := foldText(it.host.Name)
	p := foldText(it.dir.Path)
	total := 0
	var hHits, pHits []int
	for _, w := range words {
		if w.index {
			return 0, nil, nil, false
		}
		hS, hPos, _, _ := wordMatch(w.text, host)
		pS, pPos, _, _ := wordMatch(w.text, p)
		if hS <= 0 && pS <= 0 {
			return 0, nil, nil, false
		}
		if pS >= hS {
			total += pS
			pHits = append(pHits, pPos...)
		} else {
			total += hS
			hHits = append(hHits, hPos...)
		}
	}
	return total, hHits, pHits, true
}

// build lists the finder's rows for w (dirs as listed: all or git roots)
// and puts the cursor back on its row; when the query changed (requery),
// on the best row of the top session.
func (f *finder) build(w *world, allDirs bool, requery bool) {
	words := splitQuery(f.in.text)
	old := f.rows
	type group struct {
		rows  []frow
		band  int
		score int
		ago   int64
		order int
	}
	var groups []group
	f.found, f.total = 0, 0
	order := 0
	for hi := range w.hosts {
		h := &w.hosts[hi]
		reach := h.host.Reachable()
		ss := w.sessions[h.host.Name]
		for si := range ss {
			s := &ss[si]
			f.total++
			order++
			wins := w.windows[s.key]
			leader := w.groupLeader(s)
			sm, ok := matchSession(words, s, wins, leader)
			if !ok {
				continue
			}
			f.found++
			g := group{band: 0, score: sm.total, ago: int64(s.ago), order: order}
			if !reach {
				g.band = 2
			}
			expanded := f.expanded == s.key && len(wins) > 0
			if !expanded && !sm.nameMatched && len(sm.wins) == 1 && len(words) > 0 {
				wm := sm.wins[0]
				g.rows = append(g.rows, frow{kind: fWindow, key: wm.it.key, it: s, win: wm.it, fold: true, score: wm.score,
					band: g.band, hostHits: sm.hostHits, winHits: wm.hits, numHit: wm.numHit})
				groups = append(groups, g)
				continue
			}
			g.rows = append(g.rows, frow{kind: fSession, key: s.key, it: s, score: sm.own, band: g.band,
				hostHits: sm.hostHits, nameHits: sm.nameHits})
			if expanded {
				listed := map[*item]winMatch{}
				for _, wm := range sm.wins {
					listed[wm.it] = wm
				}
				for i := range wins {
					wi := &wins[i]
					wm, ok := listed[wi]
					r := frow{kind: fWindow, key: wi.key, it: s, win: wi, band: g.band, score: sm.rowScore[wi]}
					if ok {
						r.winHits, r.numHit = wm.hits, wm.numHit
					} else {
						r.dim = len(words) > 0
					}
					g.rows = append(g.rows, r)
				}
			} else if len(words) > 0 {
				for i, wm := range sm.wins {
					if i == maxWindowRows && len(sm.wins) > maxWindowRows+1 {
						more := s.key
						more.Window = "+"
						g.rows = append(g.rows, frow{kind: fMore, key: more, it: s, more: len(sm.wins) - maxWindowRows, band: g.band})
						break
					}
					g.rows = append(g.rows, frow{kind: fWindow, key: wm.it.key, it: s, win: wm.it, score: wm.score,
						band: g.band, winHits: wm.hits, numHit: wm.numHit})
				}
			}
			groups = append(groups, g)
		}
		ds := w.dirsOf(h.host.Name, allDirs)
		for di := range ds {
			d := &ds[di]
			f.total++
			order++
			score, hHits, pHits, ok := 0, []int(nil), []int(nil), true
			if len(words) > 0 {
				score, hHits, pHits, ok = matchDir(words, d)
			}
			if !ok {
				continue
			}
			f.found++
			b := 1
			if !reach {
				b = 3
			}
			// Dirs keep zoxide's order (most frecent first) after the
			// score: their ago is their place in the list.
			groups = append(groups, group{band: b, score: score, ago: int64(di), order: order,
				rows: []frow{{kind: fDir, key: d.key, it: d, score: score, band: b, hostHits: hHits, nameHits: pHits}}})
		}
	}
	slices.SortStableFunc(groups, func(a, b group) int {
		if c := cmp.Compare(a.band, b.band); c != 0 {
			return c
		}
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		if a.band == 0 || a.band == 2 {
			if c := cmp.Compare(a.ago, b.ago); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.order, b.order)
	})
	f.rows = f.rows[:0:0]
	for _, g := range groups {
		f.rows = append(f.rows, g.rows...)
	}
	if requery || f.index(f.cursor) < 0 && len(old) == 0 {
		f.toBest()
		return
	}
	if f.index(f.cursor) >= 0 {
		return
	}
	// The cursor's row went: the nearest row after it that is still
	// there, else before it.
	at := -1
	for i := range old {
		if old[i].key == f.cursor {
			at = i
		}
	}
	for i := at + 1; i < len(old) && at >= 0; i++ {
		if f.index(old[i].key) >= 0 {
			f.cursor = old[i].key
			return
		}
	}
	for i := at - 1; i >= 0; i-- {
		if f.index(old[i].key) >= 0 {
			f.cursor = old[i].key
			return
		}
	}
	f.toBest()
}

// toBest puts the cursor on the best row of the top session: tens lands
// on the window tensorboard, not on its session.
func (f *finder) toBest() {
	f.top = 0
	if len(f.rows) == 0 {
		f.cursor = rowKey{}
		return
	}
	best := 0
	for i := 1; i < len(f.rows); i++ {
		r := f.rows[i]
		if r.kind == fSession || r.kind == fDir || r.fold {
			break
		}
		if r.kind == fWindow && r.score > f.rows[best].score {
			best = i
		}
	}
	f.cursor = f.rows[best].key
}

func (f *finder) index(k rowKey) int {
	for i := range f.rows {
		if f.rows[i].key == k {
			return i
		}
	}
	return -1
}

// lines lays the rows out as the sessions column does: a run of dirs, and
// the sessions after one, open with a header and a blank line (and a
// blank above the header when rows come before it). line[i] is row i's
// line; heads are the rows that open a section, their header two lines
// above them.
func (f *finder) lines() (line []int, heads []int, total int) {
	line = make([]int, len(f.rows))
	for i := range f.rows {
		dir := f.rows[i].kind == fDir
		if i == 0 && dir || i > 0 && dir != (f.rows[i-1].kind == fDir) {
			if i > 0 {
				total++
			}
			total += 2
			heads = append(heads, i)
		}
		line[i] = total
		total++
	}
	return line, heads, total
}

// at is the cursor's row index, or -1.
func (f *finder) at() int { return f.index(f.cursor) }

func (f *finder) selected() *frow {
	if i := f.at(); i >= 0 {
		return &f.rows[i]
	}
	return nil
}

func (f *finder) move(d int) {
	if len(f.rows) == 0 {
		return
	}
	i := f.at()
	if i < 0 {
		i = 0
	}
	f.cursor = f.rows[min(max(i+d, 0), len(f.rows)-1)].key
}

// winLabel is a window as rows show it: "2:train".
func winLabel(w *item) string { return strconv.Itoa(w.win.Index) + ":" + w.win.Name }
