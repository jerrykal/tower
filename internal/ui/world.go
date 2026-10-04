package ui

import (
	"cmp"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

// rowKey is a row's identity: the cursor, the selection memory, the
// hidden set and ⏎ name rows by it, never by position. Session and window
// ids are valid only within one server instance, so the instance is part
// of it. A dir is its host and path.
type rowKey struct {
	Host    string // towerd id (hostKey)
	Inst    string
	Session string
	Window  string
	Dir     string
}

// sessionKey is the key of the session a window row belongs to.
func (k rowKey) sessionKey() rowKey { k.Window = ""; return k }

// hostKey is a host's part of a row key: its towerd id, or, for a host
// never reached (no id yet), its name, which no id can equal.
func hostKey(h *proto.Host) string {
	if h.ID == "" {
		return "name:" + h.Name
	}
	return h.ID
}

type itemKind uint8

const (
	kHost itemKind = iota
	kSession
	kDir
	kWindow
)

// item is one thing a column or the finder lists: a host, a session, a
// zoxide dir or a window, derived from a view.
type item struct {
	kind itemKind
	key  rowKey
	host *proto.Host
	sess *proto.Session
	win  *proto.Window
	dir  *proto.Dir

	local     bool // the host is the machine the dashboard runs on
	cur, prev bool // where the loop (or this client) is, and was
	others    int  // other clients on the session
	bell, act bool // rolled up from windows
	ago       time.Duration
	isNew     bool   // made from this dashboard and not attached since
	name      string // what the column's filter matches
}

// world is everything the dashboard lists, derived from one view: hosts
// in the dashboard's order, each host's sessions (most recent first) and
// zoxide dirs, each session's windows.
type world struct {
	d        *proto.Dash
	hosts    []item
	sessions map[string][]item   // by host name
	dirs     map[string][]item   // by host name, every entry
	windows  map[rowKey][]item   // by session key
	byName   map[string]*item    // hosts by name
	groups   map[string][]string // host name + group → member names
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
	return a.Host != "" && a.Host == h.ID && a.Session == s.ID && (a.Inst == "" || h.Inst == "" || a.Inst == h.Inst)
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

// hostStatus is how a host that is not up reads: "down: timed out".
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

// loading reports whether the host's list is on its way: a spinner in
// its row.
func loading(h *proto.Host) bool {
	switch h.Status {
	case proto.StatusConnecting, proto.StatusInstalling:
		return true
	}
	return false
}

// newest is the time since the host's most recent attach.
func newest(h *proto.Host) time.Duration {
	best := time.Duration(math.MaxInt64)
	for _, s := range h.Sessions {
		best = min(best, time.Duration(s.Ago)*time.Millisecond)
	}
	return best
}

// hostOrder is the hosts' order as the dashboard opens: reachable, then
// down, then turned off, each by most recent attach; ties in list order.
func hostOrder(d *proto.Dash) []string {
	hs := make([]*proto.Host, len(d.View.Hosts))
	for i := range d.View.Hosts {
		hs[i] = &d.View.Hosts[i]
	}
	slices.SortStableFunc(hs, func(a, b *proto.Host) int {
		if c := cmp.Compare(band(a), band(b)); c != 0 {
			return c
		}
		return cmp.Compare(newest(a), newest(b))
	})
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Name
	}
	return out
}

// buildWorld derives the lists from d. order is the hosts' order (hosts
// it lacks go last, in the view's order); hidden rows (kills in flight)
// are left out; fresh sessions show "new"; since ages every session.
func buildWorld(d *proto.Dash, mk marks, since time.Duration, order []string, hidden map[rowKey]hide, fresh map[rowKey]bool) *world {
	w := &world{d: d, sessions: map[string][]item{}, dirs: map[string][]item{}, windows: map[rowKey][]item{},
		byName: map[string]*item{}, groups: map[string][]string{}}
	rank := map[string]int{}
	for i, n := range order {
		rank[n] = i
	}
	idx := make([]int, len(d.View.Hosts))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		ra, oka := rank[d.View.Hosts[a].Name]
		rb, okb := rank[d.View.Hosts[b].Name]
		switch {
		case oka && okb:
			return cmp.Compare(ra, rb)
		case oka:
			return -1
		case okb:
			return 1
		}
		return 0
	})
	for _, hi := range idx {
		h := &d.View.Hosts[hi]
		hk := hostKey(h)
		hit := item{kind: kHost, key: rowKey{Host: hk}, host: h, local: h.ID != "" && h.ID == d.Self, name: h.Name, ago: newest(h)}
		var sess []item
		for si := range h.Sessions {
			s := &h.Sessions[si]
			k := rowKey{Host: hk, Inst: h.Inst, Session: s.ID}
			if _, hid := hidden[k]; hid {
				continue
			}
			it := item{kind: kSession, key: k, host: h, sess: s, local: hit.local, name: s.Name,
				ago: time.Duration(s.Ago)*time.Millisecond + since}
			it.cur = sameSession(mk.cur, h, s)
			it.prev = !it.cur && sameSession(mk.prev, h, s)
			it.others = s.Attached
			if it.cur && it.others > 0 {
				it.others-- // the client the loop is on
			}
			it.isNew = fresh[k] && !it.cur && s.Attached == 0
			var wins []item
			for wi := range s.Windows {
				win := &s.Windows[wi]
				wk := rowKey{Host: hk, Inst: h.Inst, Session: s.ID, Window: win.ID}
				if _, hid := hidden[wk]; hid {
					continue
				}
				wit := item{kind: kWindow, key: wk, host: h, sess: s, win: win, local: hit.local, name: win.Name,
					bell: win.Bell, act: win.Activity}
				wit.cur = it.cur && (mk.cur.Window == win.ID || mk.cur.Window == "" && win.Active)
				wins = append(wins, wit)
				it.bell = it.bell || win.Bell
				it.act = it.act || win.Activity
			}
			slices.SortStableFunc(wins, func(a, b item) int { return cmp.Compare(a.win.Index, b.win.Index) })
			w.windows[k] = wins
			hit.cur = hit.cur || it.cur
			hit.bell = hit.bell || it.bell
			hit.act = hit.act || it.act
			if s.Group != "" {
				gk := h.Name + "\x00" + s.Group
				w.groups[gk] = append(w.groups[gk], s.Name)
			}
			sess = append(sess, it)
		}
		slices.SortStableFunc(sess, func(a, b item) int {
			if c := cmp.Compare(a.ago, b.ago); c != 0 {
				return c
			}
			return cmp.Compare(a.name, b.name)
		})
		var dirs []item
		for di := range h.Dirs {
			dr := &h.Dirs[di]
			dirs = append(dirs, item{kind: kDir, key: rowKey{Host: hk, Dir: dr.Path}, host: h, dir: dr, local: hit.local, name: dr.Path})
		}
		w.sessions[h.Name] = sess
		w.dirs[h.Name] = dirs
		w.hosts = append(w.hosts, hit)
	}
	for i := range w.hosts {
		w.byName[w.hosts[i].host.Name] = &w.hosts[i]
	}
	return w
}

// host is the host named name, or nil.
func (w *world) host(name string) *item { return w.byName[name] }

// dirsOf are a host's dirs as listed: git roots, or every entry (all).
func (w *world) dirsOf(host string, all bool) []item {
	ds := w.dirs[host]
	if all {
		return ds
	}
	var out []item
	for _, d := range ds {
		if d.dir.Root {
			out = append(out, d)
		}
	}
	return out
}

// entries are a host's sessions, then its dirs.
func (w *world) entries(host string, all bool) []item {
	s := w.sessions[host]
	d := w.dirsOf(host, all)
	out := make([]item, 0, len(s)+len(d))
	return append(append(out, s...), d...)
}

// session finds a session item by key.
func (w *world) session(k rowKey) *item {
	k = k.sessionKey()
	for _, h := range w.hosts {
		if h.key.Host != k.Host {
			continue
		}
		ss := w.sessions[h.host.Name]
		for i := range ss {
			if ss[i].key == k {
				return &ss[i]
			}
		}
	}
	return nil
}

// find finds any item by key: a session, window or dir.
func (w *world) find(k rowKey) *item {
	switch {
	case k.Dir != "":
		for _, h := range w.hosts {
			if h.key.Host == k.Host {
				ds := w.dirs[h.host.Name]
				for i := range ds {
					if ds[i].key == k {
						return &ds[i]
					}
				}
			}
		}
	case k.Window != "":
		ws := w.windows[k.sessionKey()]
		for i := range ws {
			if ws[i].key == k {
				return &ws[i]
			}
		}
	case k.Session != "":
		return w.session(k)
	}
	return nil
}

// groupPeers are the other members of s's group on its host.
func (w *world) groupPeers(it *item) []string {
	if it.sess == nil || it.sess.Group == "" {
		return nil
	}
	var out []string
	for _, n := range w.groups[it.host.Name+"\x00"+it.sess.Group] {
		if n != it.sess.Name {
			out = append(out, n)
		}
	}
	return out
}

// groupPeerItems are the rows of the other members of s's group.
func (w *world) groupPeerItems(it *item) []*item {
	var out []*item
	ss := w.sessions[it.host.Name]
	for _, n := range w.groupPeers(it) {
		for i := range ss {
			if ss[i].sess.Name == n {
				out = append(out, &ss[i])
			}
		}
	}
	return out
}

// groupLeader reports whether s lists its group's windows in the finder:
// the session the others were made from (its name is the group's), or
// the first member when it is gone.
func (w *world) groupLeader(it *item) bool {
	if it.sess == nil || it.sess.Group == "" {
		return true
	}
	members := w.groups[it.host.Name+"\x00"+it.sess.Group]
	if slices.Contains(members, it.sess.Group) {
		return it.sess.Name == it.sess.Group
	}
	return len(members) > 0 && members[0] == it.sess.Name
}

// worktreeRepo is how many graphemes of a session's name are its repo
// part, when the session is a linked worktree named <repo><sep><branch>
// (0 otherwise).
func worktreeRepo(it *item) int {
	if it.sess == nil || it.sess.Git == nil || it.sess.Git.Repo == "" {
		return 0
	}
	repo := sessionName(it.sess.Git.Repo)
	name := it.sess.Name
	if len(name) <= len(repo)+1 || !strings.EqualFold(name[:len(repo)], repo) {
		return 0
	}
	switch name[len(repo)] {
	case '_', '.', '-', '/', '@':
		return len(graphemes(repo))
	}
	return 0
}

// sessionName is the session name tmux takes for a directory name: a
// leading '.' dropped, '.' and ':' (which tmux forbids) as '_'.
func sessionName(base string) string {
	base = strings.TrimPrefix(base, ".")
	return strings.Map(func(r rune) rune {
		if r == '.' || r == ':' {
			return '_'
		}
		return r
	}, base)
}

// dirSessionName is the session a dir would make: its basename's.
func dirSessionName(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "~" || p == "" {
		return "home"
	}
	return sessionName(path.Base(p))
}

// freeName is name, or when a session on h has it, the first free
// "<name>_<n>" from 2: a dir's session whose name is taken (the fzf
// picker's spelling), never "<name> <n>", which is a grouped duplicate's.
func freeName(h *proto.Host, name string) string {
	taken := func(n string) bool {
		return slices.ContainsFunc(h.Sessions, func(s proto.Session) bool { return s.Name == n })
	}
	if !taken(name) {
		return name
	}
	for n := 2; ; n++ {
		c := name + "_" + strconv.Itoa(n)
		if !taken(c) {
			return c
		}
	}
}

// dupName is the name of session s's grouped duplicate on h: "<name> 2",
// or the next "<name> <n>" free, <name> being the group's origin (a
// duplicate's duplicate is "train 3", not "train 2 2"). A session of that
// name already in s's group, other than s, is the duplicate made before,
// and is returned as found; one of that name not in the group (made by
// hand, or a dir's session) is passed over, never attached as if it were
// the duplicate.
func dupName(h *proto.Host, s *proto.Session) (name string, found bool) {
	base := s.Name
	if s.Group != "" {
		base = s.Group
	}
	for n := 2; ; n++ {
		c := base + " " + strconv.Itoa(n)
		i := slices.IndexFunc(h.Sessions, func(o proto.Session) bool { return o.Name == c })
		if i < 0 {
			return c, false
		}
		o := h.Sessions[i]
		if o.Name == s.Name {
			continue
		}
		if o.Group != "" && (o.Group == s.Group || o.Group == s.Name) {
			return c, true
		}
	}
}

// age is a duration tmux-short: now, 3m, 2h, 4d.
func age(d time.Duration) string {
	switch {
	case d == time.Duration(math.MaxInt64) || d < 0:
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

// gitText is a branch as rows show it: "main", "fix/auth*" when dirty.
func gitText(g *proto.Git) string {
	if g == nil || g.Branch == "" {
		return ""
	}
	if g.Dirty {
		return g.Branch + "*"
	}
	return g.Branch
}

// windowCount is a session's windows and panes: "2 windows · 3 panes".
func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + one + "s"
}
