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

// TestRunDirXDG: $XDG_RUNTIME_DIR holds the sockets of a user's own
// towerds, one directory per machine key; a TOWER_HOME (one of several
// machines simulated on one box) never shares it.
func TestRunDirXDG(t *testing.T) {
	// Short, as /run/user/<uid> is: sockets have a 104-byte limit.
	xdg, err := os.MkdirTemp("/tmp", "x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(xdg) })
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	t.Setenv("TOWER_MKEY", "")
	t.Setenv("TMPDIR", "/tmp")
	load := func(home, machine string) *Env {
		t.Helper()
		t.Setenv("TOWER_HOME", home)
		t.Setenv("TOWER_MACHINE_ID", machine)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("XDG_STATE_HOME", "")
		e, err := Load([]string{"-L", "x"})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	a, b := load(t.TempDir(), "ma"), load(t.TempDir(), "mb")
	if a.Socket() == b.Socket() || a.CMDir() == b.CMDir() || a.LockPath() == b.LockPath() {
		t.Fatalf("two TOWER_HOMEs share %s", a.RunDir)
	}
	for _, e := range []*Env{a, b} {
		if strings.HasPrefix(e.RunDir, xdg) {
			t.Fatalf("a TOWER_HOME's run dir %s is under XDG_RUNTIME_DIR", e.RunDir)
		}
	}
	u1, u2 := load("", "m1"), load("", "m2")
	if u1.RunDir != filepath.Join(xdg, "tower", u1.MKey) || u2.RunDir == u1.RunDir {
		t.Fatalf("run dirs without TOWER_HOME: %s and %s", u1.RunDir, u2.RunDir)
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
