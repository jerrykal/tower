package scenario

import (
	"bytes"
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Helpers for the attach loop's scenarios.

// UI runs `tower _ui args…` on the host as the dashboard of client (a
// TOWER_CLIENT value) would, inside that host's tmux.
func (h *Host) UI(client string, extra map[string]string, args ...string) (string, error) {
	env := map[string]string{"TMUX": h.SocketPath() + ",0,0", "TOWER_CLIENT": client}
	maps.Copy(env, extra)
	return h.TowerEnv(env, append([]string{"_ui"}, args...)...)
}

// Record starts recording every byte the terminal's program writes.
func (t *Term) Record() {
	t.w.T.Helper()
	path := filepath.Join(t.w.Dir, "term-"+t.Name+".raw")
	if _, err := t.tmux("pipe-pane", "-t", "term:", "-o", "cat >> "+path); err != nil {
		t.w.T.Fatal(err)
	}
}

// Recording is what the terminal's program wrote since Record.
func (t *Term) Recording() []byte {
	b, _ := os.ReadFile(filepath.Join(t.w.Dir, "term-"+t.Name+".raw"))
	return b
}

// Sequences the frame checks look for.
var (
	seqHold  = []byte("\x1b[?2026h")
	seqFree  = []byte("\x1b[?2026l")
	seqLeave = []byte("\x1b[?1049l")
	seqEnter = []byte("\x1b[?1049h")
)

// frameEvent is a screen change (leaving or entering the alternate
// screen) and whether synchronized output held the frame at it.
type frameEvent struct {
	at    int
	enter bool
	held  bool
}

// frames replays b: the alternate-screen changes with the hold state at
// each, and whether the frame is held at the end.
func frames(b []byte) ([]frameEvent, bool) {
	var ev []frameEvent
	held := false
	for i := 0; i < len(b); i++ {
		switch {
		case bytes.HasPrefix(b[i:], seqHold):
			held = true
		case bytes.HasPrefix(b[i:], seqFree):
			held = false
		case bytes.HasPrefix(b[i:], seqLeave):
			ev = append(ev, frameEvent{at: i, held: held})
		case bytes.HasPrefix(b[i:], seqEnter):
			ev = append(ev, frameEvent{at: i, enter: true, held: held})
		}
	}
	return ev, held
}

// LoopPids are the pids in the home's loop-*.pid files.
func (h *Host) LoopPids() []int {
	files, _ := filepath.Glob(filepath.Join(h.Paths().StateDir, "loop-*.pid"))
	var out []int
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// StandbyPids are the processes of standby shims and their ssh sessions
// to host h ("attach --standby" with h's tmux socket).
func (w *World) StandbyPids(h *Host) []int {
	out, _ := exec.Command("pgrep", "-f", "attach --standby .*-L "+h.Sock+"( |'|$)").Output()
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if n, err := strconv.Atoi(f); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

// MarksSince are the marks' texts from index i on.
func (w *World) MarkTexts() []string {
	var out []string
	for _, m := range w.ReadMarks() {
		out = append(out, m.What)
	}
	return out
}

// CountMarks counts the marks equal to what.
func (w *World) CountMarks(what string) int {
	n := 0
	for _, m := range w.ReadMarks() {
		if m.What == what {
			n++
		}
	}
	return n
}

// WaitMark waits until a mark equal to what appears.
func (w *World) WaitMark(what string, d time.Duration) {
	w.T.Helper()
	w.Eventually(d, "the mark "+what, func() bool { return w.CountMarks(what) > 0 })
}

// Route is how the last attach reached its host, judged from the marks:
// "standby", "session" (a new relayed session) or "plain".
func (w *World) Route() string {
	route := ""
	for _, m := range w.ReadMarks() {
		switch m.What {
		case "attach":
			route = "plain"
		case "attach: session":
			route = "session"
		case "standby: taken":
			route = "standby"
		}
	}
	return route
}

// ClientWindow is "index name" of the window of h's (only) non-control
// client.
func (h *Host) ClientWindow() string {
	out, _ := h.Tmux("list-clients", "-F", "#{client_control_mode} #{window_index} #{window_name}")
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if rest, ok := strings.CutPrefix(l, "0 "); ok {
			return rest
		}
	}
	return ""
}

// RestartServer kills every session of h's server, waits for it to exit
// and starts it again with session.
func (w *World) RestartServer(h *Host, session string) {
	w.T.Helper()
	killSessions(w.tmuxAt(h.Sock))
	if !waitNoServer(w.tmuxAt(h.Sock), 5*time.Second) {
		w.T.Fatalf("%s's server did not exit", h.Name)
	}
	h.NewSession(session)
}

// sttyOf is `stty -g` of a terminal's pane tty.
func (t *Term) Stty() string {
	tty, err := t.tmux("display-message", "-p", "-t", "term:", "#{pane_tty}")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f, err := os.Open(strings.TrimSpace(tty))
	if err != nil {
		return ""
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, "stty", "-g")
	cmd.Stdin = f
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}
