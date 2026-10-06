package scenario

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/transport"
)

// The container backend (TOWER_HOSTS=container): a host made with
// SSHHost is a container running sshd and tmux (hosts/), reached over
// real ssh, its link shaped with tc netem. The run's directories are
// bind-mounted at their own paths and the containers share the
// machine's pids, so a host's files, tmux socket and processes are where
// the harness looks for them; only ssh crosses the network.

// ctrs is the running backend; nil: every host is reached through the
// fake ssh.
var ctrs *ctrBackend

// ctrPath is a container's PATH.
const ctrPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

type ctrBackend struct {
	run      string   // compose project suffix: this process's pid
	compose  []string // docker compose and its file
	env      []string // compose's variables
	key      string   // the client key
	other    string   // a key no host authorizes (Down auth)
	known    string   // known_hosts of every slot
	knownBad string   // every slot with another key (Down hostkey)
	slots    []*ctrSlot
}

// ctrSlot is one host container.
type ctrSlot struct {
	name string // h1, h2: the compose service
	ctr  string // container name
	id   string // container id, in its processes' cgroup
	ip   string
	env  string // the file tt-run sources: the current world's host
	w    *World // the world using it

	stall     chan struct{} // closed to end a stall
	stallDone chan struct{} // closed once the stall's processes continue
}

// SSHHost makes a host a remote reached over ssh: on the container
// backend, a container.
func SSHHost() HostOpt {
	return func(h *Host) {
		if ctrs != nil {
			h.ctr = &ctrSlot{} // the world assigns one
		}
	}
}

// containerSetup brings the hosts up: a key, the compose project, each
// host's address and host key.
func containerSetup() error {
	b := &ctrBackend{run: strconv.Itoa(os.Getpid())}
	dir := filepath.Join(root, "ssh")
	if err := os.MkdirAll(filepath.Join(root, "slots"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b.key = filepath.Join(dir, "id")
	b.other = filepath.Join(dir, "other")
	b.known = filepath.Join(dir, "known_hosts")
	b.knownBad = filepath.Join(dir, "known_hosts_bad")
	for _, k := range []string{b.key, b.other} {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", k).CombinedOutput(); err != nil {
			return fmt.Errorf("ssh-keygen: %v: %s", err, out)
		}
	}
	pub, err := os.ReadFile(b.key + ".pub")
	if err != nil {
		return err
	}
	b.compose = []string{"compose", "-f", filepath.Join("hosts", "compose.yaml")}
	b.env = append(os.Environ(), "TT_RUN="+b.run,
		"TT_PUBKEY="+strings.TrimSpace(string(pub)), "TOWER_TEST_DIR="+root)
	ctrs = b
	tmuxVersion := os.Getenv("TT_TMUX_VERSION")
	if tmuxVersion == "" {
		tmuxVersion = "3.7c"
	}
	if out, err := b.docker("build", "-q", "--label", "tower-test", "-t", "tt-scenario-host:latest",
		"--build-arg", "UID="+strconv.Itoa(os.Getuid()), "--build-arg", "TMUX_VERSION="+tmuxVersion, "hosts"); err != nil {
		return fmt.Errorf("build the host image: %v: %s", err, out)
	}
	if out, err := b.docker(append(b.compose, "up", "-d")...); err != nil {
		return fmt.Errorf("compose up: %v: %s", err, out)
	}
	for _, n := range []string{"h1", "h2"} {
		s := &ctrSlot{name: n, ctr: "tt-" + b.run + "-" + n, env: filepath.Join(root, "slots", n+".env")}
		out, err := b.docker("inspect", "-f", "{{.Id}} {{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", s.ctr)
		if err != nil {
			return fmt.Errorf("inspect %s: %v: %s", s.ctr, err, out)
		}
		s.id, s.ip, _ = strings.Cut(strings.TrimSpace(out), " ")
		b.slots = append(b.slots, s)
	}
	// Each host's key, once sshd answers, under each of its ports.
	var known []string
	for _, s := range b.slots {
		deadline := time.Now().Add(20 * time.Second)
		for {
			out, err := exec.Command("ssh-keyscan", "-T", "1", "-t", "ed25519", s.ip).Output()
			if f := strings.Fields(string(out)); err == nil && len(f) == 3 {
				for _, p := range append(ctrPorts, 2223) {
					known = append(known, hostPort(s.ip, p)+" "+f[1]+" "+f[2])
				}
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s: sshd never answered", s.ctr)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if err := os.WriteFile(b.known, []byte(strings.Join(known, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	// The same names with the other key: a host whose key changed.
	pub, err = os.ReadFile(b.other + ".pub")
	if err != nil {
		return err
	}
	f := strings.Fields(string(pub))
	var bad []string
	for _, s := range b.slots {
		for _, p := range ctrPorts {
			bad = append(bad, hostPort(s.ip, p)+" "+f[0]+" "+f[1])
		}
	}
	return os.WriteFile(b.knownBad, []byte(strings.Join(bad, "\n")+"\n"), 0o600)
}

// hostPort is how known_hosts names a host at a port.
func hostPort(ip string, port int) string {
	if port == 22 {
		return ip
	}
	return fmt.Sprintf("[%s]:%d", ip, port)
}

// containerTeardown removes the compose project's containers and
// network (both labelled tower-test); the image stays, as a cache.
func containerTeardown() {
	if ctrs == nil {
		return
	}
	if out, err := ctrs.docker(append(ctrs.compose, "down", "--timeout", "2")...); err != nil {
		fmt.Fprintf(os.Stderr, "compose down: %v: %s\n", err, out)
	}
}

func (b *ctrBackend) docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	cmd.Env = b.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// assign gives h a free slot of the world: unshaped, no faults.
func (w *World) assign(h *Host) {
	w.T.Helper()
	for _, s := range ctrs.slots {
		if s.w == nil {
			s.w = w
			h.ctr = s
			w.real = true
			w.rootExec(s, "tc qdisc del dev eth0 root 2>/dev/null; tc qdisc del dev ifb0 root 2>/dev/null; "+
				"iptables -F TT-FREEZE && iptables -F TT-HALFOPEN")
			w.T.Cleanup(func() {
				w.stallOff(s)
				s.w = nil
			})
			return
		}
	}
	w.T.Fatalf("no free host container for %s (%d)", h.Name, len(ctrs.slots))
}

// rootExec runs a shell script as root in the slot's container.
func (w *World) rootExec(s *ctrSlot, script string) {
	w.T.Helper()
	if out, err := ctrs.docker("exec", "-u", "root", s.ctr, "sh", "-c", script); err != nil {
		w.T.Fatalf("on %s: %s: %v: %s", s.ctr, script, err, out)
	}
}

// ctrPorts are a container's sshd ports: one per ssh name, so that each
// name has a control master of its own (ssh keys one by host, port and
// user).
var ctrPorts = []int{22, 2201, 2202, 2203, 2204, 2205, 2206, 2207, 2208, 2209, 2210, 2211, 2212, 2213, 2214, 2215}

// ctrName gives alias a port of target's container, and writes target's
// environment for tt-run: what every ssh session there starts with.
func (w *World) ctrName(alias string, target *Host) {
	w.T.Helper()
	used := 0
	for _, s := range w.links {
		if s.target.ctr == target.ctr && s.port != 0 {
			used++
		}
	}
	if used == len(ctrPorts) {
		w.T.Fatalf("%s: more than %d ssh names for one container", alias, len(ctrPorts))
	}
	w.links[alias].port = ctrPorts[used]
	var b strings.Builder
	env := target.EnvMap()
	for _, k := range slices.Sorted(maps.Keys(env)) {
		b.WriteString("export " + k + "=" + transport.ShellQuote(env[k]) + "\n")
	}
	if err := os.WriteFile(target.ctr.env, []byte(b.String()), 0o644); err != nil {
		w.T.Fatal(err)
	}
	w.writeSSHConfig()
}

// sshConfig is the world's ssh config: each ssh name to its container,
// as its Down says.
func (w *World) sshConfig() string { return filepath.Join(w.Dir, "ssh_config") }

func (w *World) writeSSHConfig() {
	w.T.Helper()
	var b strings.Builder
	for _, a := range slices.Sorted(maps.Keys(w.links)) {
		s := w.links[a]
		if s.target.ctr == nil {
			continue
		}
		fmt.Fprintf(&b, "Host %s\n", a)
		key := ctrs.key // IdentityFile adds up, so each name has its own
		switch s.down {
		case "":
		case "refused": // nothing listens there
			b.WriteString("  Port 1\n")
		case "timeout": // the container drops SYNs there
			b.WriteString("  Port 2222\n")
		case "password": // an sshd with key auth off
			b.WriteString("  Port 2223\n")
		case "resolve":
			b.WriteString("  HostName tt-unknown.invalid\n")
		case "hostkey":
			fmt.Fprintf(&b, "  UserKnownHostsFile %s\n", ctrs.knownBad)
		case "auth":
			key = ctrs.other
		default:
			w.T.Fatalf("%s: down %q has no mechanism over real ssh (see docs/progress.md, Phase 2)", a, s.down)
		}
		fmt.Fprintf(&b, "  HostName %s\n  Port %d\n  IdentityFile %s\n", s.target.ctr.ip, s.port, key)
	}
	fmt.Fprintf(&b, "Host *\n  User tt\n  IdentitiesOnly yes\n  IdentityAgent none\n"+
		"  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n", ctrs.known)
	if err := os.WriteFile(w.sshConfig(), []byte(b.String()), 0o644); err != nil {
		w.T.Fatal(err)
	}
}

// ctrApply puts alias's state in place with real mechanisms, prev being
// what is in place. The link and every fault but Down are the
// container's, whichever of its names they are set through.
func (w *World) ctrApply(alias string, prev linkState, s *linkState) {
	w.T.Helper()
	c := s.target.ctr
	switch {
	case s.WindowKB != 0:
		w.T.Fatalf("%s: WindowKB has no mechanism over real ssh (see docs/progress.md, Phase 2)", alias)
	case s.exit != nil:
		w.T.Fatalf("%s: ExitWith has no mechanism over real ssh (see docs/progress.md, Phase 2)", alias)
	case s.oDelayMs != 0:
		w.T.Fatalf("%s: SlowControl has no mechanism over real ssh (see docs/progress.md, Phase 2)", alias)
	}
	if s.down != prev.down {
		w.writeSSHConfig()
	}
	if s.DelayMs != prev.DelayMs || s.JitterMs != prev.JitterMs || s.BwKBps != prev.BwKBps {
		w.netem(c, s.Link)
	}
	if s.halfOpenAt != prev.halfOpenAt && s.halfOpenAt != 0 {
		w.halfOpen(c, s.halfOpenAt)
	}
	if s.freeze != prev.freeze {
		script := "iptables -F TT-FREEZE"
		if s.freeze {
			script = "iptables -A TT-FREEZE -p tcp -j DROP"
		}
		w.rootExec(c, script)
	}
	if s.drops > prev.drops {
		w.dropConns(c)
	}
	if s.stall != prev.stall {
		if s.stall {
			w.stallOn(c)
		} else {
			w.stallOff(c)
		}
	}
}

// netem shapes the container's link: its eth0 egress and, through ifb0,
// its ingress, so each direction gets the one-way delay.
func (w *World) netem(c *ctrSlot, l Link) {
	w.T.Helper()
	script := "tc qdisc del dev eth0 root 2>/dev/null; tc qdisc del dev ifb0 root 2>/dev/null; true"
	if l.DelayMs > 0 || l.JitterMs > 0 || l.BwKBps > 0 {
		netem := fmt.Sprintf("netem delay %dms %dms limit 100000", l.DelayMs, l.JitterMs)
		if l.BwKBps > 0 {
			netem += fmt.Sprintf(" rate %dkbit", l.BwKBps*8)
		}
		script = "tc qdisc replace dev eth0 root " + netem + " && tc qdisc replace dev ifb0 root " + netem
	}
	w.rootExec(c, script)
}

// halfOpen drops, both ways and for good, every connection to the
// container's sshd that is established now: what a network change does
// to them. at may be a little in the past, not in the future.
func (w *World) halfOpen(c *ctrSlot, at int64) {
	w.T.Helper()
	if d := time.Until(time.UnixMilli(at)); d > 100*time.Millisecond {
		w.T.Fatalf("a network change %v ahead has no mechanism over real ssh", d)
	}
	script := `ss -Htn state established | while read -r _ _ local peer; do ` +
		`lport=${local##*:}; ip=${peer%:*}; port=${peer##*:}; ` +
		`iptables -A TT-HALFOPEN -p tcp -s "$ip" --sport "$port" --dport "$lport" -j DROP; ` +
		`iptables -A TT-HALFOPEN -p tcp -d "$ip" --dport "$port" --sport "$lport" -j DROP; done`
	w.rootExec(c, script)
}

// ctrProc is a process of a host container.
type ctrProc struct {
	pid, ppid, uid int
	argv0          string
}

// ctrProcs lists the container's processes: those in its cgroup (it
// shares the machine's pids).
func ctrProcs(c *ctrSlot) []ctrProc {
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	var out []ctrProc
	for _, d := range dirs {
		cg, err := os.ReadFile(filepath.Join(d, "cgroup"))
		if err != nil || !bytes.Contains(cg, []byte(c.id)) {
			continue
		}
		pid, _ := strconv.Atoi(filepath.Base(d))
		st, err1 := os.ReadFile(filepath.Join(d, "stat"))
		cmd, err2 := os.ReadFile(filepath.Join(d, "cmdline"))
		var fi syscall.Stat_t
		if err1 != nil || err2 != nil || syscall.Stat(d, &fi) != nil {
			continue
		}
		// pid (comm) state ppid …: comm may hold spaces, so after the last ')'.
		rest := st[bytes.LastIndexByte(st, ')')+1:]
		f := strings.Fields(string(rest))
		if len(f) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		argv0, _, _ := strings.Cut(string(cmd), "\x00")
		out = append(out, ctrProc{pid: pid, ppid: ppid, uid: int(fi.Uid), argv0: argv0})
	}
	return out
}

// dropConns ends every ssh connection to the container: its per-connection
// sshd processes ("sshd: tt [priv]", "sshd: tt@…"), so the connections
// close.
func (w *World) dropConns(c *ctrSlot) {
	w.T.Helper()
	var pids []string
	for _, p := range ctrProcs(c) {
		if strings.HasPrefix(p.argv0, "sshd: tt") {
			pids = append(pids, strconv.Itoa(p.pid))
		}
	}
	if len(pids) > 0 {
		// Each pid checked again where it is killed: still in the
		// container's cgroup (there, relative to its own: the same).
		w.rootExec(c, "self=$(cat /proc/self/cgroup); for p in "+strings.Join(pids, " ")+
			`; do [ "$(cat /proc/$p/cgroup 2>/dev/null)" = "$self" ] && kill -9 $p; done; true`)
	}
}

// inCtr reports whether pid is still a process of the container.
func inCtr(c *ctrSlot, pid int) bool {
	cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	return err == nil && bytes.Contains(cg, []byte(c.id))
}

// sessionProcs are the processes ssh sessions run in the container: the
// descendants of its "sshd: tt@…" processes, not those (sshd answers
// keepalives) and not what has left them (a towerd, a tmux server).
func sessionProcs(c *ctrSlot) []int {
	ps := ctrProcs(c)
	parent := map[int]int{}
	sess := map[int]bool{}
	for _, p := range ps {
		parent[p.pid] = p.ppid
		if strings.HasPrefix(p.argv0, "sshd: tt@") {
			sess[p.pid] = true
		}
	}
	var out []int
	for _, p := range ps {
		if sess[p.pid] || p.uid != os.Getuid() {
			continue
		}
		for q, n := parent[p.pid], 0; q > 1 && n < 64; q, n = parent[q], n+1 {
			if sess[q] {
				out = append(out, p.pid)
				break
			}
		}
	}
	return out
}

// stallOn stops the processes the container's ssh sessions run, and keeps
// stopping new ones, until stallOff: nothing reads or writes at the far
// end while sshd still answers.
func (w *World) stallOn(c *ctrSlot) {
	if c.stall != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	stopped := map[int]bool{}
	sweep := func() {
		for _, pid := range sessionProcs(c) {
			if !stopped[pid] && inCtr(c, pid) && syscall.Kill(pid, syscall.SIGSTOP) == nil {
				stopped[pid] = true
			}
		}
	}
	sweep()
	c.stall, c.stallDone = stop, done
	go func() {
		defer close(done)
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				for pid := range stopped {
					if inCtr(c, pid) {
						syscall.Kill(pid, syscall.SIGCONT)
					}
				}
				return
			case <-t.C:
				sweep()
			}
		}
	}()
}

// stallOff ends a stall: every process it stopped continues.
func (w *World) stallOff(c *ctrSlot) {
	if c.stall == nil {
		return
	}
	close(c.stall)
	<-c.stallDone
	c.stall, c.stallDone = nil, nil
}

// ctrExec runs a shell command on h's container as tt, with its
// environment (through tt-run).
func (h *Host) ctrExec(cmd string) (string, error) {
	out, err := ctrs.docker("exec", "-u", "tt", "-e", "SSH_ORIGINAL_COMMAND="+cmd, h.ctr.ctr, "/usr/local/bin/tt-run")
	if err != nil {
		return out, fmt.Errorf("on %s: %s: %v: %s", h.ctr.ctr, cmd, err, out)
	}
	return out, nil
}

// ctrEnv is the base of a container host's environment: the variables
// of this machine that hold there too.
func ctrEnv() map[string]string {
	m := map[string]string{"PATH": ctrPath}
	for _, k := range []string{"TMUX_TMPDIR", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TOWER_STANDBY", "TOWER_RELAY"} {
		if v, ok := os.LookupEnv(k); ok {
			m[k] = v
		}
	}
	return m
}
