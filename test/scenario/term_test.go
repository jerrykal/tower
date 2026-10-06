package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/tmux"
	"github.com/jerrykal/tower/internal/transport"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// Term is a terminal: the one pane of a tmux server of its own, running a
// command (usually the attach loop) with some host's environment.
type Term struct {
	w    *World
	Name string
	Sock string
}

// Term starts a terminal running argv with host h's environment plus
// extra. After argv exits the pane prints LOOP-EXIT=<code>. On a
// container host argv runs in the container, on a terminal of its own
// there (docker exec): a user at that machine.
func (w *World) Term(name string, h *Host, extra map[string]string, argv ...string) *Term {
	w.T.Helper()
	t := &Term{w: w, Name: name, Sock: "tt-" + w.ID + "-term-" + name}
	w.sockets = append(w.sockets, t.Sock)
	env := h.EnvMap()
	maps.Copy(env, extra)
	keys := slices.Sorted(maps.Keys(env))
	var b strings.Builder
	b.WriteString("env -u TMUX -u TMUX_PANE")
	for _, k := range keys {
		if strings.HasPrefix(k, "TOWER_") || k == "PATH" || k == "HOME" || strings.HasPrefix(k, "XDG_") && strings.HasSuffix(k, "_HOME") || k == "TMPDIR" || k == "TMUX_TMPDIR" ||
			h.ctr != nil && (k == "LANG" || strings.HasPrefix(k, "LC_")) {
			b.WriteString(" " + transport.ShellQuote(k+"="+env[k]))
		}
	}
	b.WriteString(" TERM=xterm-256color")
	for i, a := range argv {
		if i == 0 && h.ctr != nil && a == tmux.Bin() {
			a = ctrTmux
		}
		b.WriteString(" " + transport.ShellQuote(a))
	}
	cmd := b.String()
	if h.ctr != nil {
		cmd = "docker exec -it -u tt -w " + transport.ShellQuote(env["HOME"]) + " " + h.ctr.ctr + " /bin/sh -c " + transport.ShellQuote(cmd)
	} else {
		// The command goes in a script, so a shell reporting argv killed
		// names the script: the whole command line, PATH and all, can
		// fill the screen and push LOOP-EXIT off it.
		script := filepath.Join(w.Dir, "term-"+name+".sh")
		if err := os.WriteFile(script, []byte("exec "+cmd+"\n"), 0o644); err != nil {
			w.T.Fatal(err)
		}
		cmd = "sh " + transport.ShellQuote(script)
	}
	cmd += "; echo LOOP-EXIT=$?; sleep 600"
	// The server's options are in place before its pane starts. The
	// colours make the terminal answer colour queries, as a real one
	// does: a tmux client still waiting on one holds a lone ESC for 500ms
	// (up to 5s after it attaches) instead of its escape-time.
	if _, err := t.tmux("-f", "/dev/null", "start-server", ";",
		"set", "-g", "escape-time", "0", ";",
		"set", "-g", "window-style", "fg=#e0def4,bg=#191724", ";",
		"new-session", "-d", "-s", "term", "-x", "110", "-y", "32", cmd); err != nil {
		w.T.Fatal(err)
	}
	w.terms = append(w.terms, t)
	return t
}

// Loop starts a terminal running the attach loop as on home h.
func (w *World) Loop(name string, h *Host, extra map[string]string) *Term {
	return w.Term(name, h, extra, towerBin)
}

func (t *Term) tmux(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tmux.Bin(), append([]string{"-L", t.Sock}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("terminal tmux %v: %v: %s", args, err, out)
	}
	return string(out), nil
}

// Keys sends tmux key names (Enter, Escape, M-o, C-b, …).
func (t *Term) Keys(keys ...string) {
	t.w.T.Helper()
	if _, err := t.tmux(append([]string{"send-keys", "-t", "term:"}, keys...)...); err != nil {
		t.w.T.Fatal(err)
	}
}

// Type types text literally.
func (t *Term) Type(s string) {
	t.w.T.Helper()
	if _, err := t.tmux("send-keys", "-t", "term:", "-l", s); err != nil {
		t.w.T.Fatal(err)
	}
}

// Screen is what the terminal shows.
func (t *Term) Screen() string {
	out, _ := t.tmux("capture-pane", "-p", "-J", "-t", "term:")
	return out
}

// Wait waits until the screen matches re, failing the test after d.
func (t *Term) Wait(re string, d time.Duration) time.Duration {
	t.w.T.Helper()
	if el, ok := t.WaitOK(re, d); ok {
		return el
	}
	t.w.T.Fatalf("terminal %s: no %q within %v; screen:\n%s", t.Name, re, d, t.Screen())
	return 0
}

// WaitOK is Wait reporting instead of failing.
func (t *Term) WaitOK(re string, d time.Duration) (time.Duration, bool) {
	rx := regexp.MustCompile(re)
	start := time.Now()
	for {
		if rx.MatchString(t.Screen()) {
			return time.Since(start), true
		}
		if time.Since(start) > d {
			return 0, false
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// Pick types a query into the dashboard and presses Enter.
func (t *Term) Pick(query string) {
	t.w.T.Helper()
	t.Type(query)
	time.Sleep(250 * time.Millisecond)
	t.Keys("Enter")
}

// Prompt is what the dashboard shows while it waits for a query: the
// finder's mode pill, where it opens.
const Prompt = `FIND`

// LoopTo starts a loop terminal on home and attaches it to the session
// matching query; re is what the loop's current target must then match.
func (w *World) LoopTo(name string, home *Host, extra map[string]string, query, re string) *Term {
	w.T.Helper()
	t := w.Loop(name, home, extra)
	t.Wait(Prompt, 6*time.Second)
	t.Pick(query)
	w.WaitLoop(home, re, 6*time.Second)
	time.Sleep(300 * time.Millisecond)
	return t
}

// DashTo opens the dashboard in the terminal's current client (M-o) and
// picks query.
func (t *Term) DashTo(query string) {
	t.w.T.Helper()
	t.Keys("M-o")
	t.Wait(Prompt, 6*time.Second)
	t.Pick(query)
}
