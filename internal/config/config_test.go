package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTmuxTag(t *testing.T) {
	cases := map[string]string{
		"":              "default",
		"-L work":       "L-work",
		"-Lwork":        "L-work",
		"-f /dev/null":  "default",
		"-S /tmp/x/s":   "S-" + shortHash("/tmp/x/s", 8),
		"-L a/b":        "L-a_b-" + shortHash("a/b", 8),
		"-f x -L tt-s1": "L-tt-s1",
	}
	for in, want := range cases {
		if got := TmuxTag(strings.Fields(in)); got != want {
			t.Errorf("TmuxTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMachineKey(t *testing.T) {
	t.Setenv("TOWER_MKEY", "")
	t.Setenv("TOWER_MACHINE_ID", "m1")
	a, err := MachineKey()
	if err != nil || !ValidMKey(a) {
		t.Fatalf("%q %v", a, err)
	}
	t.Setenv("TOWER_MACHINE_ID", "m2")
	b, _ := MachineKey()
	if a == b {
		t.Fatal("different machines must have different keys")
	}
	t.Setenv("TOWER_MKEY", "0123456789ab")
	if k, _ := MachineKey(); k != "0123456789ab" {
		t.Fatal("a valid TOWER_MKEY is taken as is")
	}
	t.Setenv("TOWER_MKEY", "nothex")
	if k, _ := MachineKey(); k != b {
		t.Fatal("an invalid TOWER_MKEY is ignored")
	}
}

func TestLoadRunDirFallsBack(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 90))
	t.Setenv("TOWER_HOME", long)
	t.Setenv("TOWER_MACHINE_ID", "m")
	t.Setenv("TOWER_MKEY", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", "/tmp")
	e, err := Load([]string{"-L", "x"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(e.RunDir) })
	if strings.HasPrefix(e.RunDir, long) {
		t.Fatalf("run dir %s is too long for sockets", e.RunDir)
	}
	if len(e.CMDir())+1+40+sshTempSuffix >= sockMax || len(e.Socket()) >= sockMax {
		t.Fatalf("socket paths too long: %s", e.RunDir)
	}
	if e.StateDir != filepath.Join(long, "state", "towerd", e.MKey+"-L-x") {
		t.Fatal(e.StateDir)
	}
	if _, err := os.Stat(e.CMDir()); err != nil {
		t.Fatal(err)
	}
}

// TestRunDirIgnoresSession: an ssh command, a login shell and a tmux
// server see different XDG_RUNTIME_DIR and TMPDIR, and must still reach one
// towerd; two TOWER_HOMEs (two machines simulated on one box) never share.
func TestRunDirIgnoresSession(t *testing.T) {
	t.Setenv("TOWER_MKEY", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	load := func(home, machine, xdg, tmp string) *Env {
		t.Helper()
		t.Setenv("TOWER_HOME", home)
		t.Setenv("TOWER_MACHINE_ID", machine)
		t.Setenv("XDG_RUNTIME_DIR", xdg)
		t.Setenv("TMPDIR", tmp)
		e, err := Load([]string{"-L", "x"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(e.RunDir) })
		return e
	}
	short, err := os.MkdirTemp("/tmp", "x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	ssh := load("", "m", "", "")
	shell := load("", "m", short, short)
	if ssh.RunDir != shell.RunDir || ssh.Socket() != shell.Socket() || ssh.LockPath() != shell.LockPath() {
		t.Fatalf("one machine, two run dirs: %s and %s", ssh.RunDir, shell.RunDir)
	}
	if other := load("", "m2", "", ""); other.RunDir == ssh.RunDir {
		t.Fatal("two machine keys share a run dir")
	}
	a, b := load(t.TempDir(), "ma", short, "/tmp"), load(t.TempDir(), "mb", short, "/tmp")
	if a.Socket() == b.Socket() || a.CMDir() == b.CMDir() || a.LockPath() == b.LockPath() {
		t.Fatalf("two TOWER_HOMEs share %s", a.RunDir)
	}
}

// TestRunDirNotASymlink: a run dir candidate someone made a symlink is
// passed over.
func TestRunDirNotASymlink(t *testing.T) {
	d := filepath.Join(t.TempDir(), "run")
	if err := os.Symlink(t.TempDir(), d); err != nil {
		t.Fatal(err)
	}
	if private(d) {
		t.Fatal("a symlinked run dir was taken")
	}
	if !private(filepath.Join(t.TempDir(), "run")) {
		t.Fatal("a fresh run dir was refused")
	}
}

func TestTowerdIDStable(t *testing.T) {
	d := t.TempDir()
	a, err := TowerdID(d)
	if err != nil || len(a) != 8 {
		t.Fatal(a, err)
	}
	b, _ := TowerdID(d)
	if a != b {
		t.Fatal("id must persist")
	}
}

func TestHostsRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts.toml")
	if hs, err := LoadHosts(p); err != nil || hs != nil {
		t.Fatal("a missing file is an empty list")
	}
	off := false
	in := []Host{{Name: "gb200", SSH: "gb200"}, {Name: "pp", SSH: "pp", Enabled: &off, Tmux: "-L work"}}
	if err := SaveHosts(p, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadHosts(p)
	if err != nil || len(out) != 2 || out[1].On() || !out[0].On() || out[1].Tmux != "-L work" {
		t.Fatalf("%+v %v", out, err)
	}
	os.WriteFile(p, []byte("[[host]]\nname=\"a\"\n[[host]]\nname=\"A\"\n"), 0o600)
	if _, err := LoadHosts(p); err == nil {
		t.Fatal("duplicate names must fail")
	}
}

func TestDefaultName(t *testing.T) {
	cases := map[string]string{
		"me@box.lan:2222":       "box",
		"box":                   "box",
		"ssh://me@box.lan:2222": "box",
		// An address stays whole rather than cut at its first dot or
		// colon; loopback, as the repo names no host's address.
		"me@[::1]:2222":         "::1",
		"gb200":                 "gb200",
		"user@host.example.com": "host",
	}
	for in, want := range cases {
		if got := DefaultName(in); got != want {
			t.Errorf("DefaultName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostSame(t *testing.T) {
	f1, f2, tr := false, false, true
	a := Host{Name: "pc", SSH: "pc", Standby: &f1}
	b := Host{Name: "pc", SSH: "pc", Standby: &f2}
	if !a.Same(b) {
		t.Fatal("equal settings behind different pointers are the same")
	}
	if a.Same(Host{Name: "pc", SSH: "pc", Standby: &tr}) {
		t.Fatal("standby differs")
	}
	if !(Host{Name: "pc", Enabled: &tr}).Same(Host{Name: "pc"}) {
		t.Fatal("enabled = true is the default")
	}
	if a.Same(Host{Name: "pc", SSH: "pc", Standby: &f1, Tmux: "-L x"}) {
		t.Fatal("tmux differs")
	}
}
