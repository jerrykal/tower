// Package transport is how tower uses ssh: the options on every call, the
// commands for the stream, attaches, probes and master resets, the
// classification of failures, the control-socket sweep and the network
// watch.
package transport

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/jerrykal/tower/internal/config"
)

// SSH runs ssh with tower's options.
type SSH struct {
	Bin   string // TOWER_SSH, or "ssh"
	CMDir string // control sockets
}

// New returns the SSH tower uses on this machine.
func New(cmDir string) *SSH {
	bin := os.Getenv("TOWER_SSH")
	if bin == "" {
		bin = "ssh"
	}
	return &SSH{Bin: bin, CMDir: cmDir}
}

// Options are the -o settings every call passes: no prompts, a short
// connect timeout, keepalives, a control master per host, and no
// keystroke timing obfuscation (ssh 9.5+ otherwise sends an interactive
// session's keys on a 20ms timer) unless the host keeps it.
func (s *SSH) Options(h config.Host) []string {
	o := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=10m",
		"-o", "ControlPath=" + filepath.Join(s.CMDir, "%C"),
	}
	if !h.ObscureKeystrokes && s.obscureKnown() {
		o = append(o, "-o", "ObscureKeystrokeTiming=no")
	}
	return o
}

// Stream is the command for a home's stream to h: no tty, the remote
// command after "--".
func (s *SSH) Stream(h config.Host, remote string) *exec.Cmd {
	args := append([]string{"-T"}, s.Options(h)...)
	args = append(args, h.Target(), "--", Sh(remote))
	return exec.Command(s.Bin, args...)
}

// Attach is the argv of an interactive session on h (an attach or a
// standby).
func (s *SSH) Attach(h config.Host, remote string) []string {
	args := append([]string{s.Bin, "-t"}, s.Options(h)...)
	return append(args, h.Target(), "--", Sh(remote))
}

// Probe opens a fresh session on h's master and runs true: whether ssh
// still answers when the stream does not.
func (s *SSH) Probe(ctx context.Context, h config.Host) error {
	// The first value of an option wins, so this ControlMaster=no comes
	// before the common ones.
	args := append([]string{"-T", "-o", "ControlMaster=no"}, s.Options(h)...)
	args = append(args, h.Target(), "--", "true")
	cmd := exec.CommandContext(ctx, s.Bin, args...)
	cmd.Stdin = nil
	cmd.WaitDelay = waitDelay
	_, err := output(cmd)
	return err
}

// Exit makes h's control master exit, ending every session on it.
func (s *SSH) Exit(ctx context.Context, h config.Host) error {
	args := append([]string{"-O", "exit"}, s.Options(h)...)
	args = append(args, h.Target())
	cmd := exec.CommandContext(ctx, s.Bin, args...)
	cmd.WaitDelay = waitDelay
	_, err := output(cmd)
	return err
}

// Run runs remote on h with stdin and returns its stdout. A failure is an
// *Error with ssh's exit code and stderr.
func (s *SSH) Run(ctx context.Context, h config.Host, remote string, stdin io.Reader) (string, error) {
	args := append([]string{"-T"}, s.Options(h)...)
	args = append(args, h.Target(), "--", Sh(remote))
	cmd := exec.CommandContext(ctx, s.Bin, args...)
	cmd.Stdin = stdin
	cmd.WaitDelay = waitDelay
	return output(cmd)
}

// On is h's runner: Run without the host argument, the shape an install
// takes.
func (s *SSH) On(h config.Host) HostRunner { return HostRunner{s, h} }

// HostRunner runs commands on one host.
type HostRunner struct {
	s *SSH
	h config.Host
}

// Run runs remote on the host with stdin and returns its stdout.
func (r HostRunner) Run(ctx context.Context, remote string, stdin io.Reader) (string, error) {
	return r.s.Run(ctx, r.h, remote, stdin)
}

// Error is a failed ssh call.
type Error struct {
	Exit   int
	Stderr string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = "exit " + strconv.Itoa(e.Exit)
	}
	return "ssh: " + msg
}

func output(cmd *exec.Cmd) (string, error) {
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		code := -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return out.String(), &Error{Exit: code, Stderr: errb.String()}
	}
	return out.String(), nil
}

var (
	versionMu sync.Mutex
	versions  = map[string]bool{}
)

// obscureKnown reports whether this ssh understands
// ObscureKeystrokeTiming (OpenSSH 9.5 or later), asked once per binary.
func (s *SSH) obscureKnown() bool {
	versionMu.Lock()
	defer versionMu.Unlock()
	if v, ok := versions[s.Bin]; ok {
		return v
	}
	out, _ := exec.Command(s.Bin, "-V").CombinedOutput()
	v := opensshAtLeast(string(out), 9, 5)
	versions[s.Bin] = v
	return v
}

var opensshRE = regexp.MustCompile(`OpenSSH_(\d+)\.(\d+)`)

func opensshAtLeast(banner string, major, minor int) bool {
	m := opensshRE.FindStringSubmatch(banner)
	if m == nil {
		return false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	return a > major || a == major && b >= minor
}

// RemoteCommand is a shell command line for the remote side: the tower
// binary and its arguments, each quoted, so no name or path reaches ssh's
// or the shell's parsing. A binary path starting with ~/ is expanded by
// the remote shell.
func RemoteCommand(tower string, args ...string) string {
	var b strings.Builder
	if rest, ok := strings.CutPrefix(tower, "~/"); ok {
		b.WriteString(`"$HOME"/`)
		b.WriteString(ShellQuote(rest))
	} else {
		b.WriteString(ShellQuote(tower))
	}
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(ShellQuote(a))
	}
	return b.String()
}

// Sh is a remote command line run by sh: ssh hands the line to the
// user's login shell, which may be fish or csh, while tower's lines are
// POSIX shell. `sh -c '<line>'` reads the same in all of them.
func Sh(line string) string { return "sh -c " + ShellQuote(line) }

// WithHome prefixes a remote command line with TOWER_HOME=home (a path
// starting with ~/ is the host's home), or returns it as it is when home
// is empty.
func WithHome(home, line string) string {
	if home == "" {
		return line
	}
	var v string
	if rest, ok := strings.CutPrefix(home, "~/"); ok {
		v = `"$HOME"/` + ShellQuote(rest)
	} else {
		v = ShellQuote(home)
	}
	return "TOWER_HOME=" + v + " " + line
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// ShellQuote quotes s for a POSIX shell. A single quote inside is written
// '"'"' (close, a double-quoted quote, reopen) rather than '\”: fish
// reads a backslash before a quote as an escape even inside single
// quotes, and remote lines pass through the user's login shell (see Sh).
func ShellQuote(s string) string {
	if plainArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
