package ui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/jerrykal/tower/internal/proto"
)

// pick runs Pick against f with keys typed on its terminal.
func pick(t *testing.T, f *fakeTowerd, o PickOptions, keys ...string) (Choice, error) {
	t.Helper()
	in, w := io.Pipe()
	o.Input, o.Output = in, io.Discard
	go func() {
		for _, k := range keys {
			time.Sleep(30 * time.Millisecond)
			w.Write([]byte(k))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer w.Close()
	return Pick(ctx, f.client, o)
}

func TestPick(t *testing.T) {
	d := testDash()
	d.Owned, d.Loop = false, ""
	f := newFakeTowerd(t, d)
	ch, err := pick(t, f, PickOptions{Loop: "L"}, "ban", "\r")
	want := proto.Ref{Host: "bbbb", Name: "B", Inst: "2:2", Session: "$4", Label: "banana"}
	if err != nil || ch.Target != want || ch.Last {
		t.Fatalf("pick: %+v %v", ch, err)
	}
	if v := f.views[len(f.views)-1]; v.Loop != "L" || v.Client != "" {
		t.Fatalf("the picker reads the view as its loop: %+v", v)
	}
	if n := len(f.actsOf(proto.OpSwitch)); n != 0 {
		t.Fatal("the picker never switches: the loop prepares the target")
	}
	// ⏎ on a host that is down says so; esc then quits.
	if _, err := pick(t, f, PickOptions{Loop: "L"}, "char", "\r", "\x1b"); !errors.Is(err, ErrQuit) {
		t.Fatalf("esc: %v", err)
	}
	// tower dash: esc is "the last target".
	if ch, err := pick(t, f, PickOptions{Loop: "L", Dash: true}, "\x1b"); err != nil || !ch.Last {
		t.Fatalf("dash esc: %+v %v", ch, err)
	}
	// ^c clears the query, then quits, even in tower dash.
	if ch, err := pick(t, f, PickOptions{Loop: "L", Dash: true}, "x", "\x03", "\x03"); !errors.Is(err, ErrQuit) || ch.Last {
		t.Fatalf("^c: %+v %v", ch, err)
	}
	// - picks the loop's previous session.
	if ch, err := pick(t, f, PickOptions{Loop: "L"}, "-"); err != nil || ch.Target.Host != "bbbb" || ch.Target.Session != "$0" {
		t.Fatalf("-: %+v %v", ch, err)
	}
}

func TestPickCancelled(t *testing.T) {
	f := newFakeTowerd(t, testDash())
	in, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := Pick(ctx, f.client, PickOptions{Input: in, Output: io.Discard})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pick: %v", err)
	}
}

func TestView(t *testing.T) {
	m, _, _ := newTestModel(t, testDash(), false)
	for _, size := range [][2]int{{100, 30}, {40, 10}, {30, 5}, {20, 2}} {
		m.width, m.height = size[0], size[1]
		m.scroll()
		v := m.View()
		lines := strings.Split(v.Content, "\n")
		if len(lines) > max(size[1], 3) {
			t.Errorf("%v: %d lines", size, len(lines))
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w > max(size[0], 20) {
				t.Errorf("%v: line %d is %d wide: %q", size, i, w, ansi.Strip(l))
			}
		}
		if !strings.HasPrefix(ansi.Strip(lines[0]), "sessions> ") {
			t.Errorf("%v: prompt %q", size, ansi.Strip(lines[0]))
		}
	}
	m.width, m.height = 100, 30
	m.scroll()
	text := ansi.Strip(m.View().Content)
	for _, s := range []string{"6/6", "A  alpha", "A:alpha  ($0)  1:fish", "switch-client → A:alpha", "⏎ attach"} {
		if !strings.Contains(text, s) {
			t.Errorf("view lacks %q:\n%s", s, text)
		}
	}
	m.view.Note = "home A not connected (2m)"
	if !strings.Contains(ansi.Strip(m.View().Content), "home A not connected (2m)") {
		t.Error("the view's note shows")
	}
}

func TestScript(t *testing.T) {
	f := newFakeTowerd(t, testDash())
	x := &fakeTmux{out: map[string]string{}, err: map[string]error{}}
	c := &Conn{Towerd: Calls{C: f.client}, Tmux: x, Client: testClient}
	sh := func(args ...string) (string, error) {
		var b bytes.Buffer
		err := Script(context.Background(), c, args, &b)
		return b.String(), err
	}
	out, err := sh("rows")
	if err != nil || !strings.HasPrefix(out, "A  alpha    1w  now  "+glyphCur+"\nB  bravo") || strings.Count(out, "\n") != 6 {
		t.Fatalf("rows: %v\n%s", err, out)
	}
	if out, err := sh("kill", "B", "banana"); err != nil || out != "kill on B: done\n" {
		t.Fatalf("kill: %q %v", out, err)
	}
	if k := f.actsOf(proto.OpKill); len(k) != 1 || k[0].Target.Session != "$4" || k[0].Kind != proto.KindSession {
		t.Fatalf("kill sent %+v", k)
	}
	f.setAnswer(func(r proto.Request) proto.Ack {
		if r.Op == proto.OpRename {
			return proto.Ack{Err: "duplicate session: bravo"}
		}
		return proto.Ack{OK: true, Text: "line1\nline2"}
	})
	if _, err := sh("rename", "B", "banana", "bravo"); err == nil || err.Error() != "rename on B: duplicate session: bravo" {
		t.Fatalf("rename error: %v", err)
	}
	if out, err := sh("new", "D", "delta"); err != nil || out != "starting tmux on D…\nnew on D: done\n" {
		t.Fatalf("new on a host with no server: %q %v", out, err)
	}
	if out, err := sh("preview", "B", "$0"); err != nil || out != "B:bravo  ($0)  1:fish  2:nvim\nline1\nline2\n" {
		t.Fatalf("preview: %q %v", out, err)
	}
	if out, err := sh("goto", "A", "apple"); err != nil || out != "goto on A: done\n" {
		t.Fatalf("goto: %q %v", out, err)
	}
	if out, err := sh("goto", "B", "bravo", "2"); err != nil || out != "goto on B: done\n" {
		t.Fatalf("goto window: %q %v", out, err)
	}
	if sw := f.actsOf(proto.OpSwitch); len(sw) != 1 || sw[0].Target.Window != "@3" {
		t.Fatalf("goto window sent %+v", sw)
	}
	if _, err := sh("goto", "B", "nope"); err == nil || err.Error() != "goto on B: selection is gone" {
		t.Fatalf("goto gone: %v", err)
	}
	if _, err := sh("kill", "C", "charlie"); err == nil || err.Error() != "kill on C: C is down: timed out" {
		t.Fatalf("kill on a down host: %v", err)
	}
	if _, err := sh("nope"); err == nil {
		t.Fatal("an unknown command fails")
	}
}

func TestOpen(t *testing.T) {
	x := &fakeTmux{out: map[string]string{"display-message": testClient + "\n"}, err: map[string]error{}}
	t.Setenv("TOWER_HOME", "/h o'me")
	err := Open(context.Background(), x, "/bin/tower", map[string]string{"TOWER_TMUX": "-L work", "TOWER_MKEY": ""})
	if err != nil {
		t.Fatal(err)
	}
	p := x.called("display-popup")
	if len(p) != 1 {
		t.Fatalf("calls %v", x.calls)
	}
	args := p[0]
	cmd := args[len(args)-1]
	if !strings.HasPrefix(strings.Join(args, " "), "display-popup -E -c /dev/ttys042 ") {
		t.Fatalf("popup %q", args)
	}
	for _, s := range []string{"TOWER_CLIENT=" + testClient, "TOWER_TMUX='-L work'", `TOWER_HOME='/h o'"'"'me'`, "exec /bin/tower dash"} {
		if !strings.Contains(cmd, s) {
			t.Errorf("popup command lacks %s: %s", s, cmd)
		}
	}
	if strings.Contains(cmd, "TOWER_MKEY") {
		t.Error("an empty variable is left out")
	}
}

// TestActDeadlineSkew: a request's deadline is in tower's clock
// (stream.Now, TOWER_TEST_SKEW included), the one towerd checks it
// against. The skew is read as the process starts, so the check runs in a
// child.
func TestActDeadlineSkew(t *testing.T) {
	if os.Getenv("UI_SKEW_CHILD") == "1" {
		f := newFakeTowerd(t, testDash())
		c := &Conn{Towerd: Calls{C: f.client}}
		c.act(context.Background(), proto.Request{Op: proto.OpKill})
		acts := f.actsOf(proto.OpKill)
		if len(acts) != 1 {
			t.Fatalf("acts %v", acts)
		}
		ahead := acts[0].Deadline - time.Now().UnixMilli()
		if ahead < 3600000 || ahead > 3600000+ackTimeout().Milliseconds()+1000 {
			t.Fatalf("the deadline is %dms ahead of the wall clock, want the skew (3600000ms) plus the timeout", ahead)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestActDeadlineSkew$", "-test.count=1")
	cmd.Env = append(os.Environ(), "UI_SKEW_CHILD=1", "TOWER_TEST_SKEW=3600000")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
