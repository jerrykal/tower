package scenario

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/transport"
	"github.com/jerrykal/tower/test/scenario/fakenet"
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
	run     string   // compose project suffix: this process's pid
	compose []string // docker compose and its file
	env     []string // compose's variables
	key     string   // the client key
	known   string   // known_hosts of every slot
	slots   []*ctrSlot
}

// ctrSlot is one host container.
type ctrSlot struct {
	name string // h1, h2: the compose service
	ctr  string // container name
	ip   string
	env  string // the file tt-run sources: the current world's host
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
	b.known = filepath.Join(dir, "known_hosts")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", b.key).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen: %v: %s", err, out)
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
		out, err := b.docker("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", s.ctr)
		if err != nil {
			return fmt.Errorf("inspect %s: %v: %s", s.ctr, err, out)
		}
		s.ip = strings.TrimSpace(out)
		b.slots = append(b.slots, s)
	}
	// Each host's key, once sshd answers.
	var known []string
	for _, s := range b.slots {
		deadline := time.Now().Add(20 * time.Second)
		for {
			out, err := exec.Command("ssh-keyscan", "-T", "1", "-t", "ed25519", s.ip).Output()
			if err == nil && len(out) > 0 {
				known = append(known, strings.TrimSpace(string(out)))
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s: sshd never answered", s.ctr)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return os.WriteFile(b.known, []byte(strings.Join(known, "\n")+"\n"), 0o600)
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

// assign gives h a free slot of the world.
func (w *World) assign(h *Host) {
	w.T.Helper()
	for _, s := range ctrs.slots {
		if s.w == nil {
			s.w = w
			h.ctr = s
			w.real = true
			w.shape(s, 0, 0)
			w.T.Cleanup(func() { s.w = nil })
			return
		}
	}
	w.T.Fatalf("no free host container for %s (%d)", h.Name, len(ctrs.slots))
}

// shape sets the slot's one-way delay and jitter, both directions: the
// container's eth0 egress and, through ifb0, its ingress.
func (w *World) shape(s *ctrSlot, delayMs, jitterMs int) {
	w.T.Helper()
	script := "tc qdisc del dev eth0 root 2>/dev/null; tc qdisc del dev ifb0 root 2>/dev/null; true"
	if delayMs > 0 || jitterMs > 0 {
		netem := fmt.Sprintf("netem delay %dms %dms limit 100000", delayMs, jitterMs)
		script = "tc qdisc replace dev eth0 root " + netem + " && tc qdisc replace dev ifb0 root " + netem
	}
	if out, err := ctrs.docker("exec", "-u", "root", s.ctr, "sh", "-c", script); err != nil {
		w.T.Fatalf("shape %s: %v: %s", s.ctr, err, out)
	}
}

// ctrSSH registers alias as an ssh name for target's container: its
// environment for tt-run, and the world's ssh config.
func (w *World) ctrSSH(alias string, target *Host, k fakenet.Knobs) {
	w.T.Helper()
	if w.ctrAliases == nil {
		w.ctrAliases = map[string]*Host{}
		w.ctrKnobs = map[string]*fakenet.Knobs{}
	}
	w.ctrAliases[alias] = target
	w.ctrKnobs[alias] = &k
	var b strings.Builder
	env := target.EnvMap()
	for _, kv := range target.Env() {
		k, _, _ := strings.Cut(kv, "=")
		b.WriteString("export " + k + "=" + transport.ShellQuote(env[k]) + "\n")
	}
	if err := os.WriteFile(target.ctr.env, []byte(b.String()), 0o644); err != nil {
		w.T.Fatal(err)
	}
	w.writeSSHConfig()
	w.applyKnobs(alias)
}

// sshWrapper is the world's TOWER_SSH: ssh with the world's config only
// (neither the user's nor the system's).
func (w *World) sshWrapper() string {
	p := filepath.Join(w.Dir, "ssh")
	if _, err := os.Stat(p); err != nil {
		script := "#!/bin/sh\nexec /usr/bin/ssh -F " + transport.ShellQuote(filepath.Join(w.Dir, "ssh_config")) + " \"$@\"\n"
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			w.T.Fatal(err)
		}
	}
	return p
}

func (w *World) writeSSHConfig() {
	var b strings.Builder
	for _, a := range slices.Sorted(maps.Keys(w.ctrAliases)) {
		fmt.Fprintf(&b, "Host %s\n  HostName %s\n", a, w.ctrAliases[a].ctr.ip)
	}
	fmt.Fprintf(&b, "Host *\n  User tt\n  IdentityFile %s\n  IdentitiesOnly yes\n  IdentityAgent none\n"+
		"  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n  LogLevel ERROR\n", ctrs.key, ctrs.known)
	if err := os.WriteFile(filepath.Join(w.Dir, "ssh_config"), []byte(b.String()), 0o644); err != nil {
		w.T.Fatal(err)
	}
}

// applyKnobs puts alias's knobs in place with real mechanisms. ssh has
// a pty and a control master of its own (Pty, Mux); the faults have no
// mechanism yet.
func (w *World) applyKnobs(alias string) {
	w.T.Helper()
	k := w.ctrKnobs[alias]
	if k.Down != "" || k.LatencyMs != 0 || k.Exit != nil || k.Freeze || k.Drop != 0 || k.ODelayMs != 0 ||
		k.BwKBps != 0 || k.WindowKB != 0 || k.Stall || k.HalfOpenAt != 0 {
		w.T.Fatalf("%s: knobs %+v have no mechanism on the container backend yet", alias, *k)
	}
	w.shape(w.ctrAliases[alias].ctr, k.DelayMs, k.JitterMs)
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
