package scenario

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net"
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
// real ssh. The run's directories are bind-mounted at their own paths,
// so a host's files and its tmux and towerd sockets are where the
// harness looks for them; only ssh crosses the network. Each container
// is one machine: hosts on it (Machine) share it, each reached through
// ssh names of its own. It has pids of its own; an agent in it
// (hostagent) shapes its link, sets its faults, runs the host's commands
// there and resets it between worlds.

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
	ctr  string // container name, tt-<run>-host-<n>
	name string // its host name
	id   string // container id, in its processes' cgroup
	ip   string
	sock string // its agent's socket
	w    *World // the world using it
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
	b.sweep()
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
	ids, err := b.docker(append(b.compose, "ps", "-q")...)
	if err != nil {
		return fmt.Errorf("compose ps: %v: %s", err, ids)
	}
	for _, id := range strings.Fields(ids) {
		out, err := b.docker("inspect", "-f", "{{.Id}} {{.Name}} {{.Config.Hostname}} {{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", id)
		f := strings.Fields(out)
		if err != nil || len(f) != 4 {
			return fmt.Errorf("inspect %s: %v: %s", id, err, out)
		}
		s := &ctrSlot{id: f[0], ctr: strings.TrimPrefix(f[1], "/"), name: f[2], ip: f[3],
			sock: filepath.Join(root, "slots", f[2]+".sock")}
		b.slots = append(b.slots, s)
	}
	slices.SortFunc(b.slots, func(x, y *ctrSlot) int { return strings.Compare(x.ctr, y.ctr) })
	for _, s := range b.slots {
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := s.call(agentReq{Op: "reset"}); err == nil {
				break
			} else if time.Now().After(deadline) {
				return fmt.Errorf("%s: no agent: %v", s.ctr, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
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

// sweep takes down the projects of earlier runs that ended without
// their teardown (a go test timeout): those labelled tower-test whose
// run's process is gone.
func (b *ctrBackend) sweep() {
	out, _ := b.docker("ps", "-a", "--filter", "label=tower-test", "--format", `{{.Label "com.docker.compose.project"}}`)
	seen := map[string]bool{}
	for _, p := range strings.Fields(out) {
		pid, err := strconv.Atoi(strings.TrimPrefix(p, "tt-"))
		if seen[p] || err != nil || !strings.HasPrefix(p, "tt-") || alive(pid) {
			continue
		}
		seen[p] = true
		if out, err := b.docker("compose", "-p", p, "down", "--timeout", "1"); err != nil {
			fmt.Fprintf(os.Stderr, "take down %s: %v: %s\n", p, err, out)
		}
	}
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

// assign gives h its machine's container: the one of a host on the same
// machine, or a free one, reset (none of the test user's processes, an
// unshaped link, no faults), its ssh sessions running with the
// environments ctrName writes. Teardown resets it again and frees it.
func (w *World) assign(h *Host) {
	w.T.Helper()
	for _, o := range w.hosts {
		if o.Machine != h.Machine {
			continue
		}
		if o.ctr == nil {
			w.T.Fatalf("%s is a container on machine %s, %s is not", h.Name, h.Machine, o.Name)
		}
		h.ctr = o.ctr
		return
	}
	for _, s := range ctrs.slots {
		if s.w == nil {
			if err := os.MkdirAll(s.envDir(w), 0o755); err != nil {
				w.T.Fatal(err)
			}
			if _, err := s.call(agentReq{Op: "assign", Dir: s.envDir(w)}); err != nil {
				w.T.Fatalf("assign %s: %v", s.ctr, err)
			}
			s.w = w
			h.ctr = s
			return
		}
	}
	w.T.Fatalf("no free host container for %s (%d)", h.Name, len(ctrs.slots))
}

// envDir holds the environment of each of the container's sshd ports in
// w: the host its ssh name stands for.
func (s *ctrSlot) envDir(w *World) string { return filepath.Join(w.Dir, "ctr-"+s.name) }

// release resets h's container after the world and frees it.
func (w *World) release(h *Host) {
	if _, err := h.ctr.call(agentReq{Op: "reset"}); err != nil {
		w.T.Errorf("reset %s: %v", h.ctr.ctr, err)
	}
	h.ctr.w = nil
}

// agentReq is a call to a container's agent (hostagent.Request).
type agentReq struct {
	Op     string            `json:"op"`
	Dir    string            `json:"dir,omitempty"`
	Root   bool              `json:"root,omitempty"`
	Cmd    string            `json:"cmd,omitempty"`
	Vars   map[string]string `json:"vars,omitempty"`
	Ms     int               `json:"ms,omitempty"`
	On     bool              `json:"on,omitempty"`
	Port   int               `json:"port,omitempty"`
	Delay  int               `json:"delay,omitempty"`
	Jitter int               `json:"jitter,omitempty"`
	BwKBps int               `json:"bw,omitempty"`
}

// agentReply is its reply.
type agentReply struct {
	Out  string `json:"out"`
	Code int    `json:"code"`
	Err  string `json:"err"`
}

// call makes one call to the container's agent.
func (s *ctrSlot) call(r agentReq) (agentReply, error) {
	var rep agentReply
	c, err := net.DialTimeout("unix", s.sock, 5*time.Second)
	if err != nil {
		return rep, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30*time.Second + time.Duration(r.Ms)*time.Millisecond))
	b, _ := json.Marshal(r)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return rep, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return rep, err
	}
	if err := json.Unmarshal(line, &rep); err != nil {
		return rep, err
	}
	if rep.Err != "" {
		return rep, fmt.Errorf("%s", rep.Err)
	}
	return rep, nil
}

// agent makes a call that must succeed.
func (w *World) agent(s *ctrSlot, r agentReq) {
	w.T.Helper()
	if _, err := s.call(r); err != nil {
		w.T.Fatalf("%s: %s: %v", s.ctr, r.Op, err)
	}
}

// ctrPorts are a container's sshd ports: one per ssh name, so that each
// name has a control master of its own (ssh keys one by host, port and
// user).
var ctrPorts = []int{22, 2201, 2202, 2203, 2204, 2205, 2206, 2207, 2208, 2209, 2210, 2211, 2212, 2213, 2214, 2215}

// ctrName gives alias a port of target's container, and writes target's
// environment for tt-run: what every ssh session through the port starts
// with.
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
	p := filepath.Join(target.ctr.envDir(w), strconv.Itoa(ctrPorts[used])+".env")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
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
// what is in place. The link's shape and a network change are the
// container's (its machine's network), whichever of its names they are
// set through; Down, Freeze, Drop and Stall are the name's: its sshd
// port's connections.
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
		w.agent(c, agentReq{Op: "netem", Delay: s.DelayMs, Jitter: s.JitterMs, BwKBps: s.BwKBps})
	}
	if s.halfOpenAt != prev.halfOpenAt && s.halfOpenAt != 0 {
		if d := time.Until(time.UnixMilli(s.halfOpenAt)); d > 100*time.Millisecond {
			w.T.Fatalf("a network change %v ahead has no mechanism over real ssh", d)
		}
		w.agent(c, agentReq{Op: "halfopen"})
	}
	if s.freeze != prev.freeze {
		w.agent(c, agentReq{Op: "freeze", On: s.freeze, Port: s.port})
	}
	if s.drops > prev.drops {
		w.agent(c, agentReq{Op: "drop", Port: s.port})
	}
	if s.stall != prev.stall {
		w.agent(c, agentReq{Op: "stall", On: s.stall, Port: s.port})
	}
}

// ctrProc is a process of a host container, as this machine sees it.
type ctrProc struct {
	pid, ppid, uid int
	argv0          string
	nspid          int // its pid in the container
}

// ctrProcs lists the container's processes: those in its cgroup, with
// this machine's pids and the container's.
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
		p := ctrProc{pid: pid, ppid: ppid, uid: int(fi.Uid), argv0: argv0}
		// NSpid: this machine's pid, then the container's.
		status, _ := os.ReadFile(filepath.Join(d, "status"))
		for _, l := range strings.Split(string(status), "\n") {
			if rest, ok := strings.CutPrefix(l, "NSpid:"); ok {
				if f := strings.Fields(rest); len(f) > 0 {
					p.nspid, _ = strconv.Atoi(f[len(f)-1])
				}
			}
		}
		out = append(out, p)
	}
	return out
}

// HostPid is this machine's pid of the host's process pid (a pid read on
// the host: a pid file, a tmux client), or 0 when none runs; on a host of
// this machine, pid itself.
func (h *Host) HostPid(pid int) int {
	if h.ctr == nil || pid <= 0 {
		return pid
	}
	for _, p := range ctrProcs(h.ctr) {
		if p.nspid == pid {
			return p.pid
		}
	}
	return 0
}

// ctrRun runs argv in h's container as tt with environment env, killed
// after d, and returns its combined output. An exit status is an error,
// as exec's.
func (h *Host) ctrRun(env map[string]string, d time.Duration, argv ...string) (string, error) {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = transport.ShellQuote(a)
	}
	rep, err := h.ctr.call(agentReq{Op: "exec", Cmd: strings.Join(q, " "), Vars: env, Ms: int(d.Milliseconds())})
	if err == nil && rep.Code != 0 {
		err = fmt.Errorf("exit status %d", rep.Code)
	}
	return rep.Out, err
}

// ctrStart starts argv in h's container as tt with environment env, in a
// session of its own, and returns.
func (h *Host) ctrStart(env map[string]string, argv ...string) error {
	q := []string{"setsid"}
	for _, a := range argv {
		q = append(q, transport.ShellQuote(a))
	}
	rep, err := h.ctr.call(agentReq{Op: "exec", Cmd: strings.Join(q, " ") + " </dev/null >/dev/null 2>&1 &", Vars: env})
	if err == nil && rep.Code != 0 {
		err = fmt.Errorf("exit status %d: %s", rep.Code, rep.Out)
	}
	return err
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
