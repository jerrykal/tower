package towerd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/jerrykal/tower/internal/tmux"
)

// pathDir is the directory towerd puts on tmux's global PATH: its own
// binary's, when it is fit to be on every pane's PATH (pathFit); else "".
func (d *Daemon) pathDir() string {
	if d.self == "" {
		return ""
	}
	dir := filepath.Dir(d.self)
	if !pathFit(dir) {
		return ""
	}
	return dir
}

// pathFit reports whether dir may go on PATH: absolute, no list
// separator in it (it could never be found or taken out again), and
// owned by this user with no write for group or others, so nobody else
// can put a command there (a build run from /tmp is not).
func pathFit(dir string) bool {
	if !filepath.IsAbs(dir) || strings.ContainsRune(dir, filepath.ListSeparator) {
		return false
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o022 != 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// addPath appends towerd's directory (pathDir) to the server's global
// PATH, which new panes and run-shell start with, where it is not there
// yet: a shell there finds tower even on a host with none of its own,
// and a tower of its own, earlier in PATH, follows towerd's build
// (cmd/tower follow). Appended, not prepended, so shell startup files
// that put their own directories first leave it in place.
//
// kf.Path is the directory a towerd appended. One of another build goes
// first, even when nothing is to be appended now; one already in PATH and
// not recorded (or seeded, see seedPath) is the user's, never recorded,
// so never taken out. On a failed write the record stays as it was, for
// the next towerd. It reports whether PATH has towerd's directory.
func (k *keys) addPath(ctl *tmux.Control, kf *keysFile) bool {
	d := k.w.d
	dir := d.pathDir()
	old, ok := k.globalPath(ctl)
	if !ok {
		return false
	}
	path := old
	if kf.Path != "" && kf.Path != dir {
		path = withoutDir(path, kf.Path)
	}
	d.mu.Lock()
	seeded := d.seededPath
	d.mu.Unlock()
	rec := kf.Path == dir || seeded == dir
	if dir != "" && !slices.Contains(filepath.SplitList(path), dir) {
		if path != "" {
			path += string(filepath.ListSeparator)
		}
		path += dir
		rec = true
	}
	if path != old && !k.setPath(ctl, path) {
		return false
	}
	if dir == "" {
		kf.Path = ""
		return false
	}
	if rec {
		kf.Path = dir
	} else {
		kf.Path = ""
	}
	return true
}

// removePath takes dir, the one towerd recorded, out of the global PATH
// and returns what is left to record: "" once it is gone (or there was
// none), dir when it could not be.
func (k *keys) removePath(ctl *tmux.Control, dir string) string {
	if dir == "" {
		return ""
	}
	path, ok := k.globalPath(ctl)
	if !ok {
		return dir
	}
	if p := withoutDir(path, dir); p != path && !k.setPath(ctl, p) {
		return dir
	}
	return ""
}

// globalPath is the server's global PATH; ok is false when it has none.
func (k *keys) globalPath(ctl *tmux.Control) (string, bool) {
	r, err := ctl.Do("show-environment -g PATH")
	if err != nil || r.Err || len(r.Lines) == 0 {
		return "", false
	}
	return strings.CutPrefix(r.Lines[0], "PATH=")
}

func (k *keys) setPath(ctl *tmux.Control, path string) bool {
	r, err := ctl.Do("set-environment -g PATH " + tmux.Quote(path))
	if err != nil || r.Err {
		k.w.d.logf("keys: set-environment PATH: %s %v", r.Text(), err)
		return false
	}
	return true
}

// seedPath is the environment for a tmux command that starts the server
// (a new session where none runs): towerd's own with its directory at the
// end of PATH, so the server's first pane has it too; addPath then takes
// it as towerd's. TOWER_PATH=0 or no fit directory: towerd's environment
// as it is (nil).
func (d *Daemon) seedPath(pathOn bool) []string {
	dir := d.pathDir()
	if !pathOn || dir == "" {
		return nil
	}
	path := os.Getenv("PATH")
	if !slices.Contains(filepath.SplitList(path), dir) {
		if path != "" {
			path += string(filepath.ListSeparator)
		}
		path += dir
		d.mu.Lock()
		d.seededPath = dir
		d.mu.Unlock()
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "PATH=") })
	return append(env, "PATH="+path)
}

// withoutDir is PATH without dir.
func withoutDir(path, dir string) string {
	parts := slices.DeleteFunc(filepath.SplitList(path), func(p string) bool { return p == dir })
	return strings.Join(parts, string(filepath.ListSeparator))
}
