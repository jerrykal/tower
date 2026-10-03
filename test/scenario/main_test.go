// Package scenario is tower's acceptance suite. Each simulated host is a
// tmux server of its own with its own TOWER_HOME and machine id, reached
// through the fake ssh; each terminal is a pane of yet another tmux
// server, running the attach loop, driven with send-keys and read with
// capture-pane. Nothing touches the user's tmux servers.
package scenario

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Versions the suite builds tower at: the current one, and a newer one
// for upgrades.
const (
	Version  = "0.0.1-test"
	Version2 = "0.0.2-test"
)

var (
	root     string // TOWER_TEST_DIR
	towerBin string
	tower2   string
	fakeSSH  string
)

func TestMain(m *testing.M) {
	if os.Getenv("TOWER_SCENARIOS") != "1" {
		// Part of `go test ./...` only on request: the suite takes minutes
		// and needs an isolated environment (mise run scenarios).
		fmt.Println("scenario suite skipped: run it with mise run scenarios")
		os.Exit(0)
	}
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "scenario:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func setup() error {
	root = os.Getenv("TOWER_TEST_DIR")
	if root == "" {
		root = filepath.Join(os.TempDir(), "tower-test")
	}
	if os.Getenv("TMUX_TMPDIR") == "" || len(os.Getenv("TMUX_TMPDIR")) > 40 {
		return fmt.Errorf("set a short private TMUX_TMPDIR (tmux sockets are limited to 104 bytes)")
	}
	os.Unsetenv("TMUX")
	os.Unsetenv("TMUX_PANE")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	towerBin = filepath.Join(bin, "tower")
	tower2 = filepath.Join(bin, "tower-v2")
	fakeSSH = filepath.Join(bin, "ssh")
	builds := []struct{ out, pkg, version string }{
		{towerBin, "../../cmd/tower", Version},
		{tower2, "../../cmd/tower", Version2},
		{fakeSSH, "./fakessh", ""},
	}
	for _, b := range builds {
		args := []string{"build", "-o", b.out}
		if b.version != "" {
			args = append(args, "-ldflags", "-X github.com/jerrykal/tower/internal/version.Version="+b.version)
		}
		cmd := exec.Command("go", append(args, b.pkg)...)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %v\n%s", b.pkg, err, out)
		}
	}
	return nil
}
