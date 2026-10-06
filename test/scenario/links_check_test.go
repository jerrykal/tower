package scenario

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/transport"
)

// sshCall is one ssh from a host to an ssh name with tower's options.
type sshCall struct {
	cmd            *exec.Cmd
	stdout, stderr lockedBuffer
	start          time.Time
	done           chan struct{}
	code           int
	took           time.Duration
}

// startSSH runs remote on alias from host from, as tower would (its ssh,
// its options, its control socket directory), without waiting.
func (w *World) startSSH(from *Host, alias, remote string, extra ...string) *sshCall {
	w.T.Helper()
	env := from.EnvMap()
	s := &transport.SSH{Bin: env["TOWER_SSH"], CMDir: from.Paths().CMDir()}
	os.MkdirAll(s.CMDir, 0o700)
	args := append(append([]string{"-T"}, extra...), s.Options(config.Host{Name: alias, SSH: alias})...)
	args = append(args, alias, "--", transport.Sh(remote))
	c := &sshCall{cmd: exec.Command(s.Bin, args...), done: make(chan struct{})}
	for k, v := range env {
		c.cmd.Env = append(c.cmd.Env, k+"="+v)
	}
	c.cmd.Stdout, c.cmd.Stderr = &c.stdout, &c.stderr
	c.cmd.WaitDelay = time.Second
	c.start = time.Now()
	if err := c.cmd.Start(); err != nil {
		w.T.Fatal(err)
	}
	go func() {
		c.cmd.Wait()
		c.took = time.Since(c.start)
		c.code = c.cmd.ProcessState.ExitCode()
		close(c.done)
	}()
	w.T.Cleanup(func() {
		if c.cmd.Process != nil {
			c.cmd.Process.Kill()
		}
	})
	return c
}

// wait waits up to d for the call to end; false if it has not.
func (c *sshCall) wait(d time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(d):
		return false
	}
}

// ssh runs remote on alias and waits for it (up to 30s).
func (w *World) ssh(from *Host, alias, remote string) *sshCall {
	w.T.Helper()
	c := w.startSSH(from, alias, remote)
	if !c.wait(30 * time.Second) {
		w.T.Fatalf("ssh %s %q: still running after 30s", alias, remote)
	}
	return c
}

// reason is how tower reads the call's failure.
func (c *sshCall) reason(alias string) string {
	return transport.Classify(alias, c.code, c.stderr.String()).Reason
}

// TestHarnessLinks: each link setting and fault does what the World says
// it does, through the fake ssh or over real ssh alike (run it with
// TOWER_HOSTS=container too): a delay costs round trips, a bandwidth
// limits a transfer, each Down fails as tower expects, Drop ends a
// session, Freeze leaves it to give up after its alive window with the
// far side still running, a network change leaves the old master dead
// and a new one working, a stall stops the far side with ssh still up
// (that name's sessions only).
func TestHarnessLinks(t *testing.T) {
	w := NewWorld(t, "links")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Shape("B", ptyLink)

	if c := w.ssh(a, "B", "echo $TOWER_MACHINE_ID"); c.code != 0 || c.stdout.String() != "B\n" {
		t.Fatalf("ssh to B: exit %d, %q %q", c.code, c.stdout.String(), c.stderr.String())
	}

	t.Run("delay", func(t *testing.T) {
		w.Shape("B", func(l *Link) { l.DelayMs = 50 })
		defer w.Shape("B", func(l *Link) { l.DelayMs = 0 })
		w.ssh(a, "B", "true") // a master at this delay
		var best time.Duration
		for range 3 {
			c := w.ssh(a, "B", "true")
			if best == 0 || c.took < best {
				best = c.took
			}
		}
		t.Logf("a session on the master at RTT 100ms: %v", best.Round(time.Millisecond))
		if best < 100*time.Millisecond || best > 400*time.Millisecond {
			t.Fatalf("a session on the master took %v at RTT 100ms", best)
		}
	})

	t.Run("bandwidth", func(t *testing.T) {
		w.Shape("B", func(l *Link) { l.BwKBps = 256 })
		defer w.Shape("B", func(l *Link) { l.BwKBps = 0 })
		w.ssh(a, "B", "true")
		c := w.ssh(a, "B", "head -c 524288 /dev/zero")
		t.Logf("512 KiB at 256 KB/s: %v", c.took.Round(time.Millisecond))
		if c.stdout.Len() != 524288 || c.took < 1500*time.Millisecond || c.took > 4*time.Second {
			t.Fatalf("512 KiB at 256 KB/s: %d bytes in %v", c.stdout.Len(), c.took)
		}
	})

	t.Run("down", func(t *testing.T) {
		want := map[string]string{
			"refused": "connection refused", "hostkey": "host key", "auth": "authentication failed",
			"password": "authentication failed", "resolve": "cannot resolve", "timeout": "timed out",
		}
		for how, reason := range want {
			alias := "B-" + how
			w.SSH(alias, b)
			w.Down(alias, how)
			c := w.ssh(a, alias, "true")
			t.Logf("%s: exit %d in %v, %q", how, c.code, c.took.Round(time.Millisecond), strings.TrimSpace(c.stderr.String()))
			if c.code != 255 || !strings.Contains(c.reason(alias), reason) {
				t.Errorf("%s: exit %d, read as %q, want %q", how, c.code, c.reason(alias), reason)
			}
			w.Heal(alias)
			if c := w.ssh(a, alias, "true"); c.code != 0 {
				t.Errorf("%s healed: exit %d, %q", how, c.code, c.stderr.String())
			}
		}
	})

	// Each of drop and freeze runs a session on the master and one off
	// it: over real ssh, a session on a master that dies hears nothing
	// (its ssh exits 255, stderr empty), so tower can read why only off
	// the master.
	offMaster := []string{"-o", "ControlPath=none"}

	t.Run("drop", func(t *testing.T) {
		w.ssh(a, "B", "true")
		on := w.startSSH(a, "B", "sleep 30")
		off := w.startSSH(a, "B", "sleep 30", offMaster...)
		time.Sleep(time.Second)
		w.Drop("B")
		for _, c := range []*sshCall{on, off} {
			if !c.wait(3 * time.Second) {
				t.Fatal("a session outlived its drop by 3s")
			}
		}
		t.Logf("dropped: on the master exit %d, read as %q; off it exit %d, read as %q", on.code, on.reason("B"), off.code, off.reason("B"))
		if on.code != 255 || off.code != 255 || off.reason("B") != "connection lost" {
			t.Fatalf("dropped: exit %d and %d, %q", on.code, off.code, off.stderr.String())
		}
	})

	t.Run("freeze", func(t *testing.T) {
		// The fake's sessions on a master ride out a freeze (a network
		// change is what ends them); over real ssh every connection does.
		w.Shape("B", func(l *Link) { l.Mux = false })
		defer w.Shape("B", ptyLink)
		defer w.Freeze("B", false)
		pidFile := filepath.Join(w.Dir, "frozen.pid")
		w.ssh(a, "B", "true")
		on := w.startSSH(a, "B", "sleep 60")
		off := w.startSSH(a, "B", "echo $$ > "+pidFile+"; exec sleep 60", offMaster...)
		pid := b.HostPid(waitPidFile(w, pidFile))
		if pid <= 0 {
			t.Fatal("the far side's pid is not running")
		}
		t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
		time.Sleep(500 * time.Millisecond)
		w.Freeze("B", true)
		start := time.Now()
		for _, c := range []*sshCall{on, off} {
			if !c.wait(25 * time.Second) {
				t.Fatal("a frozen session never gave up")
			}
		}
		took := time.Since(start)
		t.Logf("frozen: gave up after %v; on the master exit %d, read as %q; off it exit %d, read as %q",
			took.Round(100*time.Millisecond), on.code, on.reason("B"), off.code, off.reason("B"))
		if took < 9*time.Second || on.code != 255 || off.code != 255 || off.reason("B") != "connection timed out" {
			t.Fatalf("frozen: gave up after %v: exit %d and %d, %q", took, on.code, off.code, off.stderr.String())
		}
		if !Alive(pid) {
			t.Fatal("the far side of a frozen session ended")
		}
		w.Freeze("B", false)
		if c := w.ssh(a, "B", "true"); c.code != 0 {
			t.Fatalf("thawed: exit %d, %q", c.code, c.stderr.String())
		}
	})

	t.Run("network change", func(t *testing.T) {
		w.ssh(a, "B", "true")
		c := w.startSSH(a, "B", "sleep 60")
		time.Sleep(time.Second)
		w.NetworkChange("B", time.Now())
		start := time.Now()
		if !c.wait(25 * time.Second) {
			t.Fatal("a session on the old master never gave up")
		}
		t.Logf("network change: the old master's session gave up after %v", time.Since(start).Round(100*time.Millisecond))
		if c := w.ssh(a, "B", "true"); c.code != 0 {
			t.Fatalf("a new master: exit %d, %q", c.code, c.stderr.String())
		}
	})

	t.Run("stall", func(t *testing.T) {
		// Another name for B, its sessions not stalled.
		w.SSH("B2", b)
		w.Shape("B2", ptyLink)
		loop := "while :; do echo x; sleep 0.1; done"
		c, c2 := w.startSSH(a, "B", loop), w.startSSH(a, "B2", loop)
		lines := func(c *sshCall) int { return strings.Count(c.stdout.String(), "\n") }
		w.Eventually(3*time.Second, "output", func() bool { return lines(c) > 3 && lines(c2) > 3 })
		w.Stall("B", true)
		defer w.Stall("B", false)
		time.Sleep(500 * time.Millisecond)
		n, n2 := lines(c), lines(c2)
		alive := 17 * time.Second // ServerAliveInterval × ServerAliveCountMax, and 2s
		if c.wait(alive) {
			t.Fatalf("a stalled session ended: exit %d, %q", c.code, c.stderr.String())
		}
		if m := lines(c); m != n {
			t.Fatalf("%d lines came through a stall", m-n)
		}
		if m := lines(c2); m-n2 < 100 {
			t.Fatalf("%d lines in %v through another name for the stalled host", m-n2, alive)
		}
		w.Stall("B", false)
		w.Eventually(3*time.Second, "output after the stall", func() bool { return lines(c) > n })
	})
}

// waitPidFile waits for a pid file a remote command writes.
func waitPidFile(w *World, p string) int {
	w.T.Helper()
	var pid int
	w.Eventually(5*time.Second, "pid file "+p, func() bool {
		b, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 0
	})
	return pid
}

// lockedBuffer is a buffer a process writes while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuffer) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Len()
}
