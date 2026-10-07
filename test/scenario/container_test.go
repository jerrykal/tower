package scenario

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// ctrPerWorld is the most host containers a world takes (S18's four).
// The pool holds that many for each world that runs at once, so a world
// waiting for one never waits on another that waits too.
const ctrPerWorld = 4

// ctrMu guards which world each container is with.
var ctrMu sync.Mutex

// ctrPath is a container's PATH; ctrTmux its tmux.
const (
	ctrPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	ctrTmux = "/usr/local/bin/tmux"
)

type ctrBackend struct {
	run      string   // compose project suffix and image tag: this process's pid, and a nonce
	owner    string   // label value of whoever runs the suite (runOwner)
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
	mid  string // its machine id
	w    *World // the world using it
}

// SSHHost makes a host a remote reached over ssh: on the container
// backend, a container; on the sshd backend, a host of this machine
// behind sshds of its own.
func SSHHost() HostOpt {
	return func(h *Host) {
		switch {
		case ctrs != nil:
			h.ctr = &ctrSlot{} // the world assigns one
		case sshds != nil:
			h.overSSH = true
		}
	}
}

// containerSetup brings the hosts up: a key, the compose project, each
// host's address and host key.
func containerSetup() error {
	b := &ctrBackend{run: fmt.Sprintf("%d-%04x", os.Getpid(), rand.IntN(1<<16)), owner: runOwner()}
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
		// ssh-keygen asks before overwriting a key an earlier run left
		// (the same TOWER_TEST_DIR), and with no terminal says no.
		os.Remove(k)
		os.Remove(k + ".pub")
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
	b.env = append(os.Environ(), "TT_RUN="+b.run, "TT_OWNER="+b.owner,
		"TT_PUBKEY="+strings.TrimSpace(string(pub)), "TOWER_TEST_DIR="+root)
	if os.Getenv("TT_HOSTS") == "" {
		b.env = append(b.env, "TT_HOSTS="+strconv.Itoa(ctrPerWorld*parallelWorlds))
	}
	ctrs = b
	tmuxVersion := os.Getenv("TT_TMUX_VERSION")
	if tmuxVersion == "" {
		tmuxVersion = "3.7c"
	}
	// The run's own tag, which compose starts: another run building at
	// once, from other sources or another tmux, tags its own.
	if out, err := b.docker("build", "-q", "--label", "tower-test", "--label", "tower-test.owner="+b.owner,
		"-t", ctrImage+":"+b.run,
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
		rep, err := s.call(agentReq{Op: "exec", Root: true, Cmd: "cat /etc/machine-id"})
		if s.mid = strings.TrimSpace(rep.Out); err != nil || s.mid == "" {
			return fmt.Errorf("%s: no machine id: %v", s.ctr, err)
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

// ctrImage is the host image; each run tags it with its run.
const ctrImage = "tt-scenario-host"

// runRE is a run of the container backend: its process's pid, and a nonce.
var runRE = regexp.MustCompile(`^(\d+)-[0-9a-f]{4}$`)

// runOwner names whoever runs the suite, as far as their pids go: the
// machine, the user and the pid namespace. A docker daemon can serve
// other users, and a container (a devcontainer on the machine's daemon)
// has pids of its own: only an owner's own runs are judged by their pid.
func runOwner() string {
	host, _ := os.Hostname()
	ns, _ := os.Readlink("/proc/self/ns/pid")
	ns = strings.Trim(strings.TrimPrefix(ns, "pid:"), "[]")
	return fmt.Sprintf("%s.%d.%s", host, os.Getuid(), ns)
}

// running reports whether pid is a process: one of another user's
// (EPERM) is.
func running(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// sweep takes down the projects, and untags the images, of the owner's
// earlier runs that ended without their teardown (a go test timeout):
// those whose run's process is gone.
func (b *ctrBackend) sweep() {
	gone := func(run string) bool {
		m := runRE.FindStringSubmatch(run)
		if m == nil {
			return false
		}
		pid, err := strconv.Atoi(m[1])
		return err == nil && pid > 0 && !running(pid)
	}
	own := "label=tower-test.owner=" + b.owner
	out, _ := b.docker("ps", "-a", "--filter", "label=tower-test", "--filter", own, "--format", `{{.Label "com.docker.compose.project"}}`)
	seen := map[string]bool{}
	for _, p := range strings.Fields(out) {
		if seen[p] || !strings.HasPrefix(p, "tt-") || !gone(strings.TrimPrefix(p, "tt-")) {
			continue
		}
		seen[p] = true
		if out, err := b.docker("compose", "-p", p, "down", "--timeout", "1"); err != nil {
			fmt.Fprintf(os.Stderr, "take down %s: %v: %s\n", p, err, out)
		}
	}
	out, _ = b.docker("images", "--filter", "label=tower-test", "--filter", own, "--format", "{{.Repository}} {{.Tag}}")
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == ctrImage && gone(f[1]) {
			b.docker("rmi", ctrImage+":"+f[1])
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
// network (both labelled tower-test) and the run's image tag; the build
// cache keeps the image's layers for the next run.
func containerTeardown() {
	if ctrs == nil {
		return
	}
	if out, err := ctrs.docker(append(ctrs.compose, "down", "--timeout", "2")...); err != nil {
		fmt.Fprintf(os.Stderr, "compose down: %v: %s\n", err, out)
	}
	if out, err := ctrs.docker("rmi", ctrImage+":"+ctrs.run); err != nil {
		fmt.Fprintf(os.Stderr, "untag the host image: %v: %s\n", err, out)
	}
}

func (b *ctrBackend) docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	cmd.Env = b.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// assign gives h its machine's container: the one of a host on the same
// machine, or a free one (waiting for one while the pool is taken),
// reset (none of the test user's processes, an unshaped link, no
// faults), its ssh sessions running with the environments ctrName
// writes. Teardown resets it again and frees it.
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
	if n := len(w.ctrSlots()); n >= ctrPerWorld {
		w.T.Fatalf("%s would be this world's container %d; at most %d (ctrPerWorld)", h.Name, n+1, ctrPerWorld)
	}
	s := w.freeSlot(5 * time.Minute)
	if s == nil {
		w.T.Fatalf("no free host container for %s in 5m (%d)", h.Name, len(ctrs.slots))
	}
	err := os.MkdirAll(s.envDir(w), 0o755)
	if err == nil {
		_, err = s.call(agentReq{Op: "assign", Dir: s.envDir(w)})
	}
	if err != nil {
		// h is not the world's yet: teardown would not free it.
		ctrMu.Lock()
		s.w = nil
		ctrMu.Unlock()
		w.T.Fatalf("assign %s: %v", s.ctr, err)
	}
	h.ctr = s
}

// freeSlot takes a free container for w, waiting up to d for one.
func (w *World) freeSlot(d time.Duration) *ctrSlot {
	deadline := time.Now().Add(d)
	for {
		ctrMu.Lock()
		for _, s := range ctrs.slots {
			if s.w == nil {
				s.w = w
				ctrMu.Unlock()
				return s
			}
		}
		ctrMu.Unlock()
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ctrSlots are the containers w has taken. Which world a slot is with
// is read under ctrMu: the worlds running alongside take and free slots.
func (w *World) ctrSlots() []*ctrSlot {
	ctrMu.Lock()
	defer ctrMu.Unlock()
	var out []*ctrSlot
	for _, h := range w.hosts {
		if h.ctr != nil && h.ctr.w == w && !slices.Contains(out, h.ctr) {
			out = append(out, h.ctr)
		}
	}
	return out
}

// envDir holds the environment of each of the container's sshd ports in
// w: the host its ssh name stands for.
func (s *ctrSlot) envDir(w *World) string { return filepath.Join(w.Dir, "ctr-"+s.name) }

// release resets container s after the world and frees it. One that
// will not reset stays out of the pool: the next world would inherit its
// processes, shape and faults.
func (w *World) release(s *ctrSlot) {
	_, err := s.call(agentReq{Op: "reset"})
	if err != nil {
		_, err = s.call(agentReq{Op: "reset"})
	}
	if err != nil {
		w.T.Errorf("reset %s: %v (kept out of the pool)", s.ctr, err)
		return
	}
	ctrMu.Lock()
	s.w = nil
	ctrMu.Unlock()
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
	// The first port no other name of the container holds: a name
	// registered again, or moved to another host, gives its port back.
	taken := map[int]bool{}
	for a, s := range w.links {
		if a != alias && s.target.ctr == target.ctr && s.port != 0 {
			taken[s.port] = true
		}
	}
	i := slices.IndexFunc(ctrPorts, func(p int) bool { return !taken[p] })
	if i < 0 {
		w.T.Fatalf("%s: more than %d ssh names for one container", alias, len(ctrPorts))
	}
	port := ctrPorts[i]
	w.links[alias].port = port
	var b strings.Builder
	env := target.EnvMap()
	for _, k := range slices.Sorted(maps.Keys(env)) {
		b.WriteString("export " + k + "=" + transport.ShellQuote(env[k]) + "\n")
	}
	p := filepath.Join(target.ctr.envDir(w), strconv.Itoa(port)+".env")
	if err := writeAtomic(p, []byte(b.String()), 0o644); err != nil {
		w.T.Fatal(err)
	}
	w.writeSSHConfig()
}

// writeAtomic replaces path with data in one step (a rename): an ssh or
// tt-run reading it meanwhile sees the old file or the new, never a
// partial one.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sshConfig is the world's ssh config: each ssh name to its container
// or sshd, as its Down says.
func (w *World) sshConfig() string { return filepath.Join(w.Dir, "ssh_config") }

func (w *World) writeSSHConfig() {
	w.T.Helper()
	var b strings.Builder
	key, other, known, knownBad, user, alias := "", "", "", "", "", ""
	if ctrs != nil {
		key, other, known, knownBad, user = ctrs.key, ctrs.other, ctrs.known, ctrs.knownBad, "tt"
	} else {
		key, other, known, knownBad, user, alias = sshds.key, sshds.other, sshds.known, sshds.knownBad, sshds.user, sshdAlias
	}
	for _, a := range slices.Sorted(maps.Keys(w.links)) {
		s := w.links[a]
		// Each name's address, the port nothing answers a SYN on, and the
		// one with key auth off.
		var host string
		var port, dead, pw int
		switch {
		case s.target.ctr != nil:
			host, port, dead, pw = s.target.ctr.ip, s.port, 2222, 2223
		case s.sshd != nil:
			host, port, dead, pw = "127.0.0.1", s.sshd.port, s.sshd.dead, s.sshd.pw
		default:
			continue
		}
		fmt.Fprintf(&b, "Host %s\n", a)
		k := key // IdentityFile adds up, so each name has its own
		switch s.down {
		case "":
		case "refused": // nothing listens there
			port = 1
		case "timeout":
			port = dead
		case "password":
			port = pw
		case "resolve":
			host = "tt-unknown.invalid"
		case "hostkey":
			fmt.Fprintf(&b, "  UserKnownHostsFile %s\n", knownBad)
		case "auth":
			k = other
		default:
			w.T.Fatalf("%s: down %q has no mechanism over real ssh (see docs/progress.md, Phase 2)", a, s.down)
		}
		fmt.Fprintf(&b, "  HostName %s\n  Port %d\n  IdentityFile %s\n", host, port, k)
	}
	fmt.Fprintf(&b, "Host *\n  User %s\n  IdentitiesOnly yes\n  IdentityAgent none\n"+
		"  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n", user, known)
	if alias != "" {
		fmt.Fprintf(&b, "  HostKeyAlias %s\n", alias)
	}
	// Live ssh calls read it while a Down or Heal rewrites it.
	if err := writeAtomic(w.sshConfig(), []byte(b.String()), 0o644); err != nil {
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

// masterPid is this machine's pid of home's ssh master to alias (real
// ssh), or 0 when none answers within 3s.
func (w *World) masterPid(home *Host, alias string) int {
	var out []byte
	for start := time.Now(); time.Since(start) < 3*time.Second; time.Sleep(100 * time.Millisecond) {
		cmd := exec.Command("/usr/bin/ssh", "-F", w.sshConfig(), "-o", "ControlPath="+filepath.Join(home.Paths().CMDir(), "%C"), "-O", "check", alias)
		out, _ = cmd.CombinedOutput()
		if m := masterPidRE.FindSubmatch(out); m != nil {
			pid, _ := strconv.Atoi(string(m[1]))
			return home.HostPid(pid)
		}
	}
	w.T.Logf("no master to %s: %s", alias, out)
	return 0
}

// masterPidRE is the pid in ssh -O check's answer.
var masterPidRE = regexp.MustCompile(`\(pid=(\d+)\)`)

// ctrProc is a process of a host container, as this machine sees it.
type ctrProc struct {
	pid   int // this machine's
	nspid int // its pid in the container
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
		p := ctrProc{pid: pid}
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

// SplitFar splits pids into those of the machine the test runs on and
// those on h: in its container, or under its sshds (none for a host of
// the fake ssh).
func (h *Host) SplitFar(pids []int) (near, far []int) {
	var in map[int]bool
	switch {
	case h.ctr != nil:
		in = map[int]bool{}
		for _, p := range ctrProcs(h.ctr) {
			in[p.pid] = true
		}
	case h.overSSH:
		in = h.sshdFar()
	default:
		return pids, nil
	}
	for _, p := range pids {
		if in[p] {
			far = append(far, p)
		} else {
			near = append(near, p)
		}
	}
	return near, far
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
