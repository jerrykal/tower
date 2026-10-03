package transport

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jerrykal/tower/internal/config"
)

func TestRemoteCommandSurvivesTheShell(t *testing.T) {
	args := []string{"towerd", "--stdio", "--tmux", "-L it's ; $(x) `y` \"z\"", "", "a b"}
	line := RemoteCommand("/bin/echo", args...)
	// The remote side runs the line through a shell; print each argument.
	out, err := exec.Command("/bin/sh", "-c", "set -- "+line[len("/bin/echo"):]+`; for a; do printf '[%s]' "$a"; done`).Output()
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, a := range args {
		want.WriteString("[" + a + "]")
	}
	if string(out) != want.String() {
		t.Fatalf("got %s want %s", out, want.String())
	}
	if got := RemoteCommand("~/.local/share/tower/current/tower", "x"); !strings.HasPrefix(got, `"$HOME"/`) {
		t.Fatalf("~ must expand on the remote: %s", got)
	}
}

func TestOptions(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ssh")
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'OpenSSH_9.6p1, LibreSSL 3.3.6' >&2\n"), 0o755)
	s := &SSH{Bin: fake, CMDir: "/run/cm"}
	o := strings.Join(s.Options(config.Host{Name: "pc"}), " ")
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=5", "ServerAliveInterval=5", "ControlMaster=auto", "ControlPersist=10m", "ControlPath=/run/cm/%C", "ObscureKeystrokeTiming=no"} {
		if !strings.Contains(o, want) {
			t.Errorf("missing %s in %s", want, o)
		}
	}
	if o := strings.Join(s.Options(config.Host{Name: "pc", ObscureKeystrokes: true}), " "); strings.Contains(o, "ObscureKeystrokeTiming") {
		t.Error("a host that keeps obscuring gets ssh's default")
	}
	old := filepath.Join(dir, "ssh-old")
	os.WriteFile(old, []byte("#!/bin/sh\necho 'OpenSSH_8.9p1' >&2\n"), 0o755)
	if o := strings.Join((&SSH{Bin: old}).Options(config.Host{}), " "); strings.Contains(o, "ObscureKeystrokeTiming") {
		t.Error("an older ssh does not know the option")
	}
	argv := s.Attach(config.Host{Name: "pc", SSH: "me@pc"}, "tower attach")
	if argv[1] != "-t" || !slices.Contains(argv, "me@pc") || argv[len(argv)-2] != "--" {
		t.Fatalf("attach argv %v", argv)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		exit       int
		stderr     string
		class, has string
	}{
		{127, "sh: tower: command not found", Failed, "tower is not installed on pc"},
		{255, "No ED25519 host key is known for pc and you have requested strict checking.\nHost key verification failed.", Down, "host key"},
		{255, "pc: Permission denied (publickey,password).", Down, "ssh-add"},
		{255, "ssh: Could not resolve hostname pc: nodename nor servname provided", Down, "cannot resolve host name"},
		{255, "ssh: connect to host pc port 22: Connection refused", Down, "connection refused"},
		{255, "ssh: connect to host pc port 22: Operation timed out", Down, "timed out"},
		{255, "# Tailscale SSH requires an additional check.\n# To authenticate, visit: https://login.tailscale.com/a/x", Down, "Tailscale SSH wants a check: run `ssh pc` once"},
		{255, "Connection to pc closed by remote host.", Down, "connection lost"},
		{3, "", Down, "ssh exited 3"},
	}
	for _, c := range cases {
		f := Classify("pc", c.exit, c.stderr)
		if f.Class != c.class || !strings.Contains(f.Reason, c.has) {
			t.Errorf("Classify(%d, %q) = %+v", c.exit, c.stderr, f)
		}
	}
}

func TestSweepKeepsLiveSockets(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	live := filepath.Join(dir, "live")
	l, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// A listener that never accepts: busy, not dead.
	busy := filepath.Join(dir, "busy")
	lb, err := net.Listen("unix", busy)
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()
	dead := filepath.Join(dir, "dead")
	ld, _ := net.Listen("unix", dead)
	ld.(*net.UnixListener).SetUnlinkOnClose(false)
	ld.Close()
	Sweep(dir)
	for _, p := range []string{live, busy} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed", p)
		}
	}
	if _, err := os.Stat(dead); err == nil {
		t.Error("dead socket kept")
	}
}

func TestTmuxAtLeast(t *testing.T) {
	for v, want := range map[string]bool{"3.2": true, "3.2a": true, "3.10": true, "next-3.6": true, "3.1c": false, "2.9": false, "4.0": true, "": false} {
		if got := tmuxAtLeast(v, MinTmux); got != want {
			t.Errorf("%q: %v", v, got)
		}
	}
}
