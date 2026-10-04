package ui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

// ScriptUsage lists the scripted entry points.
const ScriptUsage = `usage: tower _ui <command>

  rows                                   every session, one per line
  find <query…>                          the finder's rows for a query
  goto <host> <session> [<window index>] ⏎ on that row
  kill <host> <session> [<window index>] x, y
  ask-kill <host> <session> [<window index>] the question x asks
  rename <host> <session> <new name>     r
  new <host> [<name>]                    n
  open <host> <dir>                      ⏎ on a dir: a session there, attached
  dup <host> <session>                   D
  preview <host> <session>               the preview's text
`

// Script runs one scripted entry point: the dashboard's own code paths
// without the TUI, for the scenario suite and scripting. Actions print
// "<op> on <host>: done" and return the error otherwise. Hosts are named
// by label or towerd id, sessions by name or id.
func Script(ctx context.Context, c *Conn, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(ScriptUsage)
	}
	op, args := args[0], args[1:]
	need := map[string][2]int{"rows": {0, 0}, "find": {0, 64}, "goto": {2, 3}, "kill": {2, 3}, "ask-kill": {2, 3},
		"rename": {3, 3}, "new": {1, 2}, "open": {2, 2}, "dup": {2, 2}, "preview": {2, 2}}
	n, ok := need[op]
	if !ok || len(args) < n[0] || len(args) > n[1] {
		return errors.New(ScriptUsage)
	}
	d, err := c.Towerd.View(ctx, c.viewArgs())
	if err != nil {
		return err
	}
	var here proto.Ref
	if !d.Owned && c.Client != "" && c.Tmux != nil && (op == "rows" || op == "find") {
		if info, err := c.clientInfo(ctx); err == nil {
			here = proto.Ref{Host: d.Self, Session: info.session, Window: info.window}
		}
	}
	loop := d.Loop
	if loop == "" {
		loop = c.Loop
	}
	mk := marksFor(&d, loop, here)
	switch op {
	case "rows":
		for _, l := range plainRows(&d, mk) {
			fmt.Fprintln(out, l)
		}
		return nil
	case "find":
		w := buildWorld(&d, mk, 0, hostOrder(&d), nil, nil)
		f := finder{}
		f.in.set(strings.Join(args, " "))
		f.build(w, false, true)
		for _, r := range f.rows {
			mark := " "
			if r.key == f.cursor {
				mark = ">"
			}
			fmt.Fprintln(out, mark+" "+findText(&r))
		}
		return nil
	}
	h := findHost(&d, args[0])
	if h == nil {
		return fmt.Errorf("no host %q", args[0])
	}
	switch op {
	case "new":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		if err := reachable(h); err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		if n := startingNote(h); n != "" {
			fmt.Fprintln(out, n)
		}
		ack, err := c.newSession(ctx, h, name, "")
		if err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		fmt.Fprintln(out, done(op, h.Name, ack.Note))
		return nil
	case "open":
		if err := reachable(h); err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		name := freeName(h, dirSessionName(args[1]))
		ack, err := c.newSession(ctx, h, name, args[1])
		if err == nil && ack.Ref == nil {
			err = errors.New("no session made")
		}
		if err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		if _, _, err := c.enter(ctx, refKey(*ack.Ref), ""); err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		fmt.Fprintln(out, done(op, h.Name, name))
		return nil
	}
	s := findSession(h, args[1])
	if s == nil {
		return errors.New(failed(op, h.Name, errGone))
	}
	k := rowKey{Host: h.ID, Inst: h.Inst, Session: s.ID}
	t := target{host: h, sess: s, ref: proto.Ref{Host: h.ID, Name: h.Name, Inst: h.Inst, Session: s.ID, Label: s.Name}}
	if len(args) > 2 && (op == "goto" || op == "kill" || op == "ask-kill") {
		idx, err := strconv.Atoi(args[2])
		w := -1
		for i := range s.Windows {
			if err == nil && s.Windows[i].Index == idx {
				w = i
			}
		}
		if w < 0 {
			return errors.New(failed(op, h.Name, errGone))
		}
		k.Window = s.Windows[w].ID
		t.ref.Window = k.Window
	}
	switch op {
	case "goto":
		if _, _, err := c.enter(ctx, k, ""); err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		fmt.Fprintln(out, done(op, h.Name, ""))
	case "ask-kill":
		m := &Model{c: c, ctx: ctx, now: time.Now, view: d, have: true, hidden: map[rowKey]hide{}, fresh: map[rowKey]bool{},
			checks: map[string]*hostCheck{}}
		m.sel = selection{entry: map[string]rowKey{}, window: map[rowKey]string{}, pane: map[rowKey]string{}}
		m.cap.cache = map[capKey]*capture{}
		m.rebuild()
		k.Host = hostKey(h)
		it := m.w.find(k)
		if it == nil {
			return errors.New(failed(op, h.Name, errGone))
		}
		m.selectItem(it)
		m.focus = colSessions
		if it.kind == kWindow {
			m.focus = colWindows
		}
		m.mode = modeNormal
		if cmd := m.askKill(); cmd != nil {
			if msg, ok := cmd().(panesMsg); ok {
				m.gotPanes(msg)
			}
		}
		if m.confirm == nil {
			return errors.New(m.note.text)
		}
		fmt.Fprintln(out, m.confirm.text())
	case "kill", "rename":
		if err := reachable(h); err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		var ack proto.Ack
		if op == "kill" {
			ack, err = c.kill(ctx, t)
		} else {
			ack, err = c.rename(ctx, t, args[2])
		}
		if err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		fmt.Fprintln(out, done(op, h.Name, ack.Note))
	case "dup":
		if err := reachable(h); err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		name := s.Name + " 2"
		var ref proto.Ref
		if other := findSession(h, name); other != nil && other.Name == name {
			ref = proto.Ref{Host: h.ID, Inst: h.Inst, Session: other.ID}
		} else {
			ack, err := c.dup(ctx, t.ref, name)
			if err == nil && ack.Ref == nil {
				err = errors.New("no session made")
			}
			if err != nil {
				return errors.New(failed(op, h.Name, err))
			}
			ref = *ack.Ref
		}
		if _, _, err := c.enter(ctx, refKey(ref), ""); err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		fmt.Fprintln(out, done(op, h.Name, name))
	case "preview":
		// The windows from the view at once, then the capture.
		fmt.Fprintln(out, previewHeader(h, s))
		if err := reachable(h); err != nil {
			return err
		}
		text, _, err := c.capture(ctx, t)
		if err != nil {
			return errors.New(failed("capture", h.Name, err))
		}
		fmt.Fprint(out, text)
		if text != "" && !strings.HasSuffix(text, "\n") {
			fmt.Fprintln(out)
		}
	}
	return nil
}

// findText is a finder row as plain text.
func findText(r *frow) string {
	switch r.kind {
	case fSession:
		return r.it.host.Name + "  " + r.it.sess.Name
	case fWindow:
		if r.fold {
			return r.it.host.Name + "  " + r.it.sess.Name + " › " + winLabel(r.win)
		}
		return "    └ " + winLabel(r.win)
	case fMore:
		return "    └ +" + strconv.Itoa(r.more) + " more"
	}
	return r.it.host.Name + "  " + r.it.dir.Path
}

// plainRows are every session of every host, one per line: reachable
// hosts first, then by recency across hosts; host, name, window count,
// age and marks in columns. A host with no sessions has one row.
func plainRows(d *proto.Dash, mk marks) []string {
	type row struct {
		band  int
		ago   time.Duration
		host  string
		cells []string
	}
	var rows []row
	for hi := range d.View.Hosts {
		h := &d.View.Hosts[hi]
		status := hostStatus(h)
		if len(h.Sessions) == 0 {
			cells := []string{h.Name, "(no sessions)", "", ""}
			if status != "" {
				cells = append(cells, status)
			}
			rows = append(rows, row{band(h), time.Duration(math.MaxInt64), h.Name, cells})
			continue
		}
		for si := range h.Sessions {
			s := &h.Sessions[si]
			cur := sameSession(mk.cur, h, s)
			prev := !cur && sameSession(mk.prev, h, s)
			others := s.Attached
			if cur && others > 0 {
				others--
			}
			ago := time.Duration(s.Ago) * time.Millisecond
			cells := []string{h.Name, s.Name, strconv.Itoa(len(s.Windows)) + "w", age(ago)}
			switch {
			case cur:
				cells = append(cells, glyphCur)
			case prev:
				cells = append(cells, glyphPrev)
			}
			if others > 0 {
				cells = append(cells, glyphClients+" "+strconv.Itoa(others))
			}
			bell, act := false, false
			for _, w := range s.Windows {
				bell, act = bell || w.Bell, act || w.Activity
			}
			if bell {
				cells = append(cells, glyphBell)
			}
			if act {
				cells = append(cells, glyphAct)
			}
			if status != "" {
				cells = append(cells, status)
			}
			rows = append(rows, row{band(h), ago, h.Name, cells})
		}
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		if c := cmp.Compare(a.band, b.band); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ago, b.ago); c != 0 {
			return c
		}
		if c := cmp.Compare(a.host, b.host); c != 0 {
			return c
		}
		return cmp.Compare(a.cells[1], b.cells[1])
	})
	var widths [4]int
	for _, r := range rows {
		for j := range 4 {
			if r.cells[1] != "(no sessions)" || j == 0 {
				widths[j] = max(widths[j], width(r.cells[j]))
			}
		}
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		var b strings.Builder
		for j, c := range r.cells {
			if j > 0 {
				b.WriteString("  ")
			}
			b.WriteString(c)
			if j < 4 {
				b.WriteString(strings.Repeat(" ", max(0, widths[j]-width(c))))
			}
		}
		out[i] = strings.TrimRight(b.String(), " ")
	}
	return out
}

func findHost(d *proto.Dash, name string) *proto.Host {
	for i := range d.View.Hosts {
		if d.View.Hosts[i].Name == name {
			return &d.View.Hosts[i]
		}
	}
	if name != "" {
		return d.View.HostByID(name)
	}
	return nil
}

func findSession(h *proto.Host, name string) *proto.Session {
	for i := range h.Sessions {
		if h.Sessions[i].Name == name {
			return &h.Sessions[i]
		}
	}
	for i := range h.Sessions {
		if h.Sessions[i].ID == name {
			return &h.Sessions[i]
		}
	}
	return nil
}
