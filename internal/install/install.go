// Package install puts the home's tower build on another host over the
// ssh master the home already has: the host's platform from uname, the
// build for it (this binary, the dist cache, or a verified download of
// the release), and one upload session that renames everything into
// place. Remote hosts never need GitHub or a package manager.
package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jerrykal/tower/internal/transport"
)

// DefaultRoot is where builds live on a host: <root>/<version>/tower, and
// <root>/current pointing at the newest installed.
const DefaultRoot = "~/.local/share/tower"

// Runner runs one remote command on a host (an ssh session on its
// master), with stdin, and returns its stdout.
type Runner interface {
	Run(ctx context.Context, remote string, stdin io.Reader) (string, error)
}

// Root is the install root the home uses for every host: TOWER_INSTALL_DIR,
// else DefaultRoot. A path may start with ~/ (the host's home).
func Root() string {
	if v := os.Getenv("TOWER_INSTALL_DIR"); v != "" {
		return v
	}
	return DefaultRoot
}

// rootExpr is the root as a word for the host's shell: the host's own
// TOWER_INSTALL_DIR when its environment sets one (the scenario suite
// gives every simulated host its own), else root, with ~/ as $HOME.
func rootExpr(root string) string {
	var b strings.Builder
	b.WriteString(`"${TOWER_INSTALL_DIR:-`)
	if rest, ok := strings.CutPrefix(root, "~/"); ok {
		b.WriteString("$HOME/")
		root = rest
	} else if root == "~" {
		b.WriteString("$HOME")
		root = ""
	}
	for _, r := range root {
		if strings.ContainsRune("\\\"$`}", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteString(`}"`)
	return b.String()
}

// Path is the shell word for version's binary under root on a host.
func Path(root, version string) string {
	return rootExpr(root) + "/" + transport.ShellQuote(version) + "/tower"
}

// Command is a shell line on a host running version's binary with args,
// each quoted: by its exact path, never current, so a half-done install
// never runs and two homes of different versions each run their own.
func Command(root, version string, args ...string) string {
	var b strings.Builder
	b.WriteString(Path(root, version))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(transport.ShellQuote(a))
	}
	return b.String()
}

// Platform is a build target.
type Platform struct{ OS, Arch string }

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Local is this binary's platform.
func Local() Platform { return Platform{runtime.GOOS, runtime.GOARCH} }

// ParsePlatform reads `uname -s -m` ("Linux x86_64", "Darwin arm64").
func ParsePlatform(uname string) (Platform, error) {
	f := strings.Fields(uname)
	if len(f) != 2 {
		return Platform{}, fmt.Errorf("unsupported platform %q", strings.TrimSpace(uname))
	}
	var p Platform
	switch f[0] {
	case "Linux":
		p.OS = "linux"
	case "Darwin":
		p.OS = "darwin"
	}
	switch f[1] {
	case "x86_64", "amd64":
		p.Arch = "amd64"
	case "aarch64", "arm64":
		p.Arch = "arm64"
	}
	if p.OS == "" || p.Arch == "" {
		return Platform{}, fmt.Errorf("unsupported platform %s %s", f[0], f[1])
	}
	return p, nil
}

// Installer installs one version of tower on hosts.
type Installer struct {
	Version string
	Root    string // install root on hosts; empty: Root()
	Source  *Source
}

// New is the installer of this build (self is its binary).
func New(version, self string) *Installer {
	return &Installer{Version: version, Root: Root(), Source: NewSource(version, self)}
}

// Install puts the build on the host r reaches (named host in messages):
// its platform, the build for it, then the upload.
func (in *Installer) Install(ctx context.Context, r Runner, host string) error {
	out, err := r.Run(ctx, "uname -s -m", nil)
	if err != nil {
		return fmt.Errorf("cannot install tower on %s: uname: %w", host, err)
	}
	p, err := ParsePlatform(out)
	if err != nil {
		return fmt.Errorf("cannot install tower on %s: %w", host, err)
	}
	bin, err := in.Source.Binary(ctx, p)
	if err != nil {
		return err
	}
	f, err := os.Open(bin)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := r.Run(ctx, UploadScript(in.root(), in.Version), f); err != nil {
		return fmt.Errorf("cannot install tower %s on %s: %w", in.Version, host, err)
	}
	return nil
}

func (in *Installer) root() string {
	if in.Root != "" {
		return in.Root
	}
	return Root()
}

// Path is the shell word for this version's binary on a host.
func (in *Installer) Path() string { return Path(in.root(), in.Version) }

// Command is a shell line running this version on a host.
func (in *Installer) Command(args ...string) string { return Command(in.root(), in.Version, args...) }

// UploadScript is the one remote command of an upload: the binary comes
// on stdin into a temporary name, and renames put it and the current
// symlink in place, so two homes installing at once leave one whole
// binary and a valid current.
func UploadScript(root, version string) string {
	v := transport.ShellQuote(version)
	return strings.Join([]string{
		"set -e",
		"r=" + rootExpr(root),
		`mkdir -p "$r"/` + v,
		`t="$r"/` + v + `/.tower.$$`,
		`cat > "$t"`,
		`chmod 755 "$t"`,
		`mv -f "$t" "$r"/` + v + `/tower`,
		`ln -sfn ` + v + ` "$r/.current.$$"`,
		// Rename the link over current without following it (GNU -T, BSD -h).
		`mv -fT "$r/.current.$$" "$r/current" 2>/dev/null || mv -fh "$r/.current.$$" "$r/current"`,
	}, "\n")
}

// cacheDir is the dist cache of version: TOWER_DIST_DIR (else
// ~/.cache/tower/dist) / version.
func cacheDir(version string) string {
	base := os.Getenv("TOWER_DIST_DIR")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache", "tower", "dist")
	}
	return filepath.Join(base, version)
}
