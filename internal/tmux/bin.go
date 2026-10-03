// Package tmux is everything tower says to tmux: which binary to run, how
// to quote, one-shot commands, and the control-mode client towerd keeps on
// its server.
package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Resolve finds name's binary past version-manager shims: for each match
// on PATH, a plain binary is the answer; a mise or asdf shim is asked
// which binary it runs, and passed over when it cannot say (its tool is
// not active here), which is what the shim itself would fall back to. A
// shim is a process start of its own on every call (60–80ms for tmux
// through mise's), so tower runs the binary directly. With no match it
// returns name.
func Resolve(name string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if !executable(p) {
			continue
		}
		mgr := shimManager(p)
		if mgr == "" {
			return p
		}
		if real := askShim(mgr, name); real != "" {
			return real
		}
	}
	return name
}

// shimManager returns "mise" or "asdf" when p is one of their shims.
func shimManager(p string) string {
	dir := filepath.Dir(p)
	if filepath.Base(dir) != "shims" {
		// mise's shims may be symlinks to mise itself.
		if t, err := os.Readlink(p); err == nil && filepath.Base(t) == "mise" {
			return "mise"
		}
		return ""
	}
	parent := filepath.Base(filepath.Dir(dir))
	switch {
	case strings.Contains(parent, "mise") || strings.Contains(dir, "/mise/"):
		return "mise"
	case strings.Contains(parent, "asdf"):
		return "asdf"
	}
	if t, err := os.Readlink(p); err == nil && filepath.Base(t) == "mise" {
		return "mise"
	}
	return ""
}

func askShim(mgr, name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, mgr, "which", name).Output()
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	if filepath.IsAbs(p) && executable(p) {
		return p
	}
	return ""
}

func executable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

var (
	binOnce sync.Once
	bin     string
)

// Bin is the tmux binary every tower process runs: TOWER_TMUX_BIN when it
// is an absolute executable path (towerd passes the one it resolved),
// else Resolve("tmux"), once per process.
func Bin() string {
	binOnce.Do(func() {
		if p := os.Getenv("TOWER_TMUX_BIN"); filepath.IsAbs(p) && executable(p) {
			bin = p
			return
		}
		bin = Resolve("tmux")
	})
	return bin
}

// UseBin makes p the binary Bin returns, when it is an absolute
// executable path: a process that learns towerd's resolved binary takes
// it instead of resolving again.
func UseBin(p string) {
	if !filepath.IsAbs(p) || !executable(p) {
		return
	}
	binOnce.Do(func() {})
	bin = p
}
