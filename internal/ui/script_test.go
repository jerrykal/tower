package ui

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jerrykal/tower/internal/proto"
)

func TestScriptAtlas(t *testing.T) {
	d := testDash()
	d.View.Hosts[1].Sessions[0].Windows[1].Panes = 2
	f := newFakeTowerd(t, d)
	f.setAnswer(func(r proto.Request) proto.Ack {
		switch r.Op {
		case proto.OpPanes:
			return proto.Ack{OK: true, Panes: []proto.Pane{{ID: "%1", Command: "zsh"}, {ID: "%2", Command: "nvim"}}}
		case proto.OpDup, proto.OpNew:
			ref := proto.Ref{Host: r.Target.Host, Inst: r.Target.Inst, Session: "$9"}
			f.update(func(d *proto.Dash) {
				h := d.View.HostByID(r.Target.Host)
				h.Sessions = append(h.Sessions, proto.Session{ID: "$9", Name: r.Name})
			})
			return proto.Ack{OK: true, Ref: &ref}
		}
		return proto.Ack{OK: true}
	})
	x := &fakeTmux{out: map[string]string{}, err: map[string]error{}}
	c := &Conn{Towerd: Calls{C: f.client}, Tmux: x, Client: testClient}
	sh := func(args ...string) (string, error) {
		var b bytes.Buffer
		err := Script(context.Background(), c, args, &b)
		return b.String(), err
	}
	if out, err := sh("find", "nvim", "B"); err != nil || out != "  B  bravo\n>     └ 2:nvim\n" {
		t.Fatalf("find: %q %v", out, err)
	}
	if out, err := sh("ask-kill", "B", "bravo"); err != nil || out != "kill session B:bravo? 2 windows · 3 panes · nvim running\n" {
		t.Fatalf("ask-kill: %q %v", out, err)
	}
	if out, err := sh("ask-kill", "A", "alpha", "1"); err != nil || !strings.Contains(out, "the session goes too") {
		t.Fatalf("ask-kill a last window: %q %v", out, err)
	}
	if out, err := sh("dup", "B", "bravo"); err != nil || out != "dup on B: done (bravo 2)\n" {
		t.Fatalf("dup: %q %v", out, err)
	}
	if dup := f.actsOf(proto.OpDup); len(dup) != 1 || dup[0].Name != "bravo 2" {
		t.Fatalf("dup sent %+v", dup)
	}
	if out, err := sh("open", "A", "~/src/alpha"); err != nil || out != "open on A: done (alpha 2)\n" {
		t.Fatalf("open: %q %v", out, err)
	}
	if n := f.actsOf(proto.OpNew); len(n) != 1 || n[0].Dir != "~/src/alpha" || n[0].Name != "alpha 2" {
		t.Fatalf("open sent %+v", n)
	}
}
