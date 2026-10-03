package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jerrykal/tower/internal/proto"
)

// ScriptUsage lists the scripted entry points.
const ScriptUsage = `usage: tower _ui <command>

  rows                                  the rows as the picker shows them
  goto <host> <session> [<window index>] ⏎ on that row
  kill <host> <session>                 ^x
  rename <host> <session> <new name>    ^r
  new <host> [<name>]                   ^n
  preview <host> <session>              the preview's text
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
	need := map[string][2]int{"rows": {0, 0}, "goto": {2, 3}, "kill": {2, 2}, "rename": {3, 3}, "new": {1, 2}, "preview": {2, 2}}
	n, ok := need[op]
	if !ok || len(args) < n[0] || len(args) > n[1] {
		return errors.New(ScriptUsage)
	}
	d, err := c.Towerd.View(ctx, c.viewArgs())
	if err != nil {
		return err
	}
	if op == "rows" {
		var here proto.Ref
		if !d.Owned && c.Client != "" && c.Tmux != nil {
			if info, err := c.clientInfo(ctx); err == nil {
				here = proto.Ref{Host: d.Self, Session: info.session, Window: info.window}
			}
		}
		loop := d.Loop
		if loop == "" {
			loop = c.Loop
		}
		rows := sessionRows(&d, marksFor(&d, loop, here), 0)
		cols := layout(rows)
		for i := range rows {
			fmt.Fprintln(out, rows[i].plain(cols))
		}
		return nil
	}
	h := findHost(&d, args[0])
	if h == nil {
		return fmt.Errorf("no host %q", args[0])
	}
	if op == "new" {
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
		ack, err := c.newSession(ctx, h, name)
		if err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		fmt.Fprintln(out, done(op, h.Name, ack.Note))
		return nil
	}
	s := findSession(h, args[1])
	if s == nil {
		return errors.New(failed(op, h.Name, errGone))
	}
	k := rowKey{Host: h.ID, Inst: h.Inst, Session: s.ID}
	t := target{host: h, sess: s, ref: proto.Ref{Host: h.ID, Name: h.Name, Inst: h.Inst, Session: s.ID, Label: s.Name}}
	switch op {
	case "goto":
		if len(args) > 2 {
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
		}
		if _, _, err := c.enter(ctx, k); err != nil {
			return errors.New(failed(op, h.Name, err))
		}
		fmt.Fprintln(out, done(op, h.Name, ""))
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
	case "preview":
		// The windows from the view at once, then the capture.
		fmt.Fprintln(out, previewHeader(h, s))
		if err := reachable(h); err != nil {
			return err
		}
		text, err := c.capture(ctx, t)
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
