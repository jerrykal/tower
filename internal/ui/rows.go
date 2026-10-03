package ui

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

// rowKey is a row's identity: the cursor, the hidden set and ⏎ name rows
// by it, never by position. Session and window ids are valid only within
// one server instance, so the instance is part of it. A host with no
// sessions has a key with an empty Session.
type rowKey struct {
	Host    string // towerd id
	Inst    string
	Session string
	Window  string
}

// sessionKey is the key of the session a window row belongs to.
func (k rowKey) sessionKey() rowKey { k.Window = ""; return k }

// row is one line of the picker, derived from a view.
type row struct {
	key      rowKey
	host     *proto.Host
	sess     *proto.Session // nil: the host's "(no sessions)" row
	win      *proto.Window  // windows mode only
	band     int            // reachable, not reachable, turned off
	ago      time.Duration
	cur      bool // where the loop (or this client) is
	prev     bool // where it was before
	others   int  // other clients on the session
	bell     bool
	activity bool
	local    bool   // the host is the machine the dashboard runs on
	status   string // the host's status when it is not up
	text     string // what the filter matches: "host session" or "idx:name"
	nameAt   int    // rune offset of the session (or window) name in text; -1: none
}

// marks are what the rows mark as current and previous.
type marks struct {
	cur, prev proto.Ref
}

// marksFor reads the loop's current and previous targets from the view.
// here, when set, is the client's own session, which is current for a
// client no loop owns.
func marksFor(d *proto.Dash, loop string, here proto.Ref) marks {
	var m marks
	if l := d.View.LoopByID(loop); l != nil {
		m.cur, m.prev = l.Cur, l.Prev
	}
	if m.cur.IsZero() {
		m.cur = here
	}
	return m
}

func sameSession(a proto.Ref, h *proto.Host, s *proto.Session) bool {
	return a.Host == h.ID && a.Session == s.ID && (a.Inst == "" || h.Inst == "" || a.Inst == h.Inst)
}

// hostKey is a host's part of a row key: its towerd id, or, for a host
// never reached (no id yet), its name, which no id can equal.
func hostKey(h *proto.Host) string {
	if h.ID == "" {
		return "name:" + h.Name
	}
	return h.ID
}

// band orders hosts: reachable, then not reachable, then turned off.
func band(h *proto.Host) int {
	switch {
	case h.Reachable():
		return 0
	case h.Status == proto.StatusOff:
		return 2
	}
	return 1
}

// hostStatus is how a row shows a host that is not up.
func hostStatus(h *proto.Host) string {
	switch h.Status {
	case proto.StatusLocal, proto.StatusUp:
		return ""
	case proto.StatusDown, proto.StatusFailed, proto.StatusDup:
		if h.Reason != "" {
			return h.Status + ": " + h.Reason
		}
	}
	return h.Status
}

// sessionRows builds the picker's rows from a view: every session of every
// host, reachable hosts first, then by recency (most recently attached
// first) across hosts; a host with no sessions gets one row. since is the
// time since the view was read, which ages every session.
func sessionRows(d *proto.Dash, mk marks, since time.Duration) []row {
	var rows []row
	for hi := range d.View.Hosts {
		h := &d.View.Hosts[hi]
		base := row{host: h, band: band(h), local: h.ID != "" && h.ID == d.Self, status: hostStatus(h)}
		if len(h.Sessions) == 0 {
			r := base
			r.key = rowKey{Host: hostKey(h), Inst: h.Inst}
			r.ago = time.Duration(math.MaxInt64)
			r.text = h.Name + " (no sessions)"
			r.nameAt = -1
			rows = append(rows, r)
			continue
		}
		for si := range h.Sessions {
			s := &h.Sessions[si]
			r := base
			r.key = rowKey{Host: hostKey(h), Inst: h.Inst, Session: s.ID}
			r.sess = s
			r.ago = time.Duration(s.Ago)*time.Millisecond + since
			r.cur = sameSession(mk.cur, h, s)
			r.prev = !r.cur && sameSession(mk.prev, h, s)
			r.others = s.Attached
			if r.cur && r.others > 0 {
				r.others-- // the client the loop is on
			}
			for _, w := range s.Windows {
				r.bell = r.bell || w.Bell
				r.activity = r.activity || w.Activity
			}
			r.text = h.Name + " " + s.Name
			r.nameAt = len([]rune(h.Name)) + 1
			rows = append(rows, r)
		}
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		if c := cmp.Compare(a.band, b.band); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ago, b.ago); c != 0 {
			return c
		}
		if c := cmp.Compare(a.host.Name, b.host.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.text, b.text)
	})
	return rows
}

// windowRows are the windows of the session k names, in index order; nil
// when the session is gone.
func windowRows(d *proto.Dash, k rowKey, mk marks) []row {
	h := d.View.HostByID(k.Host)
	if h == nil || h.Inst != k.Inst {
		return nil
	}
	for si := range h.Sessions {
		s := &h.Sessions[si]
		if s.ID != k.Session {
			continue
		}
		cur := sameSession(mk.cur, h, s)
		rows := make([]row, 0, len(s.Windows))
		for wi := range s.Windows {
			w := &s.Windows[wi]
			r := row{
				key:  rowKey{Host: h.ID, Inst: h.Inst, Session: s.ID, Window: w.ID},
				host: h, sess: s, win: w, band: band(h), local: h.ID == d.Self, status: hostStatus(h),
				bell: w.Bell, activity: w.Activity,
				text:   strconv.Itoa(w.Index) + ":" + w.Name,
				nameAt: len(strconv.Itoa(w.Index)) + 1,
			}
			r.cur = cur && (mk.cur.Window == w.ID || mk.cur.Window == "" && w.Active)
			rows = append(rows, r)
		}
		slices.SortStableFunc(rows, func(a, b row) int { return cmp.Compare(a.win.Index, b.win.Index) })
		return rows
	}
	return nil
}

// age is a session's time since it was last attached, tmux-short.
func age(d time.Duration) string {
	switch {
	case d == time.Duration(math.MaxInt64):
		return ""
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// Glyphs (Nerd Font).
const (
	glyphCur     = "󰧟"
	glyphPrev    = "-"
	glyphBell    = "󰂞"
	glyphAct     = "󰐰"
	glyphClients = "󰍺"
)

// part kinds: what a cell of a row is, for styling.
type part int

const (
	partHost part = iota
	partName
	partCount
	partAge
	partCur
	partMark
	partClients
	partBell
	partAct
	partStatus
	partNone
)

// cell is one column of a row: its text and kind; off is the rune offset
// of the text within row.text when the filter matches it (-1 otherwise).
type cell struct {
	text string
	kind part
	off  int
}

// cells lays a session or window row out in columns.
func (r *row) cells() []cell {
	var c []cell
	if r.win != nil {
		c = append(c, cell{strconv.Itoa(r.win.Index) + ":", partCount, 0},
			cell{r.win.Name, partName, len([]rune(strconv.Itoa(r.win.Index))) + 1})
		if r.win.Panes > 1 {
			c = append(c, cell{strconv.Itoa(r.win.Panes) + "p", partCount, -1})
		} else {
			c = append(c, cell{"", partCount, -1})
		}
		if r.win.Active {
			c = append(c, cell{"*", partMark, -1})
		} else {
			c = append(c, cell{"", partMark, -1})
		}
	} else {
		c = append(c, cell{r.host.Name, partHost, 0})
		if r.sess == nil {
			c = append(c, cell{"(no sessions)", partNone, -1}, cell{"", partCount, -1}, cell{"", partAge, -1})
		} else {
			c = append(c,
				cell{r.sess.Name, partName, len([]rune(r.host.Name)) + 1},
				cell{strconv.Itoa(len(r.sess.Windows)) + "w", partCount, -1},
				cell{age(r.ago), partAge, -1})
		}
	}
	var m []cell
	switch {
	case r.cur:
		m = append(m, cell{glyphCur, partCur, -1})
	case r.prev:
		m = append(m, cell{glyphPrev, partMark, -1})
	}
	if r.others > 0 {
		m = append(m, cell{glyphClients + " " + strconv.Itoa(r.others), partClients, -1})
	}
	if r.bell {
		m = append(m, cell{glyphBell, partBell, -1})
	}
	if r.activity {
		m = append(m, cell{glyphAct, partAct, -1})
	}
	if r.status != "" {
		m = append(m, cell{r.status, partStatus, -1})
	}
	return append(c, m...)
}

// columns are the widths of the fixed columns of a set of rows, so cells
// line up down the list.
type columns []int

func layout(rows []row) columns {
	var w columns
	for i := range rows {
		cs := rows[i].cells()
		fixed := 4 // host/idx, name, count, age/active mark
		for j := 0; j < fixed && j < len(cs); j++ {
			if j >= len(w) {
				w = append(w, 0)
			}
			if cs[j].kind != partNone { // "(no sessions)" may overflow
				w[j] = max(w[j], width(cs[j].text))
			}
		}
	}
	return w
}

// plain is a row as text: its cells padded to cols and joined by two
// spaces, trailing blanks trimmed. It is what `tower _ui rows` prints and
// what the picker draws, without styling.
func (r *row) plain(cols columns) string {
	var b strings.Builder
	for j, c := range r.cells() {
		if j > 0 {
			b.WriteString("  ")
		}
		b.WriteString(c.text)
		if j < len(cols) {
			b.WriteString(strings.Repeat(" ", max(0, cols[j]-width(c.text))))
		}
	}
	return strings.TrimRight(b.String(), " ")
}
