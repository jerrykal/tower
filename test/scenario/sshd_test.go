package scenario

import (
	"bytes"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/transport"
)

// The sshd backend (TOWER_HOSTS=sshd): a host made with SSHHost is a tmux
// server of this machine, as with the fake ssh, reached over real ssh.
// Each of its ssh names has an sshd of its own on 127.0.0.1, run by the
// harness as this user (no account or sshd of the machine's), whose
// sessions start with the host's environment. Drop and Stall act on the
// name's sshd's processes, Down on the world's ssh config; the link's
// shape, Freeze, a network change and a host that times out are pf and
// dummynet rules on lo0 (macOS, through sudo -n). Elsewhere a scenario
// that needs one of those is skipped.

// sshds is the running backend; nil: not this one.
var sshds *sshdBackend

type sshdBackend struct {
	user            string
	hostKey         string // every sshd's
	key, other      string // the client key; one no sshd authorizes (Down auth)
	authorized      string
	known, knownBad string // the sshds' key under sshdAlias; another key there (Down hostkey)
	run             string // every session's command (sshd-run)
	pf              *pfNet // lo0's shapes and faults; nil: none here
}

// sshdAlias is every name's host key alias: one known_hosts line holds
// for every sshd's port.
const sshdAlias = "tt-sshd"

// sshdRun is every session's command (sshd's ForceCommand): the
// environment of the simulated host its ssh name stands for, then the
// command as sshd runs it.
const sshdRun = `#!/bin/sh
. "$1"
if [ -n "$SSH_ORIGINAL_COMMAND" ]; then
	exec /bin/sh -c "$SSH_ORIGINAL_COMMAND"
fi
exec /bin/sh -l
`

// sshd is one ssh name's sshd.
type sshd struct {
	cmd  *exec.Cmd
	done chan struct{} // closed once it has exited
	dir  string
	port int // its sessions'
	pw   int // the same sshd, key auth off (Down password)
	dead int // nothing listens; pf drops its SYNs (Down timeout)

	stallMu sync.Mutex
	stalled map[int]bool  // Stall: the session processes stopped
	stallCh chan struct{} // closes the sweep; nil: no stall
	stallWg sync.WaitGroup
}

// sshdSetup makes the keys every sshd and client of the run use, and on
// macOS takes pf for lo0's rules.
func sshdSetup() error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, "sshd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b := &sshdBackend{user: u.Username, hostKey: filepath.Join(dir, "hostkey"), key: filepath.Join(dir, "id"),
		other: filepath.Join(dir, "other"), authorized: filepath.Join(dir, "authorized_keys"),
		known: filepath.Join(dir, "known_hosts"), knownBad: filepath.Join(dir, "known_hosts_bad"),
		run: filepath.Join(dir, "sshd-run")}
	for _, k := range []string{b.hostKey, b.key, b.other} {
		os.Remove(k)
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", k).CombinedOutput(); err != nil {
			return fmt.Errorf("ssh-keygen: %v: %s", err, out)
		}
	}
	pubs := map[string]string{}
	for _, k := range []string{b.hostKey, b.key, b.other} {
		pub, err := os.ReadFile(k + ".pub")
		if err != nil {
			return err
		}
		f := strings.Fields(string(pub))
		pubs[k] = f[0] + " " + f[1]
	}
	for path, s := range map[string]string{
		b.authorized: pubs[b.key] + "\n",
		b.known:      sshdAlias + " " + pubs[b.hostKey] + "\n",
		b.knownBad:   sshdAlias + " " + pubs[b.other] + "\n",
	} {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(b.run, []byte(sshdRun), 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		if b.pf, err = pfStart(); err != nil {
			return err
		}
	}
	sshds = b
	return nil
}

// sshdTeardown gives pf back as it was.
func sshdTeardown() {
	if sshds != nil && sshds.pf != nil {
		sshds.pf.close()
	}
}

// sshdName starts alias's sshd, its sessions with target's environment,
// and writes the world's ssh config.
func (w *World) sshdName(alias string, target *Host) *sshd {
	w.T.Helper()
	d := &sshd{dir: filepath.Join(w.Dir, "sshd-"+alias)}
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		w.T.Fatal(err)
	}
	envFile := filepath.Join(d.dir, "env")
	if err := os.WriteFile(envFile, []byte(sshdEnv(target.EnvMap())), 0o600); err != nil {
		w.T.Fatal(err)
	}
	for try := 1; ; try++ {
		err := d.start(envFile)
		if err == nil {
			break
		}
		if try == 3 {
			w.T.Fatalf("%s's sshd: %v", alias, err)
		}
	}
	w.links[alias].sshd = d
	w.writeSSHConfig()
	if l, ok := w.sshdShapes[target.Machine]; ok {
		w.sshdShape(alias, target.Machine, l)
	}
	return d
}

// sshdEnv is env as the lines sshd-run sources: sshd's own variables (the
// terminal ssh asked for, the connection) left as sshd sets them.
func sshdEnv(env map[string]string) string {
	var b strings.Builder
	name := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	for _, k := range slices.Sorted(maps.Keys(env)) {
		if !name.MatchString(k) || k == "TERM" || strings.HasPrefix(k, "SSH_") || k == "_" || k == "PWD" || k == "OLDPWD" || k == "SHLVL" {
			continue
		}
		b.WriteString("export " + k + "=" + transport.ShellQuote(env[k]) + "\n")
	}
	return b.String()
}

// start runs the sshd on three free ports and waits until it listens.
func (d *sshd) start(envFile string) error {
	ports, err := freePorts(3)
	if err != nil {
		return err
	}
	d.port, d.pw, d.dead = ports[0], ports[1], ports[2]
	b := sshds
	conf := strings.Join([]string{
		"ListenAddress 127.0.0.1",
		"Port " + strconv.Itoa(d.port),
		"Port " + strconv.Itoa(d.pw),
		"HostKey " + b.hostKey,
		"PidFile " + filepath.Join(d.dir, "sshd.pid"),
		"AuthorizedKeysFile " + b.authorized,
		"AllowUsers " + b.user,
		"UsePAM no",
		"StrictModes no",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"PrintMotd no",
		"PrintLastLog no",
		"UseDNS no",
		"ForceCommand " + transport.ShellQuote(b.run) + " " + transport.ShellQuote(envFile),
		"Match LocalPort " + strconv.Itoa(d.pw),
		"  PubkeyAuthentication no",
		"  PasswordAuthentication yes",
	}, "\n") + "\n"
	path := filepath.Join(d.dir, "sshd_config")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		return err
	}
	logPath := filepath.Join(d.dir, "sshd.log")
	log, err := os.Create(logPath)
	if err != nil {
		return err
	}
	// sshd re-executes itself for each connection: by its full path.
	d.cmd = exec.Command("/usr/sbin/sshd", "-D", "-e", "-f", path)
	d.cmd.Stdout, d.cmd.Stderr = log, log
	if err := d.cmd.Start(); err != nil {
		log.Close()
		return err
	}
	d.done = make(chan struct{})
	go func() {
		d.cmd.Wait()
		log.Close()
		close(d.done)
	}()
	want := []string{fmt.Sprintf("listening on 127.0.0.1 port %d.", d.port), fmt.Sprintf("listening on 127.0.0.1 port %d.", d.pw)}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		out, _ := os.ReadFile(logPath)
		if strings.Contains(string(out), want[0]) && strings.Contains(string(out), want[1]) {
			return nil
		}
		select {
		case <-d.done:
			return fmt.Errorf("sshd exited: %s", out)
		default:
		}
		if time.Now().After(deadline) || bytes.Contains(out, []byte("Bind to port")) {
			d.stop()
			return fmt.Errorf("sshd not listening: %s", out)
		}
	}
}

// freePorts are n ports of 127.0.0.1 nothing listens on.
func freePorts(n int) ([]int, error) {
	var ports []int
	for range n {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		defer l.Close()
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}

// procs are the sshd's connections' processes (sshd's own) and what
// their sessions run: the rest under them, but for a daemon one started
// (underDaemon); a tmux server, which forks twice, has left them.
func (d *sshd) procs() (conns, sessions []int) {
	tree := procTree()
	by := make(map[int]pnode, len(tree))
	for _, p := range tree {
		by[p.pid] = p
	}
	root := d.cmd.Process.Pid
	for _, p := range descendants(tree, root) {
		if strings.HasPrefix(p.comm, "sshd") {
			conns = append(conns, p.pid)
		} else if !underDaemon(p, root, by) {
			sessions = append(sessions, p.pid)
		}
	}
	return conns, sessions
}

// underDaemon reports whether p is, or runs under, a daemon a session
// started below root: a process leading a group of its own whose parent
// is no sshd (a towerd, which whichever bridge finds none starts with
// setsid). It serves every name, as on a machine of its own, so no
// name's stall or drop is its.
func underDaemon(p pnode, root int, by map[int]pnode) bool {
	for q := p; q.pid != root && q.pid > 1; q = by[q.ppid] {
		if q.pgid == q.pid && !strings.HasPrefix(by[q.ppid].comm, "sshd") {
			return true
		}
	}
	return false
}

// drop ends the sshd's connections: what killing them does to ssh.
func (d *sshd) drop() {
	conns, _ := d.procs()
	for _, pid := range conns {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// setStall stops the sessions' processes, and new ones as they come,
// while sshd answers; or continues them.
func (d *sshd) setStall(on bool) {
	if on {
		if d.stallCh != nil {
			return
		}
		d.stalled = map[int]bool{}
		d.sweep()
		stop := make(chan struct{})
		d.stallCh = stop
		d.stallWg.Add(1)
		go func() {
			defer d.stallWg.Done()
			t := time.NewTicker(20 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					d.sweep()
				}
			}
		}()
		return
	}
	if d.stallCh == nil {
		return
	}
	close(d.stallCh)
	d.stallWg.Wait()
	d.stallCh = nil
	d.stallMu.Lock()
	for pid := range d.stalled {
		syscall.Kill(pid, syscall.SIGCONT)
	}
	d.stalled = nil
	d.stallMu.Unlock()
}

func (d *sshd) sweep() {
	_, sessions := d.procs()
	d.stallMu.Lock()
	defer d.stallMu.Unlock()
	for _, pid := range sessions {
		if !d.stalled[pid] && syscall.Kill(pid, syscall.SIGSTOP) == nil {
			d.stalled[pid] = true
		}
	}
}

// stop ends the sshd and its connections.
func (d *sshd) stop() {
	d.setStall(false)
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	conns, _ := d.procs()
	d.cmd.Process.Kill()
	for _, pid := range conns {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
	}
}

// sshdApply puts alias's state in place, prev being what is in place. The
// link's shape and a network change are the machine's (every port of
// its names), whichever of its names they are set through; Down, Freeze,
// Drop and Stall are the name's.
func (w *World) sshdApply(alias string, prev linkState, s *linkState) {
	w.T.Helper()
	switch {
	case s.WindowKB != 0:
		w.T.Fatalf("%s: WindowKB has no mechanism over real ssh (see docs/progress.md, Phase 2)", alias)
	case s.exit != nil:
		w.T.Fatalf("%s: ExitWith has no mechanism over real ssh (see docs/progress.md, Phase 2)", alias)
	case s.oDelayMs != 0:
		w.T.Fatalf("%s: SlowControl has no mechanism over real ssh (see docs/progress.md, Phase 2)", alias)
	}
	d, m := s.sshd, s.target.Machine
	if s.down != prev.down {
		if s.down == "timeout" || prev.down == "timeout" {
			rules := ""
			if s.down == "timeout" {
				w.needPF(alias, "a host that times out")
				rules = fmt.Sprintf("block drop quick on lo0 proto tcp from any to any port %d flags S/SA\n", d.dead)
			}
			w.pfSet("dead/"+alias, rules)
		}
		w.writeSSHConfig()
	}
	if s.Link != prev.Link && (s.DelayMs != prev.DelayMs || s.JitterMs != prev.JitterMs || s.BwKBps != prev.BwKBps) {
		if s.DelayMs != 0 || s.JitterMs != 0 || s.BwKBps != 0 {
			w.needPF(alias, "a shaped link")
		}
		if w.sshdShapes == nil {
			w.sshdShapes = map[string]Link{}
		}
		w.sshdShapes[m] = s.Link
		w.sshdShape(alias, m, s.Link)
	}
	if s.halfOpenAt != prev.halfOpenAt && s.halfOpenAt != 0 {
		if d := time.Until(time.UnixMilli(s.halfOpenAt)); d > 100*time.Millisecond {
			w.T.Fatalf("a network change %v ahead has no mechanism over real ssh", d)
		}
		w.needPF(alias, "a network change")
		w.sshdHalfOpen(m)
	}
	if s.freeze != prev.freeze {
		rules := ""
		if s.freeze {
			w.needPF(alias, "a frozen link")
			rules = fmt.Sprintf("block drop quick on lo0 proto tcp from any to any port %d\nblock drop quick on lo0 proto tcp from any port %d to any\n", d.port, d.port)
		}
		w.pfSet("freeze/"+alias, rules)
	}
	if s.drops > prev.drops {
		d.drop()
	}
	if s.stall != prev.stall {
		d.setStall(s.stall)
	}
}

// lo0 reports whether links on this backend can be shaped and frozen:
// not on the sshd backend off macOS.
func lo0() bool { return sshds == nil || sshds.pf != nil }

// needLo0 skips t where lo0 has no rules.
func needLo0(t *testing.T) {
	t.Helper()
	if !lo0() {
		t.Skip("needs pf and dummynet on lo0 (macOS) on the sshd backend")
	}
}

// needPF skips the scenario where lo0 has no rules (not macOS).
func (w *World) needPF(alias, what string) {
	w.T.Helper()
	if sshds.pf == nil {
		w.T.Skipf("%s: %s needs pf and dummynet on lo0 (macOS) on the sshd backend", alias, what)
	}
}

// pfSet sets the world's rules under name.
func (w *World) pfSet(name, rules string) {
	w.T.Helper()
	if sshds.pf == nil {
		return
	}
	if err := sshds.pf.set(w.ID+"/"+name, rules); err != nil {
		w.T.Fatal(err)
	}
}

// machinePorts are the ports of every sshd of machine m's names.
func (w *World) machinePorts(m string) []int {
	var ports []int
	for _, a := range slices.Sorted(maps.Keys(w.links)) {
		if s := w.links[a]; s.sshd != nil && s.target.Machine == m {
			ports = append(ports, s.sshd.port, s.sshd.pw)
		}
	}
	return ports
}

// sshdShape shapes machine m's traffic as l: each way through a dummynet
// pipe of l's delay and bandwidth. dummynet has no jitter: with one, each
// packet takes one of five pipes, of delays across ±jitter, at random.
func (w *World) sshdShape(alias, m string, l Link) {
	w.T.Helper()
	if sshds.pf == nil {
		return
	}
	var err error
	if l.DelayMs == 0 && l.JitterMs == 0 && l.BwKBps == 0 {
		err = sshds.pf.shape(w.ID+"/shape/"+m, nil, 0, nil, 0)
	} else {
		delays := []int{l.DelayMs}
		if j := l.JitterMs; j > 0 {
			delays = []int{l.DelayMs - j, l.DelayMs - j/2, l.DelayMs, l.DelayMs + j/2, l.DelayMs + j}
		}
		for i := range delays {
			delays[i] = max(delays[i], 0)
		}
		bw := 0
		if l.BwKBps > 0 {
			bw = max(l.BwKBps/len(delays), 1)
		}
		err = sshds.pf.shape(w.ID+"/shape/"+m, w.machinePorts(m), len(delays), delays, bw)
	}
	if err != nil {
		w.T.Fatalf("%s: shape: %v", alias, err)
	}
}

// sshdHalfOpen makes every connection to machine m's ports half-open for
// good, both ways: what a network change does to them.
func (w *World) sshdHalfOpen(m string) {
	w.T.Helper()
	ports := w.machinePorts(m)
	out, err := exec.Command("netstat", "-an", "-p", "tcp").Output()
	if err != nil {
		w.T.Fatalf("netstat: %v", err)
	}
	var b strings.Builder
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 6 || f[5] != "ESTABLISHED" || !strings.HasPrefix(f[3], "127.0.0.1.") || !strings.HasPrefix(f[4], "127.0.0.1.") {
			continue
		}
		local, _ := strconv.Atoi(strings.TrimPrefix(f[3], "127.0.0.1."))
		peer, _ := strconv.Atoi(strings.TrimPrefix(f[4], "127.0.0.1."))
		if slices.Contains(ports, local) {
			fmt.Fprintf(&b, "block drop quick on lo0 proto tcp from any port %d to any port %d\n", peer, local)
			fmt.Fprintf(&b, "block drop quick on lo0 proto tcp from any port %d to any port %d\n", local, peer)
		}
	}
	w.halfOpen += b.String()
	w.pfSet("halfopen", w.halfOpen)
}

// sshdRelease ends the world's sshds and takes its rules off lo0.
func (w *World) sshdRelease() {
	for _, s := range w.links {
		if s.sshd != nil {
			s.sshd.stop()
		}
	}
	if sshds != nil && sshds.pf != nil {
		if err := sshds.pf.clear(w.ID + "/"); err != nil {
			w.T.Errorf("pf: %v", err)
		}
	}
}

// sshdFar are the processes under h's names' sshds.
func (h *Host) sshdFar() map[int]bool {
	in := map[int]bool{}
	for _, s := range h.w.links {
		if s.sshd != nil && s.target == h {
			conns, sessions := s.sshd.procs()
			for _, p := range append(conns, sessions...) {
				in[p] = true
			}
		}
	}
	return in
}

// pfNet is the run's rules on lo0: one anchor under com.apple (which
// macOS's main ruleset evaluates), and dummynet pipes, through sudo -n.
// Each world sets its own under names of its own; every change loads the
// whole anchor.
type pfNet struct {
	mu     sync.Mutex
	anchor string
	token  string // pf's enable reference, released at the end
	next   int    // the next pipe number
	rules  map[string]string
	pipes  map[string][]int
}

// pfStart enables pf (a reference of the run's) and checks dummynet
// delays lo0's traffic.
func pfStart() (*pfNet, error) {
	if out, err := exec.Command("sudo", "-n", "true").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("the sshd backend's links and faults need pfctl and dnctl through sudo -n: %v: %s", err, out)
	}
	p := &pfNet{anchor: "com.apple/tt-" + strconv.Itoa(os.Getpid()), next: 7001, rules: map[string]string{}, pipes: map[string][]int{}}
	p.sweep()
	out, err := exec.Command("sudo", "-n", "pfctl", "-E").CombinedOutput()
	if m := regexp.MustCompile(`Token : (\d+)`).FindSubmatch(out); err == nil && m != nil {
		p.token = string(m[1])
	} else {
		return nil, fmt.Errorf("pfctl -E: %v: %s", err, out)
	}
	if err := p.check(); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

// sweep flushes the anchors of earlier runs that ended without their
// teardown.
func (p *pfNet) sweep() {
	out, _ := exec.Command("sudo", "-n", "pfctl", "-a", "com.apple", "-s", "Anchors").Output()
	for _, a := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(strings.TrimPrefix(a, "com.apple/tt-"))
		if err == nil && strings.HasPrefix(a, "com.apple/tt-") && !alive(pid) {
			exec.Command("sudo", "-n", "pfctl", "-a", a, "-F", "all").Run()
		}
	}
}

// check shapes a port of its own 50ms each way and measures a round trip.
func (p *pfNet) check() error {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 1)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
			c.Write(buf)
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	if err := p.shape("check", []int{port}, 1, []int{50}, 0); err != nil {
		return err
	}
	defer p.shape("check", nil, 0, nil, 0)
	c, err := net.DialTimeout("tcp4", l.Addr().String(), 5*time.Second)
	if err != nil {
		return fmt.Errorf("dummynet check: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	var best time.Duration
	for range 3 {
		start := time.Now()
		if _, err := c.Write(buf); err != nil {
			return fmt.Errorf("dummynet check: %v", err)
		}
		if _, err := c.Read(buf); err != nil {
			return fmt.Errorf("dummynet check: %v", err)
		}
		if d := time.Since(start); best == 0 || d < best {
			best = d
		}
	}
	if best < 90*time.Millisecond || best > 200*time.Millisecond {
		return fmt.Errorf("dummynet check: a round trip at 50ms each way took %v", best)
	}
	return nil
}

// shape puts name's traffic to and from ports through n pipes each way,
// of delays (ms) and bandwidth bw (KB/s, 0 unlimited) each; no ports
// takes name's pipes away.
func (p *pfNet) shape(name string, ports []int, n int, delays []int, bw int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(ports) == 0 {
		p.deletePipes(name)
		delete(p.rules, "0 "+name)
		return p.load()
	}
	if len(p.pipes[name]) != 2*n {
		p.deletePipes(name)
		for range 2 * n {
			p.pipes[name] = append(p.pipes[name], p.next)
			p.next++
		}
	}
	pipes := p.pipes[name]
	for i, pipe := range pipes {
		cfg := []string{"pipe", strconv.Itoa(pipe), "config", "delay", strconv.Itoa(delays[i%n]), "queue", "100"}
		if bw > 0 {
			cfg = append(cfg, "bw", strconv.Itoa(bw)+"KByte/s")
		}
		if out, err := exec.Command("sudo", append([]string{"-n", "dnctl"}, cfg...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("dnctl %s: %v: %s", strings.Join(cfg, " "), err, out)
		}
	}
	list := "{ " + strings.Trim(strings.Join(strings.Fields(fmt.Sprint(ports)), ", "), "[]") + " }"
	var b strings.Builder
	for way, match := range []string{"from any to any port " + list, "from any port " + list + " to any"} {
		for i := range n {
			prob := ""
			if i < n-1 {
				prob = fmt.Sprintf(" probability %d%%", 100/(n-i))
			}
			fmt.Fprintf(&b, "dummynet in quick on lo0 proto tcp %s%s pipe %d\n", match, prob, pipes[way*n+i])
		}
	}
	// Dummynet rules load before filter rules: "0 " sorts first.
	p.rules["0 "+name] = b.String()
	return p.load()
}

func (p *pfNet) deletePipes(name string) {
	for _, pipe := range p.pipes[name] {
		exec.Command("sudo", "-n", "dnctl", "pipe", "delete", strconv.Itoa(pipe)).Run()
	}
	delete(p.pipes, name)
}

// set sets name's filter rules; empty takes them away.
func (p *pfNet) set(name, rules string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rules == "" {
		delete(p.rules, "1 "+name)
	} else {
		p.rules["1 "+name] = rules
	}
	return p.load()
}

// clear takes away every rule and pipe of names under prefix.
func (p *pfNet) clear(prefix string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.rules {
		if strings.HasPrefix(k[2:], prefix) {
			delete(p.rules, k)
		}
	}
	for name := range p.pipes {
		if strings.HasPrefix(name, prefix) {
			p.deletePipes(name)
		}
	}
	return p.load()
}

func (p *pfNet) load() error {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(p.rules)) {
		b.WriteString(p.rules[k])
	}
	cmd := exec.Command("sudo", "-n", "pfctl", "-a", p.anchor, "-f", "-")
	cmd.Stdin = strings.NewReader(b.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pfctl -a %s -f: %v: %s\n%s", p.anchor, err, out, b.String())
	}
	return nil
}

// close takes the run's rules and pipes away and releases pf.
func (p *pfNet) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	exec.Command("sudo", "-n", "pfctl", "-a", p.anchor, "-F", "all").Run()
	for name := range p.pipes {
		p.deletePipes(name)
	}
	if p.token != "" {
		exec.Command("sudo", "-n", "pfctl", "-X", p.token).Run()
	}
}
