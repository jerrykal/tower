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
		killSessions(localTmux(term.Sock))
	}
	time.Sleep(300 * time.Millisecond)
	// The sshds first: their connections' processes are found under them,
	// and a home reconnecting finds no host.
	w.sshdRelease()

	var pids []int
	towerds, _ := filepath.Glob(filepath.Join(w.Dir, "home-*", "state", "towerd", "*", "towerd.pid"))
	for _, p := range towerds {
		if b, err := os.ReadFile(p); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
				// A container's pid file holds its own pid: this
				// machine's, or none.
				if h := w.homeOf(p); h != nil {
					pid = h.HostPid(pid)
				}
				if pid > 1 {
					pids = append(pids, pid)
				}
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
		killSessions(w.tmuxAt(sock))
	}
	for _, sock := range w.sockets {
		if !waitNoServer(w.tmuxAt(sock), 10*time.Second) {
			wedged = append(wedged, sock)
			exec.Command("pkill", "-9", "-f", "tmux -L "+sock+" ").Run()
			exec.Command("pkill", "-9", "-f", "tmux -L "+sock+"$").Run()
		}
	}
	exec.Command("pkill", "-f", w.Dir+"/").Run()
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

	// Each container: none of its test user's processes, its link and
	// faults reset, free for the next world.
	for _, s := range w.ctrSlots() {
		w.release(s)
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

// homeOf is the container host whose TOWER_HOME holds path, or nil.
func (w *World) homeOf(path string) *Host {
	for _, h := range w.hosts {
		if h.ctr != nil && strings.HasPrefix(path, h.HomeDir+"/") {
			return h
		}
	}
	return nil
}

// tmuxRun runs a tmux command on one test server.
type tmuxRun func(args ...string) (string, error)

// localTmux runs this machine's tmux on the server at sock.
func localTmux(sock string) tmuxRun {
	return func(args ...string) (string, error) {
		out, err := exec.Command(tmux.Bin(), append([]string{"-L", sock}, args...)...).Output()
		return string(out), err
	}
}

// tmuxAt runs tmux on the world's server at sock: a container host's
// with the container's tmux (Host.Tmux), another with this machine's.
func (w *World) tmuxAt(sock string) tmuxRun {
	for _, h := range w.hosts {
		if h.Sock == sock && h.ctr != nil {
			return h.Tmux
		}
	}
	return localTmux(sock)
}

// killSessions ends every session of a test server, which ends the server
// (exit-empty). A server is never killed outright: the user's tooling
// forbids kill-server, and a server that does not exit is a finding.
func killSessions(tm tmuxRun) {
	out, err := tm("list-sessions", "-F", "#{session_id}")
	if err != nil {
		return
	}
	for _, id := range strings.Fields(out) {
		tm("kill-session", "-t", id)
	}
}

func waitNoServer(tm tmuxRun, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := tm("list-sessions"); err != nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
