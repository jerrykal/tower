package scenario

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/tmux"
)

// teardown ends everything the world started and checks nothing outlived
// it: terminals first (a live loop restarts a stopped towerd), then every
// towerd, the half-open holders, the hosts' tmux servers, and orphans.
func (w *World) teardown() {
	t := w.T
	for _, term := range w.terms {
		killSessions(term.Sock)
	}
	time.Sleep(300 * time.Millisecond)

	var pids []int
	towerds, _ := filepath.Glob(filepath.Join(w.Dir, "home-*", "state", "towerd", "*", "towerd.pid"))
	for _, p := range towerds {
		if b, err := os.ReadFile(p); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
				pids = append(pids, pid)
			}
		}
	}
	for _, c := range w.spawned {
		if c.Process != nil {
			pids = append(pids, c.Process.Pid)
		}
	}
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGCONT)
		syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range pids {
		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if alive(pid) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}

	if b, err := os.ReadFile(filepath.Join(w.Fake, "holders")); err == nil {
		for _, f := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}

	var wedged []string
	for _, sock := range w.sockets {
		killSessions(sock)
	}
	for _, sock := range w.sockets {
		if !waitNoServer(sock, 10*time.Second) {
			wedged = append(wedged, sock)
			exec.Command("pkill", "-9", "-f", "tmux -L "+sock+" ").Run()
			exec.Command("pkill", "-9", "-f", "tmux -L "+sock+"$").Run()
		}
	}
	exec.Command("pkill", "-f", w.Dir).Run()
	// Anything still running with this world's TOWER_HOME: a towerd a
	// reconnect started during teardown, a bridge, a standby shim.
	for _, pid := range pidsWithEnvPrefix("TOWER_HOME", w.Dir+"/") {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	// Real ssh's control masters: their process title overwrites their
	// environment, so they are found by their control path.
	if w.real {
		for _, h := range w.hosts {
			cm := "ssh: " + h.Paths().CMDir() + "/"
			for _, pid := range procsWhere(func(p proc) bool { return len(p.argv) > 0 && strings.HasPrefix(p.argv[0], cm) }) {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}

	if len(wedged) > 0 {
		t.Errorf("teardown: tmux servers outlived their sessions: %v", wedged)
	}
	if reports, _ := filepath.Glob(filepath.Join(w.Dir, "race", "report.*")); len(reports) > 0 {
		for _, r := range reports {
			b, _ := os.ReadFile(r)
			t.Errorf("race detected (%s):\n%s", filepath.Base(r), b)
		}
	}
	if !t.Failed() {
		os.RemoveAll(w.Dir)
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// killSessions ends every session of a test server, which ends the server
// (exit-empty). A server is never killed outright: the user's tooling
// forbids kill-server, and a server that does not exit is a finding.
func killSessions(sock string) {
	out, err := exec.Command(tmux.Bin(), "-L", sock, "list-sessions", "-F", "#{session_id}").Output()
	if err != nil {
		return
	}
	for _, id := range strings.Fields(string(out)) {
		exec.Command(tmux.Bin(), "-L", sock, "kill-session", "-t", id).Run()
	}
}

func waitNoServer(sock string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if exec.Command(tmux.Bin(), "-L", sock, "list-sessions").Run() != nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
