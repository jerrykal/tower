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

// Locate is shell statements that set $b to version's binary on a host,
// under root or the host's own TOWER_INSTALL_DIR when its environment
// sets one (the scenario suite gives each simulated host its own), in a
// directory of the host's platform ($(uname -sm), e.g. Linux-x86_64):
// hosts of two architectures sharing one home directory each get their
// own build. Root's default is a plain assignment, so no character in it
// means something to the shell.
func Locate(root, version string) string {
	var def string
	if rest, ok := strings.CutPrefix(root, "~/"); ok {
		def = `"$HOME"/` + transport.ShellQuote(rest)
	} else if root == "~" {
		def = `"$HOME"`
	} else {
		def = transport.ShellQuote(root)
	}
	return `r=${TOWER_INSTALL_DIR:-}; [ -n "$r" ] || r=` + def +
		`; u=$(uname -sm); p="${u% *}-${u#* }"; b="$r"/` + transport.ShellQuote(version) + `/"$p"/tower`
}

// Command is a shell line on a host running version's binary with args,
// each quoted: by its exact path, never current, so a half-done install
// never runs and two homes of different versions each run their own.
func Command(root, version string, args ...string) string {
	var b strings.Builder
	b.WriteString(Locate(root, version))
	// A missing build is exit 127 (what the link installs on) in every
	// sh: exec of a missing file exits 126 in macOS's.
	b.WriteString(`; [ -x "$b" ] || { echo "tower: $b: not found" >&2; exit 127; }; exec "$b"`)
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
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if _, err := r.Run(ctx, UploadScript(in.Root, in.Version, st.Size()), f); err != nil {
		return fmt.Errorf("cannot install tower %s on %s: %w", in.Version, host, err)
	}
	return nil
}

// Command is a shell line running this version on a host.
func (in *Installer) Command(args ...string) string { return Command(in.Root, in.Version, args...) }

// UploadScript is the one remote command of an upload. The binary comes
// on stdin into a temporary name; it goes in place only if all size bytes
// arrived and it says it is this version (an upload cut short, or a
// binary replaced on the home since towerd started, is refused rather
// than installed). Renames put it and the current symlink in place, so
// two homes installing at once leave one whole binary and a valid
// current. current only moves to a newer version: a home of an older
// one installing there never downgrades the host's tower.
func UploadScript(root, version string, size int64) string {
	v := transport.ShellQuote(version)
	return strings.Join([]string{
		"set -e",
		Locate(root, version),
		`d="$r"/` + v + `/"$p"`,
		`mkdir -p "$d"`,
		`t="$d/.tower.$$"`,
		`trap 'rm -f "$t"' EXIT`,
		`cat > "$t"`,
		`n=$(wc -c < "$t" | tr -d ' ')`,
		`[ "$n" = ` + fmt.Sprint(size) + ` ] || { echo "upload cut short: $n of ` + fmt.Sprint(size) + ` bytes" >&2; exit 1; }`,
		`chmod 755 "$t"`,
		`got=$("$t" version 2>/dev/null || true)`,
		`[ "$got" = ` + v + ` ] || { echo "the uploaded build says it is \"$got\", not ` + version + `" >&2; exit 1; }`,
		`mv -f "$t" "$b"`,
		`trap - EXIT`,
		`c=$(readlink "$r/current" 2>/dev/null || true); c=${c%%/*}`,
		`top=$(printf '%s\n%s\n' "$c" ` + v + ` | sort -V 2>/dev/null | tail -n 1)`,
		`if [ -z "$c" ] || [ -z "$top" ] || [ "$top" = ` + v + ` ]; then`,
		`  ln -sfn ` + v + `/"$p" "$r/.current.$$"`,
		// Rename the link over current without following it (GNU -T, BSD -h).
		`  mv -fT "$r/.current.$$" "$r/current" 2>/dev/null || mv -fh "$r/.current.$$" "$r/current"`,
		`fi`,
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
