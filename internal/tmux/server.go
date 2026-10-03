package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Server is one tmux server: the binary and the arguments selecting it
// (-L name, -S path).
type Server struct {
	Bin  string
	Args []string
}

// Command is tmux with the server's arguments and args.
func (s Server) Command(args ...string) *exec.Cmd {
	return exec.Command(s.bin(), append(append([]string{}, s.Args...), args...)...)
}

// Run runs one tmux command and returns its stdout. An error carries
// tmux's stderr.
func (s Server) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.bin(), append(append([]string{}, s.Args...), args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), &Error{Msg: msg, Exit: exitCode(err)}
	}
	return out.String(), nil
}

func (s Server) bin() string {
	if s.Bin != "" {
		return s.Bin
	}
	return Bin()
}

// Error is a failed tmux command.
type Error struct {
	Msg  string
	Exit int
}

func (e *Error) Error() string { return "tmux: " + e.Msg }

// NoServer reports whether err says no server is running.
func NoServer(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "no server running") ||
		strings.Contains(m, "error connecting to") ||
		strings.Contains(m, "server exited unexpectedly")
}

func exitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// Fields splits one record of a tab-separated listing into n fields, the
// last taking the rest of the line (a free-text field such as a name goes
// last).
func Fields(line string, n int) ([]string, error) {
	f := strings.SplitN(line, "\t", n)
	if len(f) != n {
		return nil, fmt.Errorf("tmux: %d fields in %q, want %d", len(f), line, n)
	}
	return f, nil
}
