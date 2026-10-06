package scenario

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/relay"
	"github.com/jerrykal/tower/internal/tmux"
)

// Scenarios of the Atlas dashboard: the columns, the finder's ranking,
// kill and rename asking first, a grouped duplicate, a session in a dir,
// adding a host, and the popup's start.

// Slow sends keys one at a time, a little apart: ESC sent with the next
// key in one write reads as alt+key.
func (t *Term) Slow(keys ...string) {
	t.w.T.Helper()
	for _, k := range keys {
		time.Sleep(120 * time.Millisecond)
		t.Keys(k)
	}
	time.Sleep(120 * time.Millisecond)
}

// sessionFormat is a format of h's session named name.
func sessionFormat(h *Host, name, format string) string {
	out, _ := h.Tmux("list-sessions", "-F", "#{session_name}\t"+format)
	for _, l := range strings.Split(out, "\n") {
		if n, v, ok := strings.Cut(l, "\t"); ok && n == name {
			return v
		}
	}
	return ""
}

// OpenDash opens the dashboard in the terminal's client: the finder.
func (t *Term) OpenDash() {
	t.w.T.Helper()
	t.Keys("M-o")
	t.Wait(Prompt, 6*time.Second)
	time.Sleep(200 * time.Millisecond)
}

// U01: the columns: ^l from the finder shows hosts, sessions and
// windows on the client's own session; h j l walk them, each column
// remembering its row through live changes; ⏎ on a window of another
// host hands off to that window.
func TestU01(t *testing.T) {
	w := NewWorld(t, "u01")
	a := w.Host("A", []string{"alpha", "apple"})
	b := w.Host("B", []string{"bravo", "banana"}, SSHHost())
	b.MustTmux("new-window", "-d", "-t", "bravo:", "-n", "second")
	stdSetup(w, a, b)
	// tmux dates attaches in whole seconds: alpha's attach comes in a
	// later second than B's sessions were made, so it is the most recent
	// and the finder's top row.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	term := w.LoopTo("t", a, nil, "alpha", "^A:alpha")
	term.OpenDash()
	// The finder's preview: the window's layout and its pane.
	term.Wait(`layout · 1:`, 3*time.Second)
	term.Wait(`active · \d+×\d+`, 3*time.Second)
	term.Slow("C-l")
	term.Wait(`NORMAL`, 3*time.Second)
	for _, re := range []string{`\[1\] hosts`, `\[2\] sessions`, `\[3\] windows`, `local server`} {
		term.Wait(re, 3*time.Second)
	}
	if got := term.cursor(); got != "A:alpha" {
		t.Fatalf("the columns open on the client's session, not %q:\n%s", got, term.Screen())
	}
	// B, its sessions filtered to bravo, then its windows.
	term.Slow("h", "j", "l", "/")
	term.Type("bra")
	term.Slow("Escape")
	term.Wait(`B  \x{ebc8} bravo`, 3*time.Second)
	// A session made elsewhere does not move the selection.
	b.NewSession("blueberry")
	w.Eventually(3*time.Second, "blueberry in the view", func() bool { return HasSession(&a.View("").View, "B", "blueberry") })
	time.Sleep(300 * time.Millisecond)
	if got := term.cursor(); got != "B:bravo" {
		t.Fatalf("after a live change the cursor is on %q:\n%s", got, term.Screen())
	}
	term.Slow("l", "j")
	term.Wait(`\x{f04e9} 2:second`, 3*time.Second)
	term.Slow("Enter")
	w.WaitLoop(a, "^B:bravo", 6*time.Second)
	w.Eventually(5*time.Second, "B's client on 2:second", func() bool { return b.ClientWindow() == "2 second" })
}

// U02: the finder ranks what a query names first, so ⏎ takes it: a
// name before a longer one containing it, words in any order, a window
// by name or by number.
func TestU02(t *testing.T) {
	w := NewWorld(t, "u02")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"s150", "s50", "train-llm"}, SSHHost())
	b.MustTmux("rename-window", "-t", "train-llm:1", "claude")
	b.MustTmux("new-window", "-d", "-t", "train-llm:", "-n", "train")
	b.MustTmux("new-window", "-d", "-t", "s150:", "-n", "tensorboard")
	stdSetup(w, a, b)
	term := w.LoopTo("t", a, nil, "alpha", "^A:alpha")
	cl := a.ClientIDs("alpha")[0]
	for q, want := range map[string]string{
		"s50":             "> B  s50",
		"B s5":            "> B  s5", // a host word and a session's start
		"tens":            "> B  s150 › 2:tensorboard",
		"train":           ">     └ 2:train", // the window's whole name beats the session's start
		"llm claude":      ">     └ 1:claude",
		"B:train-llm:2":   ">     └ 2:train",
		"zz-nothing-here": "",
	} {
		out, err := a.UI(cl, nil, append([]string{"find"}, strings.Fields(q)...)...)
		if err != nil {
			t.Fatalf("find %q: %v %s", q, err, out)
		}
		cur := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, ">") {
				cur = l
			}
		}
		if !strings.HasPrefix(cur, want) || want == "" && cur != "" {
			t.Errorf("find %q: cursor on %q, want %q\n%s", q, cur, want, out)
		}
	}
	// Typed into the popup, ⏎ goes where the cursor is.
	term.OpenDash()
	term.Pick("s50")
	w.WaitLoop(a, "^B:s50(:|$)", 6*time.Second)
	term.OpenDash()
	term.Pick("B:train-llm:2")
	w.WaitLoop(a, "^B:train-llm", 6*time.Second)
	w.Eventually(5*time.Second, "B's client on 2:train", func() bool { return b.ClientWindow() == "2 train" })
}

// U03: kill asks first and says what is at stake: windows and panes,
// the other clients it detaches, a last window taking its session;
// any key but y keeps it; y hides the row at once and kills it.
func TestU03(t *testing.T) {
	w := NewWorld(t, "u03")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo", "victim", "solo"}, SSHHost())
	b.MustTmux("new-window", "-d", "-t", "victim:", "-n", "two")
	b.MustTmux("split-window", "-d", "-t", "victim:2")
	stdSetup(w, a, b)
	w.Term("other", b, nil, tmux.Bin(), "-L", b.Sock, "attach", "-t", "victim")
	term := w.LoopTo("t", a, nil, "bravo", "^B:bravo")
	cl := b.ClientIDs("bravo")[0]
	time.Sleep(500 * time.Millisecond)
	out, err := b.UI(cl, nil, "ask-kill", "B", "victim")
	if err != nil || !strings.HasPrefix(out, "kill session B:victim? 2 windows · 3 panes") || !strings.Contains(out, "detaches 1 other client") {
		t.Fatalf("ask-kill: %v %q", err, out)
	}
	if out, err := b.UI(cl, nil, "ask-kill", "B", "solo", "1"); err != nil || !strings.Contains(out, "its last window: the session goes too") {
		t.Fatalf("ask-kill a last window: %v %q", err, out)
	}
	// What runs there, when towerd answers panes.
	b.MustTmux("send-keys", "-t", "victim:2.1", "sleep 300", "Enter")
	time.Sleep(300 * time.Millisecond)
	if out, _ := b.UI(cl, nil, "ask-kill", "B", "victim"); !strings.Contains(out, "sleep running") {
		t.Fatalf("ask-kill with a command running: %q", out)
	}
	// In the popup: n keeps it, y kills it.
	term.OpenDash()
	term.Type("victim")
	time.Sleep(300 * time.Millisecond)
	term.Keys("C-x")
	term.Wait(`CONFIRM`, 3*time.Second)
	term.Wait(`detaches 1 other client`, 3*time.Second)
	term.Slow("n")
	term.Wait(Prompt, 3*time.Second)
	if !b.hasSession("victim") {
		t.Fatal("n killed it")
	}
	gone := term.timeUntil(func() { term.Keys("C-x", "y") }, rowRe("B", "victim"), false, 3*time.Second)
	if gone < 0 {
		t.Fatalf("the row stayed:\n%s", term.Screen())
	}
	w.Eventually(3*time.Second, "victim killed", func() bool { return !b.hasSession("victim") })
	t.Logf("^x y: the row gone after %v", gone.Round(time.Millisecond))
	term.CloseDash()
}

// U04: rename asks with the name prefilled; a taken name keeps the
// prompt open saying so; '.' and ':' become '_'.
func TestU04(t *testing.T) {
	w := NewWorld(t, "u04")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo", "banana"}, SSHHost())
	stdSetup(w, a, b)
	term := w.LoopTo("t", a, nil, "alpha", "^A:alpha")
	term.OpenDash()
	term.Type("bravo")
	time.Sleep(300 * time.Millisecond)
	term.Slow("C-r")
	term.Wait(`RENAME .*bravo`, 3*time.Second)
	term.Slow("C-u")
	term.Type("banana")
	term.Slow("Enter")
	term.Wait(`banana is taken on B`, 3*time.Second)
	term.Slow("C-u")
	term.Type("my.new:name")
	term.Slow("Enter")
	w.Eventually(5*time.Second, "renamed", func() bool { return b.hasSession("my_new_name") && !b.hasSession("bravo") })
	term.Wait(`rename on B: done`, 3*time.Second)
	term.CloseDash()
}

// U05: D duplicates a session as a grouped one, "<name> 2", and attaches
// it; D on the same session again attaches the one already made.
func TestU05(t *testing.T) {
	w := NewWorld(t, "u05")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	stdSetup(w, a, b)
	term := w.LoopTo("t", a, nil, "alpha", "^A:alpha")
	dup := func() {
		term.OpenDash()
		term.Type("bravo")
		time.Sleep(300 * time.Millisecond)
		term.Slow("C-l", "D")
	}
	dup()
	w.WaitLoop(a, "^B:bravo 2", 6*time.Second)
	if group := sessionFormat(b, "bravo 2", "#{session_group}"); group == "" {
		t.Fatalf("bravo 2 is not grouped (%q); B has %q", group, b.Sessions())
	}
	time.Sleep(500 * time.Millisecond)
	dup()
	time.Sleep(time.Second)
	if got := b.Sessions(); !slices.Equal(got, []string{"bravo", "bravo 2"}) {
		t.Fatalf("D again made another: %q", got)
	}
	w.WaitLoop(a, "^B:bravo 2", 6*time.Second)
}

// U06: zoxide dirs in the dashboard: git roots after the sessions, every
// entry after ^g; ⏎ on a dir makes a session named after it there and
// attaches it; a taken name opens the prompt with the first free
// "<name>_<n>" ("<name> <n>" is a grouped duplicate's); `tower _ui open`
// does the same by path.
func TestU06(t *testing.T) {
	w := NewWorld(t, "u06")
	w.repo("src/proj")
	w.repo("work/proj")
	os.MkdirAll(filepath.Join(w.UserHome, "notes"), 0o755)
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, Zoxide("~/src/proj", "~/work/proj", "~/notes"), SSHHost())
	stdSetup(w, a, b)
	term := w.LoopTo("t", a, nil, "alpha", "^A:alpha")
	w.Eventually(6*time.Second, "B's dirs", func() bool {
		h := HostIn(&a.View("").View, "B")
		return h != nil && len(h.Dirs) == 3
	})
	// A git root, typed and ⏎: a session there, attached.
	term.OpenDash()
	term.Type("src proj")
	term.Wait(`B {2,}\x{f114} ~/src/proj +main`, 3*time.Second)
	term.Slow("Enter")
	w.WaitLoop(a, "^B:proj(:@|$)", 6*time.Second)
	if p := sessionFormat(b, "proj", "#{session_path}"); p != filepath.Join(w.UserHome, "src", "proj") {
		t.Fatalf("proj is in %q", p)
	}
	// ~/notes is no git root: listed after ^g only.
	term.OpenDash()
	term.Type("notes")
	term.Wait(`no match`, 3*time.Second)
	term.Slow("C-g")
	term.Wait(`B {2,}\x{f114} ~/notes`, 3*time.Second)
	term.Slow("C-g", "C-u")
	// A taken name: the prompt, with the first free one.
	term.Type("work proj")
	term.Wait(`B {2,}\x{f114} ~/work/proj`, 3*time.Second)
	term.Slow("Enter")
	term.Wait(`NEW SESSION`, 3*time.Second)
	term.Wait(`proj is taken on B`, 3*time.Second)
	term.Slow("Enter")
	w.WaitLoop(a, "^B:proj_2(:@|$)", 6*time.Second)
	if p := sessionFormat(b, "proj_2", "#{session_path}"); p != filepath.Join(w.UserHome, "work", "proj") {
		t.Fatalf("proj_2 is in %q", p)
	}
	// By path, from a script.
	dir := filepath.Join(w.Dir, "elsewhere", "proj")
	os.MkdirAll(dir, 0o700)
	cl := b.ClientIDs("proj_2")[0]
	if out, err := b.UI(cl, nil, "open", "B", dir); err != nil || out != "open on B: done (proj_3)\n" {
		t.Fatalf("open: %v %q", err, out)
	}
	w.WaitLoop(a, "^B:proj_3(:@|$)", 6*time.Second)
	if p := sessionFormat(b, "proj_3", "#{session_path}"); p != dir {
		t.Fatalf("proj_3 is in %q, want %q", p, dir)
	}
}

// U07: adding a host from the dashboard: the picker offers the ssh
// aliases not in the list; picking one runs tower host add's checks in
// its row; a host that fails a check stays, marked, with the reason; x
// removes it after y.
func TestU07(t *testing.T) {
	w := NewWorld(t, "u07")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Home(a)
	w.WaitUp(a, "A")
	os.MkdirAll(filepath.Join(w.UserHome, ".ssh"), 0o700)
	if err := os.WriteFile(filepath.Join(w.UserHome, ".ssh", "config"), []byte("Host B\nHost nas\nHost *.lan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	term := w.LoopTo("t", a, nil, "alpha", "^A:alpha")
	term.OpenDash()
	term.Slow("C-l", "1", "a")
	term.Wait(`ADD HOST`, 3*time.Second)
	term.Wait(`nas`, 3*time.Second)
	term.Type("B")
	time.Sleep(200 * time.Millisecond)
	term.Slow("Enter")
	term.Wait(`B: every check passed`, 20*time.Second)
	hosts, _ := config.LoadHosts(a.Paths().HostsFile())
	if len(hosts) != 1 || hosts[0].Name != "B" || hosts[0].Target() != "B" {
		t.Fatalf("hosts.toml: %+v", hosts)
	}
	// The simulated B runs its tmux on a socket of its own, which the
	// user names in hosts.toml; edits there are taken on a reload.
	hosts[0].Tmux = "-L " + b.Sock
	config.SaveHosts(a.Paths().HostsFile(), hosts)
	a.Call(proto.CallReload, nil, nil)
	w.WaitLink(a, "B", "up", 20*time.Second)
	// An alias whose ssh fails: kept, marked, the reason shown.
	term.Slow("a")
	term.Wait(`ADD HOST`, 3*time.Second)
	if strings.Contains(term.Screen(), "  B\n") {
		t.Fatalf("B is still offered:\n%s", term.Screen())
	}
	term.Type("nas")
	time.Sleep(200 * time.Millisecond)
	term.Slow("Enter")
	term.Wait(`nas ssh: `, 20*time.Second)
	hosts, _ = config.LoadHosts(a.Paths().HostsFile())
	if len(hosts) != 2 || hosts[1].Name != "nas" {
		t.Fatalf("a host that failed its check is kept: %+v", hosts)
	}
	// x on it, after y: off the list.
	w.Eventually(5*time.Second, "nas in the hosts column", func() bool { return strings.Contains(term.Screen(), "nas") })
	term.Slow("G", "x")
	term.Wait(`remove host nas from the list\?`, 3*time.Second)
	term.Slow("y")
	w.Eventually(5*time.Second, "nas removed", func() bool {
		hosts, _ := config.LoadHosts(a.Paths().HostsFile())
		return len(hosts) == 1
	})
	_ = b
}

// U08: the popup's start and a key's echo, measured on a pty against the
// home's towerd: process start to the first frame on the terminal, then
// a typed character to its echo. Run alone for the numbers that matter;
// the thresholds only catch a regression by a multiple.
func TestU08(t *testing.T) {
	w := NewWorld(t, "u08")
	a := w.Host("A", []string{"alpha", "apple", "avocado"})
	b := w.Host("B", []string{"bravo", "banana"}, SSHHost())
	stdSetup(w, a, b)
	bin := towerBin
	if o := os.Getenv("U08_BIN"); o != "" {
		bin = o // an older build, to compare
	}
	ready := regexp.MustCompile(`FIND|sessions>`)
	var starts, keys []time.Duration
	for i := range 15 {
		start, key := popupOnPty(t, a, bin, ready, fmt.Sprint(i))
		starts = append(starts, start)
		keys = append(keys, key)
	}
	logResults(t, "U08", map[string][]time.Duration{"start to first frame": starts, "key to echo": keys})
	if m := median(starts); m > 60*time.Millisecond {
		t.Fatalf("the popup's first frame: median %v", m)
	}
	if m := median(keys); m > 30*time.Millisecond {
		t.Fatalf("a key's echo: median %v", m)
	}
}

// popupOnPty runs the dashboard as the popup does, on a pty of 120×35,
// and times its first frame and one key's echo.
func popupOnPty(t *testing.T, h *Host, bin string, ready *regexp.Regexp, tag string) (time.Duration, time.Duration) {
	t.Helper()
	p, err := relay.OpenPty()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	relay.SetSize(int(p.Slave.Fd()), 35, 120)
	cmd := exec.Command(bin)
	env := h.EnvMap()
	env["TMUX"] = h.SocketPath() + ",0,0"
	env["TOWER_CLIENT"] = "1:1:/dev/null-" + tag
	env["TERM"] = "xterm-256color"
	env["TOWER_LIVE"] = "0"
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = p.Slave, p.Slave, p.Slave
	var mu sync.Mutex
	var out bytes.Buffer
	seen := make(chan struct{}, 64)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := p.Master.Read(buf)
			if n > 0 {
				mu.Lock()
				out.Write(buf[:n])
				mu.Unlock()
				select {
				case seen <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	wait := func(from int, re *regexp.Regexp) bool {
		deadline := time.After(5 * time.Second)
		for {
			mu.Lock()
			ok := re.Match(out.Bytes()[from:])
			mu.Unlock()
			if ok {
				return true
			}
			select {
			case <-seen:
			case <-deadline:
				return false
			}
		}
	}
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	if !wait(0, ready) {
		t.Fatalf("no first frame:\n%q", out.String())
	}
	start := time.Since(t0)
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	from := out.Len()
	mu.Unlock()
	t1 := time.Now()
	p.Master.Write([]byte("q"))
	if !wait(from, regexp.MustCompile(`q`)) {
		t.Fatalf("no echo of q:\n%q", out.String())
	}
	key := time.Since(t1)
	p.Master.Write([]byte("\x1b"))
	return start, key
}
