package scenario

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// The real-host scenarios run over real ssh against the hosts named in
// TOWER_REAL (two ssh hosts, comma-separated), from the main session
// only. On each host they use only the tmux socket realSock and only
// realBase; the user's own tmux, tower and demo there are never touched.
// Teardown stops the home first (a live home would reconnect and start the
// remote towerd again), then cleans each host and checks it is clean.
const (
	realSock = "tower-test-harness"
	realBase = ".cache/tower-test/harness" // under the remote home
)

func realHosts(t *testing.T) []string {
	v := os.Getenv("TOWER_REAL")
	if v == "" {
		t.Skip("real hosts: set TOWER_REAL=<host>,<host>")
	}
	return strings.Split(v, ",")
}

// rssh runs a shell line on host h over real ssh, with the user's own ssh
// config.
func rssh(h, line string, stdin []byte) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", h, line)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustSSH(t *testing.T, h, line string) string {
	t.Helper()
	out, err := rssh(h, line, nil)
	if err != nil {
		t.Fatalf("ssh %s %q: %v\n%s", h, line, err, out)
	}
	return out
}

// realBuild cross-builds tower for linux/amd64 at the suite's version.
func realBuild(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, "tower-linux-amd64")
	cmd := exec.Command("go", "build", "-o", out, "-ldflags", "-X github.com/jerrykal/tower/internal/version.Version="+Version, "../../cmd/tower")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-build: %v\n%s", err, b)
	}
	return out
}

// realTmuxConf is the test server's config on a real host: M-o runs the
// dashboard of the harness's tower with its TOWER_HOME.
func realTmuxConf(sync bool) string {
	tower := "~/" + realBase + "/tower"
	popup := "TOWER_HOME=~/" + realBase + "/home TOWER_CLIENT=#{client_pid}:#{client_created}:#{client_name} " + tower + " 2>>~/" + realBase + "/dash.log"
	lines := []string{
		"set -g exit-empty on",
		"set -g default-shell /bin/sh",
		"set -g escape-time 10",
		"set -g status-left '[#{host_short}:#S] '",
		"set -g status-left-length 40",
		`bind -n M-o run-shell -C "display-popup -E -w 100% -h 100% '` + popup + `'"`,
	}
	if sync {
		lines = append(lines, `set -as terminal-features ",*:sync"`)
	}
	return strings.Join(lines, "\n") + "\n"
}

// realHost prepares host h: the harness directory, the binary (unless
// installOnly), the tmux config, and a test server with sessions. It
// registers the host's teardown, which first stops w's homes.
func realHost(t *testing.T, w *World, h, bin string, sync bool, sessions ...string) {
	t.Helper()
	// Never take over a server we did not start.
	if out, err := rssh(h, "tmux -L "+realSock+" list-sessions", nil); err == nil {
		t.Fatalf("%s already has a %s server: %s", h, realSock, out)
	}
	t.Cleanup(func() { realTeardown(t, w, h) })
	mustSSH(t, h, "mkdir -p ~/"+realBase+"/home")
	if bin != "" {
		b, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := rssh(h, "cat > ~/"+realBase+"/tower.tmp && chmod 755 ~/"+realBase+"/tower.tmp && mv -f ~/"+realBase+"/tower.tmp ~/"+realBase+"/tower", b); err != nil {
			t.Fatalf("upload to %s: %v\n%s", h, err, out)
		}
	}
	if out, err := rssh(h, "cat > ~/"+realBase+"/tmux.conf", []byte(realTmuxConf(sync))); err != nil {
		t.Fatalf("config to %s: %v\n%s", h, err, out)
	}
	for i, s := range sessions {
		line := "tmux -L " + realSock + " new-session -d -s " + s
		if i == 0 {
			line = "tmux -L " + realSock + " -f ~/" + realBase + "/tmux.conf new-session -d -s " + s + " -x 100 -y 30"
		}
		mustSSH(t, h, line)
	}
}

// stopHomes ends w's terminals and home towerds, so nothing reconnects
// to a host being cleaned.
func (w *World) stopHomes() {
	for _, term := range w.terms {
		killSessions(localTmux(term.Sock))
	}
	for _, c := range w.spawned {
		if c.Process != nil {
			c.Process.Signal(syscall.SIGTERM)
		}
	}
	for _, h := range w.hosts {
		if b, err := os.ReadFile(filepath.Join(h.Paths().StateDir, "towerd.pid")); err == nil {
			var pid int
			fmt.Sscan(string(b), &pid)
			if pid > 1 {
				syscall.Kill(pid, syscall.SIGTERM)
				for i := 0; i < 100 && alive(pid); i++ {
					time.Sleep(50 * time.Millisecond)
				}
			}
		}
	}
}

// realTeardown cleans host h: its harness towerd stopped, only the test
// server's own sessions ended, the harness directory removed. It then
// checks nothing of the harness is left there.
func realTeardown(t *testing.T, w *World, h string) {
	w.stopHomes()
	home := "TOWER_HOME=~/" + realBase + "/home"
	rssh(h, home+" ~/"+realBase+"/tower stop --tmux '-L "+realSock+"' 2>/dev/null; true", nil)
	for _, v := range []string{"install/current/tower"} {
		rssh(h, home+" ~/"+realBase+"/"+v+" stop --tmux '-L "+realSock+"' 2>/dev/null; true", nil)
	}
	// Processes the harness started there (a standby shim waiting for its
	// go line, a bridge): ours, under the harness directory only.
	rssh(h, "pkill -f '"+realBase+"/' ; true", nil)
	time.Sleep(500 * time.Millisecond)
	// Only ids of sessions, never names a banner could make up.
	out, _ := rssh(h, "tmux -L "+realSock+" list-sessions -F '#{session_id}'", nil)
	for _, id := range regexp.MustCompile(`(?m)^\$[0-9]+$`).FindAllString(out, -1) {
		rssh(h, "tmux -L "+realSock+" kill-session -t '"+id+"'", nil)
	}
	gone := false
	for i := 0; i < 50; i++ {
		if _, err := rssh(h, "tmux -L "+realSock+" list-sessions", nil); err != nil {
			gone = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !gone {
		t.Errorf("%s: the %s server outlived its sessions", h, realSock)
	}
	rssh(h, "rm -rf ~/"+realBase+"; rmdir ~/.cache/tower-test 2>/dev/null; true", nil)
	left, _ := rssh(h, "pgrep -af 'tower-test/harness' | grep -v pgrep; ls -d ~/"+realBase+" 2>/dev/null; true", nil)
	if strings.TrimSpace(left) != "" {
		t.Errorf("%s: left after teardown:\n%s", h, left)
	}
}

// realWorld is a world whose home uses real ssh with the user's ssh
// config (and so the real HOME), at production timings.
func realWorld(t *testing.T, id string) (*World, *Host) {
	w := NewWorld(t, id)
	w.Timing(ProductionTimings)
	home, _ := os.UserHomeDir()
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	a := w.Host("A", []string{"rt-local"}, Env("TOWER_SSH", ssh), Env("HOME", home),
		Env("XDG_CONFIG_HOME", filepath.Join(home, ".config")), Env("TOWER_TEST_PICKER", "1"))
	return w, a
}

func realEntry(h string, pinned bool) config.Host {
	c := config.Host{Name: h, SSH: h, Tmux: "-L " + realSock, Home: "~/" + realBase + "/home"}
	if pinned {
		c.Tower = "~/" + realBase + "/tower"
	}
	return c
}

// TestR01 runs laptop → h1 → h2 over real ssh: bridges, an unresolvable
// host, control paths, an attach, a hand-off, a kill relayed from one
// remote's dashboard to the other, a remote dashboard's rows, a wake.
func TestR01(t *testing.T) {
	hs := realHosts(t)
	if len(hs) < 2 {
		t.Skip("R01 needs two hosts")
	}
	h1, h2 := hs[0], hs[1]
	w, a := realWorld(t, "r01")
	bin := realBuild(t, w.Dir)
	realHost(t, w, h1, bin, false, "rt-"+h1, "rt-"+h1+"-b")
	realHost(t, w, h2, bin, false, "rt-"+h2, "rt-"+h2+"-b")

	start := time.Now()
	w.Home(a, realEntry(h1, true), realEntry(h2, true),
		config.Host{Name: "bogus", SSH: "tower-test-nonexistent.invalid", Tower: "tower"})
	for _, h := range []string{h1, h2} {
		w.WaitLink(a, h, "up", 20*time.Second)
		l := w.Link(a, h)
		t.Logf("%s up after %v: os %s, version %s, protocol %d", h, time.Since(start).Round(time.Millisecond), l.OS, l.Version, l.Proto)
	}
	d := w.WaitLink(a, "bogus", "down", 15*time.Second)
	if r := w.Link(a, "bogus").Reason; !strings.Contains(r, "cannot resolve host name") {
		t.Fatalf("bogus is down for %q", r)
	}
	t.Logf("bogus down in %v", d.Round(time.Millisecond))
	if w.Link(a, h1).ID == w.Link(a, h2).ID {
		t.Fatal("two hosts with one towerd id")
	}
	socks, _ := filepath.Glob(filepath.Join(a.Paths().CMDir(), "*"))
	longest := 0
	for _, s := range socks {
		longest = max(longest, len(s))
	}
	if len(socks) < 2 || longest >= 104 {
		t.Fatalf("control sockets %v (longest %d bytes)", socks, longest)
	}
	t.Logf("control sockets: %d, longest path %d bytes", len(socks), longest)

	term := w.Loop("t", a, nil)
	term.Wait(Prompt, 10*time.Second)
	at := time.Now()
	term.Pick("rt-" + h1 + " ")
	w.WaitLoop(a, "^"+regexp.QuoteMeta(h1)+":rt-"+regexp.QuoteMeta(h1)+"(:|$)", 15*time.Second)
	t.Logf("picker → attached to %s in %v", h1, time.Since(at).Round(time.Millisecond))
	term.Wait(`\[`+regexp.QuoteMeta(h1)+`[^:]*:rt-`+regexp.QuoteMeta(h1)+`\]|rt-`+regexp.QuoteMeta(h1), 8*time.Second)

	time.Sleep(time.Second)
	at = time.Now()
	term.DashTo("rt-" + h2 + "-b")
	w.WaitLoop(a, "^"+regexp.QuoteMeta(h2)+":rt-"+regexp.QuoteMeta(h2)+"-b", 15*time.Second)
	t.Logf("hand-off %s → %s from the dashboard on %s: %v", h1, h2, h1, time.Since(at).Round(time.Millisecond))

	time.Sleep(time.Second)
	at = time.Now()
	mustSSH(t, h2, "true")
	sshTime := time.Since(at)

	// A kill from h2's dashboard, relayed through the home to h1.
	mustSSH(t, h1, "tmux -L "+realSock+" new-session -d -s rt-victim")
	time.Sleep(time.Second)
	info := strings.Fields(mustSSH(t, h2, "tmux -L "+realSock+" list-clients -F '#{client_control_mode} #{client_pid}:#{client_created}:#{client_name}' | grep '^0 '; tmux -L "+realSock+" display-message -p '#{socket_path}'"))
	if len(info) < 3 {
		t.Fatalf("no client on %s: %v", h2, info)
	}
	client, sp := info[1], info[len(info)-1]
	ui := "TOWER_HOME=~/" + realBase + "/home TMUX=" + sp + ",0,0 TOWER_CLIENT=" + client + " ~/" + realBase + "/tower _ui"
	at = time.Now()
	out := mustSSH(t, h2, ui+" kill "+h1+" rt-victim")
	killTime := time.Since(at)
	if !strings.Contains(out, "done") {
		t.Fatalf("kill from %s: %s", h2, out)
	}
	if _, err := rssh(h1, "tmux -L "+realSock+" has-session -t =rt-victim", nil); err == nil {
		t.Fatal("rt-victim survived its kill")
	}
	t.Logf("kill on %s from %s's dashboard: %v, of which ssh %v", h1, h2, killTime.Round(time.Millisecond), sshTime.Round(time.Millisecond))
	at = time.Now()
	rows := mustSSH(t, h2, ui+" rows")
	if !strings.Contains(rows, "rt-"+h1) || !strings.Contains(rows, "rt-local") {
		t.Fatalf("%s's rows lack rt-%s or rt-local:\n%s", h2, h1, rows)
	}
	t.Logf("%s's dashboard rows: %v (ssh included)", h2, time.Since(at).Round(time.Millisecond))

	// A wake resets every stream and master; the loop gets back to h2.
	before := map[string]int{h1: w.Link(a, h1).Attempts, h2: w.Link(a, h2).Attempts}
	at = time.Now()
	if err := a.Call(proto.CallWake, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{h1, h2} {
		w.Eventually(20*time.Second, h+" reconnected", func() bool {
			l := w.Link(a, h)
			return l.Attempts > before[h] && l.Status == proto.StatusUp
		})
	}
	t.Logf("streams back after the wake in %v", time.Since(at).Round(time.Millisecond))
	w.Eventually(20*time.Second, "the loop back on "+h2, func() bool {
		out, _ := rssh(h2, "tmux -L "+realSock+" list-clients -F '#{client_control_mode} #{session_name}'", nil)
		return strings.Contains(out, "0 rt-"+h2+"-b")
	})
	t.Logf("client back on %s after the wake in %v", h2, time.Since(at).Round(time.Millisecond))
	for _, h := range []string{h1, h2} {
		l := w.Link(a, h)
		t.Logf("%s: rtt %dms, offset %dms, rx %d, tx %d", h, l.RTT, l.Offset, l.Rx, l.Tx)
	}

	term.Keys("C-b", "d")
	term.Wait(`LOOP-EXIT=0`, 10*time.Second)
	w.stopHomes()
	for _, h := range []string{h1, h2} {
		rssh(h, "TOWER_HOME=~/"+realBase+"/home ~/"+realBase+"/tower stop --tmux '-L "+realSock+"'", nil)
	}
	time.Sleep(time.Second)
	for _, h := range []string{h1, h2} {
		if out, _ := rssh(h, "tmux -L "+realSock+" list-sessions -F '#{session_name}'", nil); strings.Contains(out, "_tower") {
			t.Errorf("%s keeps _tower after stop: %s", h, out)
		}
		if out, _ := rssh(h, "pgrep -af '"+realBase+"/tower towerd' | grep -v pgrep; true", nil); strings.TrimSpace(out) != "" {
			t.Errorf("%s keeps a towerd after stop: %s", h, out)
		}
	}
}

// TestR02 records the terminal through a hand-off from a real host to
// the laptop: the frame is held across the old client leaving and the
// new one entering, and released after.
func TestR02(t *testing.T) {
	hs := realHosts(t)
	h := hs[0]
	w, a := realWorld(t, "r02")
	a.Set("TOWER_TEST_TIMING", w.Marks)
	a.MustTmux("set", "-as", "terminal-features", ",*:sync")
	bin := realBuild(t, w.Dir)
	realHost(t, w, h, bin, true, "rt-"+h)
	w.Home(a, realEntry(h, true))
	w.WaitLink(a, h, "up", 20*time.Second)

	rec := filepath.Join(w.Dir, "typescript")
	term := w.Term("t", a, nil, "script", "-q", "-F", rec, towerBin)
	term.Wait(Prompt, 10*time.Second)
	term.Pick("rt-" + h)
	w.WaitLoop(a, "^"+regexp.QuoteMeta(h)+":rt-"+regexp.QuoteMeta(h), 15*time.Second)
	time.Sleep(time.Second)
	w.ClearMarks()
	term.DashTo("rt-local")
	w.WaitLoop(a, "^A:rt-local", 15*time.Second)
	// The host left gets a standby; the switch back goes through it.
	w.Eventually(20*time.Second, "a standby on "+h+" ready", func() bool {
		for _, m := range w.ReadMarks() {
			if m.What == "standby: ready "+h {
				return true
			}
		}
		return false
	})
	w.ClearMarks()
	term.DashTo("rt-" + h)
	w.WaitLoop(a, "^"+regexp.QuoteMeta(h)+":rt-"+regexp.QuoteMeta(h), 15*time.Second)
	time.Sleep(time.Second)
	logSwitch(t, w, "laptop → "+h)

	mark, _ := os.ReadFile(rec)
	w.ClearMarks()
	term.DashTo("rt-local")
	w.WaitLoop(a, "^A:rt-local", 15*time.Second)
	time.Sleep(2 * time.Second)
	logSwitch(t, w, h+" → laptop")
	all, _ := os.ReadFile(rec)
	b := all[len(mark):]
	leave := bytes.LastIndex(b, []byte("\x1b[?1049l"))
	enter := bytes.LastIndex(b, []byte("\x1b[?1049h"))
	if leave < 0 || enter <= leave {
		t.Fatalf("no leave then enter in the recording (leave %d, enter %d)", leave, enter)
	}
	held := func(at int) bool {
		on := bytes.LastIndex(b[:at], []byte("\x1b[?2026h"))
		off := bytes.LastIndex(b[:at], []byte("\x1b[?2026l"))
		return on > off
	}
	if !held(leave) || !held(enter) {
		t.Fatalf("the frame is not held: at the old client leaving %v, at the new one entering %v", held(leave), held(enter))
	}
	if held(len(b)) {
		t.Fatal("the frame is still held at the end")
	}
	t.Logf("held across the leave (byte %d) and the enter (byte %d), released after", leave, enter)

	// With the loop gone, its standby shims on the host exit by
	// themselves once their heartbeats stop.
	w.stopHomes()
	start := time.Now()
	w.Eventually(45*time.Second, "the standby shims on "+h+" exit", func() bool {
		// The shim itself (its argv[0] is the harness's tower), not a
		// process whose arguments merely mention it (tailscaled's
		// helper for the session does).
		out, _ := rssh(h, "pgrep -f '^[^ ]*"+realBase+"/tower attach --standby' | head -1; true", nil)
		return strings.TrimSpace(out) == ""
	})
	t.Logf("standby shims on %s gone %v after the loop", h, time.Since(start).Round(100*time.Millisecond))
}

// logSwitch logs the time from the switch stored to the home seeing the
// new client, from the timing marks.
func logSwitch(t *testing.T, w *World, what string) {
	var stored, seen time.Time
	via := "a new session"
	for _, m := range w.ReadMarks() {
		switch {
		case m.What == "switch stored: hold" && stored.IsZero():
			stored = m.At
		case m.What == "home sees the new client" && !stored.IsZero() && seen.IsZero():
			seen = m.At
		case m.What == "attach: standby":
			via = "its standby"
		case m.What == "attach" && via == "a new session":
			via = "a local attach"
		}
	}
	if !stored.IsZero() && !seen.IsZero() {
		t.Logf("%s: %v from the switch stored to the new client (through %s)", what, seen.Sub(stored).Round(time.Millisecond), via)
	}
}

// realClients are the sessions of h's non-control clients on the test
// server, sorted.
func realClients(h string) []string {
	out, _ := rssh(h, "tmux -L "+realSock+" list-clients -F '#{client_control_mode} #{session_name}'", nil)
	var s []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if rest, ok := strings.CutPrefix(l, "0 "); ok {
			s = append(s, rest)
		}
	}
	slices.Sort(s)
	return s
}

// TestR04 hands off back and forth between two real hosts over their
// masters, through standbys, then to the laptop: each host left keeps no
// client and the host the terminal is on has only its own. These hosts
// end a session with its ssh even without the home's detach; the leak
// seen in real use (decision 112) does not show here.
func TestR04(t *testing.T) {
	hs := realHosts(t)
	if len(hs) < 2 {
		t.Skip("R04 needs two hosts")
	}
	h1, h2 := hs[0], hs[1]
	w, a := realWorld(t, "r04")
	a.Set("TOWER_TEST_TIMING", w.Marks)
	bin := realBuild(t, w.Dir)
	realHost(t, w, h1, bin, false, "rt-"+h1)
	realHost(t, w, h2, bin, false, "rt-"+h2)
	w.Home(a, realEntry(h1, true), realEntry(h2, true))
	for _, h := range []string{h1, h2} {
		w.WaitLink(a, h, "up", 20*time.Second)
	}
	term := w.Loop("t", a, nil)
	term.Wait(Prompt, 10*time.Second)
	term.Pick("rt-" + h1 + " ")
	w.WaitLoop(a, "^"+regexp.QuoteMeta(h1)+":rt-"+regexp.QuoteMeta(h1)+"(:|$)", 15*time.Second)
	hops := []struct{ to, left string }{{h2, h1}, {h1, h2}, {h2, h1}, {"", h2}}
	for i, hop := range hops {
		if hop.to != "" {
			w.Eventually(20*time.Second, "a standby on "+hop.to+" ready", func() bool {
				return w.CountMarks("standby: ready "+hop.to) > 0
			})
		}
		time.Sleep(time.Second)
		w.ClearMarks()
		if hop.to == "" {
			term.DashTo("rt-local")
			w.WaitLoop(a, "^A:rt-local", 15*time.Second)
		} else {
			term.DashTo("rt-" + hop.to)
			w.WaitLoop(a, "^"+regexp.QuoteMeta(hop.to)+":rt-"+regexp.QuoteMeta(hop.to)+"(:|$)", 15*time.Second)
			w.Eventually(5*time.Second, hop.to+" has only the terminal's client", func() bool {
				return slices.Equal(realClients(hop.to), []string{"rt-" + hop.to})
			})
		}
		start := time.Now()
		w.Eventually(5*time.Second, hop.left+" keeps no client", func() bool { return len(realClients(hop.left)) == 0 })
		t.Logf("hand-off %d: %s clean %v after the loop moved (ssh included)", i+1, hop.left, time.Since(start).Round(time.Millisecond))
	}
	term.Keys("C-b", "d")
	term.Wait(`LOOP-EXIT=0`, 10*time.Second)
}

// TestR03 installs tower on connect over real ssh, into a test root: the
// home's build for linux/amd64 comes from a dist cache, goes to
// <root>/<version>/tower with current swapped, and the host streams; a
// second connect installs nothing.
func TestR03(t *testing.T) {
	hs := realHosts(t)
	w, a := realWorld(t, "r03")
	bin := realBuild(t, w.Dir)
	dist := filepath.Join(w.Dir, "dist")
	vdir := filepath.Join(dist, Version)
	name := "tower_" + Version + "_linux_amd64"
	os.MkdirAll(filepath.Join(vdir, "x"), 0o755)
	b, _ := os.ReadFile(bin)
	os.WriteFile(filepath.Join(vdir, "x", "tower"), b, 0o755)
	if out, err := exec.Command("tar", "-C", filepath.Join(vdir, "x"), "-czf", filepath.Join(vdir, name+".tar.gz"), "tower").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, out)
	}
	os.RemoveAll(filepath.Join(vdir, "x"))
	arch, _ := os.ReadFile(filepath.Join(vdir, name+".tar.gz"))
	sum := sha256.Sum256(arch)
	os.WriteFile(filepath.Join(vdir, "checksums.txt"), []byte(hex.EncodeToString(sum[:])+"  "+name+".tar.gz\n"), 0o644)
	a.Set("TOWER_DIST_DIR", dist)
	a.Set("TOWER_RELEASE_URL", "http://127.0.0.1:9") // never GitHub
	a.Set("TOWER_INSTALL_DIR", "~/"+realBase+"/install")

	var entries []config.Host
	for _, h := range hs {
		realHost(t, w, h, "", false, "rt-"+h)
		entries = append(entries, realEntry(h, false))
	}
	start := time.Now()
	w.Home(a, entries...)
	for _, h := range hs {
		w.WaitLink(a, h, "up", 60*time.Second)
		t.Logf("%s installed and up after %v", h, time.Since(start).Round(time.Millisecond))
		out := mustSSH(t, h, "ls -l ~/"+realBase+"/install/; ~/"+realBase+"/install/current/tower version")
		if !strings.Contains(out, "current -> "+Version) || !strings.Contains(out, Version+"\n") {
			t.Fatalf("%s's install root:\n%s", h, out)
		}
	}
	// A second connect finds the build there and installs nothing.
	stamp := map[string]string{}
	for _, h := range hs {
		stamp[h] = mustSSH(t, h, "stat -c %Y ~/"+realBase+"/install/"+Version+"/Linux-x86_64/tower")
	}
	before := map[string]int{}
	for _, h := range hs {
		before[h] = w.Link(a, h).Attempts
	}
	if err := a.Call(proto.CallWake, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		w.Eventually(30*time.Second, h+" reconnected", func() bool {
			l := w.Link(a, h)
			return l.Attempts > before[h] && l.Status == proto.StatusUp
		})
		if got := mustSSH(t, h, "stat -c %Y ~/"+realBase+"/install/"+Version+"/Linux-x86_64/tower"); got != stamp[h] {
			t.Fatalf("%s: the build was installed again on reconnect", h)
		}
	}
}
