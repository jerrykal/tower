package hosts

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jerrykal/tower/internal/config"
)

func TestSSHAliases(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "conf.d"), 0o700)
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("config", `# comment
Host gb200 pc
  HostName 10.0.0.1
Host *.lan !bad
Host=831
host "pp" gb200
Include conf.d/*
Match all
`)
	write("conf.d/a", "Host workbox\nInclude ../config\n")
	got := SSHAliases(filepath.Join(dir, "config"))
	want := []string{"gb200", "pc", "831", "pp", "workbox"}
	if !slices.Equal(got, want) {
		t.Fatalf("aliases %q, want %q", got, want)
	}
	if got := SSHAliases(filepath.Join(dir, "nope")); len(got) != 0 {
		t.Fatalf("a missing file: %q", got)
	}
}

func TestNames(t *testing.T) {
	hosts := []config.Host{{Name: "gb200"}, {Name: "box", SSH: "me@box.lan"}}
	aliases := []string{"gb200", "pc", "nas"}
	for _, c := range []struct {
		name, target string
		skip         int
		ok           bool
	}{
		{"GB200", "gb200", -1, false}, // taken, any case
		{"pc", "pc", -1, true},        // the alias's own host
		{"pc", "me@other", -1, false}, // an alias for another host
		{"other", "me@other", -1, true},
		{"box", "me@box.lan", 1, true}, // renaming itself
		{"", "x", -1, false},
	} {
		err := CheckName(hosts, aliases, c.name, c.target, c.skip)
		if (err == nil) != c.ok {
			t.Errorf("CheckName(%q, %q): %v", c.name, c.target, err)
		}
	}
	if got := Clean(" my host:2 "); got != "my-host-2" {
		t.Errorf("Clean: %q", got)
	}
	for target, want := range map[string]string{
		"me@box.lan:2222": "box-2", // box is taken
		"nas":             "nas",   // the alias nas, added as itself
		"me@nas.lan":      "nas-2", // nas is another host's alias
		"10.0.0.5":        "10.0.0.5",
	} {
		if got := FreeName(hosts, aliases, target); got != want {
			t.Errorf("FreeName(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestEdits(t *testing.T) {
	dir := t.TempDir()
	e := &config.Env{ConfigDir: dir, RunDir: dir, StateDir: dir}
	l := &List{Env: e, SSHConfig: filepath.Join(dir, "ssh_config")}
	os.WriteFile(l.SSHConfig, []byte("Host gb200 pc\n"), 0o600)
	if err := config.SaveHosts(e.HostsFile(), []config.Host{{Name: "gb200"}, {Name: "pp"}}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetOn("PP", false); err != nil {
		t.Fatal(err)
	}
	if err := l.Rename("gb200", "big box"); err != nil {
		t.Fatal(err)
	}
	if err := l.Rename("pp", "pc"); err == nil {
		t.Fatal("pc is another host's alias")
	}
	hs, _ := l.Load()
	if len(hs) != 2 || hs[0].Name != "big-box" || hs[0].Target() != "gb200" || hs[1].On() {
		t.Fatalf("after the edits: %+v", hs)
	}
	if err := l.Remove("big-box"); err != nil {
		t.Fatal(err)
	}
	if err := l.Remove("nope"); err == nil {
		t.Fatal("removing a host that is not there")
	}
	if hs, _ := l.Load(); len(hs) != 1 || hs[0].Name != "pp" {
		t.Fatalf("after remove: %+v", hs)
	}
}
