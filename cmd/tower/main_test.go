package main

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jerrykal/tower/internal/config"
)

func TestRunUnknownCommand(t *testing.T) {
	if err := run([]string{"nope"}); err == nil {
		t.Fatal("an unknown command must fail")
	}
}

func TestRunVersion(t *testing.T) {
	if err := run([]string{"version"}); err != nil {
		t.Fatal(err)
	}
}

func TestParseFlagsInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	name := fs.String("name", "", "")
	var tm tmuxFlag
	fs.Var(&tm, "tmux", "")
	pos, err := parseFlags(fs, []string{"B", "--name", "bee", "--tmux", "-L x", "extra"})
	if err != nil || !slices.Equal(pos, []string{"B", "extra"}) || *name != "bee" || !slices.Equal(tm.value(), []string{"-L", "x"}) {
		t.Fatalf("%v %v %q %v", pos, err, *name, tm.value())
	}
	var none tmuxFlag
	if none.value() != nil {
		t.Fatal("an unset --tmux leaves TOWER_TMUX in charge")
	}
	none.Set("")
	if v := none.value(); v == nil || len(v) != 0 {
		t.Fatal("--tmux '' is the default server")
	}
}

func TestHostAddRefusesATakenName(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tch")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("TOWER_HOME", dir)
	t.Setenv("TOWER_MACHINE_ID", "unit")
	env, err := config.Load([]string{"-L", "unit"})
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveHosts(filepath.Join(env.ConfigDir, "hosts.toml"), []config.Host{{Name: "bee", SSH: "B"}}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err = cmdHost([]string{"add", "B", "--name", "BEE"}, &out)
	if err == nil || !strings.Contains(err.Error(), `a host named "BEE" already exists`) {
		t.Fatalf("%v", err)
	}
	if err := cmdHost([]string{"off", "bee"}, &out); err != nil {
		t.Fatal(err)
	}
	hs, _ := config.LoadHosts(env.HostsFile())
	if len(hs) != 1 || hs[0].On() {
		t.Fatalf("off: %+v", hs)
	}
	if err := cmdHost([]string{"rm", "BEE"}, &out); err != nil {
		t.Fatal(err)
	}
	if hs, _ := config.LoadHosts(env.HostsFile()); len(hs) != 0 {
		t.Fatalf("rm: %+v", hs)
	}
}

// A refused hand-off's note is shown as text: a #(…) from a host's ssh
// error is not run.
func TestLastRefusalIsLiteral(t *testing.T) {
	got := refusal("/dev/pts/3", "alpha is down: boom #(touch x) #S")
	if want := "tower: alpha is down: boom ##(touch x) ##S"; got[len(got)-1] != want {
		t.Fatalf("refusal %q, want %q", got[len(got)-1], want)
	}
}
