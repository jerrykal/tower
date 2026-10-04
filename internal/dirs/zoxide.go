package dirs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/tmux"
)

// Query lists zoxide's directories, most frecent first. --all keeps
// zoxide from checking that each exists (a stat per entry, network
// mounts included); the caller checks the local ones itself.
func Query(ctx context.Context, zoxide string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, zoxide, "query", "--list", "--all")
	cmd.WaitDelay = time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("zoxide took too long")
		}
		// An empty database is an error to zoxide ("no match found").
		if out.Len() == 0 && strings.Contains(errb.String(), "no match") {
			return nil, nil
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	var dirs []string
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if p := sc.Text(); filepath.IsAbs(p) {
			dirs = append(dirs, filepath.Clean(p))
		}
	}
	return dirs, sc.Err()
}

// FindZoxide is the zoxide binary, past version-manager shims, or "".
func FindZoxide() string { return find("zoxide") }

// FindGit is the git binary, past version-manager shims, or "". On macOS
// /usr/bin/git is a stub that opens an installer dialog when the command
// line tools are missing; it is used only when they are there.
func FindGit() string {
	p := find("git")
	if p == "/usr/bin/git" && runtime.GOOS == "darwin" {
		if err := exec.Command("/usr/bin/xcode-select", "-p").Run(); err != nil {
			return ""
		}
	}
	return p
}

func find(name string) string {
	p := tmux.Resolve(name)
	if !filepath.IsAbs(p) {
		return ""
	}
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		return ""
	}
	return p
}

// Short spells path as the dashboard shows it: ~ for home.
func Short(path, home string) string {
	if home == "" || home == "/" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home) && path[len(home)] == '/' {
		return "~" + path[len(home):]
	}
	return path
}

// Expand turns a leading ~ (alone or before a slash) into home and cleans
// the result. Other paths come back cleaned; a relative one is taken from
// home.
func Expand(path, home string) string {
	switch {
	case path == "~":
		return filepath.Clean(home)
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:])
	case path == "":
		return ""
	case !filepath.IsAbs(path) && home != "":
		return filepath.Join(home, path)
	}
	return filepath.Clean(path)
}
