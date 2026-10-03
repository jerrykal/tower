// Package loop is the attach loop (tower outside tmux), the attach shim
// (tower attach) and the loop's standby sessions.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/relay"
	"github.com/jerrykal/tower/internal/tmux"
)

// The markers a standby shim writes on its pty are the relay's: ready
// for the go line (relay.MarkerReady), and the go line taken
// (relay.MarkerGo). The loop strips both.

// standbyLife is how long a standby nobody used waits before it exits.
const standbyLife = 12 * time.Hour

// Shim is one attach: who it belongs to and what it attaches to.
type Shim struct {
	Loop    string
	Gen     int
	Home    string
	Inst    string
	MKey    string
	Tmux    []string // nil: TOWER_TMUX
	Session string
	Window  string
	Standby bool
	Version string
	Note    string // shown in the new client's status line as it attaches
}

// RunShim registers the attach with the towerd here and execs the tmux
// client, so the registered pid is the client's. As a standby it first
// waits on its pty for the go line that names the attach. It returns
// only on failure.
func RunShim(s Shim, tty *os.File) error {
	if s.Standby {
		c, err := ensure(s.Tmux, s.MKey, s.Version)
		if err != nil {
			return err
		}
		g, err := waitGo(tty, os.Stdout)
		if err != nil {
			return err
		}
		s.Loop, s.Gen, s.Home, s.Inst, s.Session, s.Window = g.Loop, g.Gen, g.Home, g.Inst, g.Session, g.Window
		if g.MKey != "" {
			s.MKey = g.MKey
		}
		if g.Note != "" {
			s.Note = g.Note
		}
		config.Mark("standby: taken")
		return attach(c, s)
	}
	return attach(nil, s)
}

// ensure makes sure the towerd here answers. A towerd the shim starts is
// remote-only: an attach never makes a machine a home.
func ensure(tmuxArgs []string, mkey, version string) (*client.Client, error) {
	if config.ValidMKey(mkey) {
		os.Setenv("TOWER_MKEY", mkey)
	}
	env, err := config.Load(tmuxArgs)
	if err != nil {
		return nil, err
	}
	c := client.New(env)
	if version != "" {
		c.Version = version
	}
	c.Bridged = true
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	st, err := c.Ensure(ctx)
	if err != nil {
		return nil, err
	}
	if st.MKey != env.MKey {
		// The key passed down was not this machine's: use the towerd's.
		os.Setenv("TOWER_MKEY", st.MKey)
		if env, err = config.Load(tmuxArgs); err != nil {
			return nil, err
		}
		c.Env = env
	}
	tmux.UseBin(st.TmuxBin)
	return c, nil
}

// attach registers and execs tmux. With a machine key passed down and a
// towerd answering under it with that key, the registration is the only
// call; otherwise the towerd is ensured first.
func attach(c *client.Client, s Shim) error {
	args := proto.RegisterArgs{Pid: os.Getpid(), Loop: s.Loop, Gen: s.Gen, Home: s.Home, Inst: s.Inst}
	var reg proto.Registered
	registered := false
	if c == nil && config.ValidMKey(s.MKey) {
		os.Setenv("TOWER_MKEY", s.MKey)
		if env, err := config.Load(s.Tmux); err == nil {
			fast := client.New(env)
			if s.Version != "" {
				fast.Version = s.Version
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := fast.Call(ctx, proto.CallRegister, args, &reg)
			cancel()
			if err == nil && reg.MKey == s.MKey {
				registered, c = true, fast
			}
		}
		if !registered {
			os.Unsetenv("TOWER_MKEY")
		}
	}
	if !registered {
		var err error
		if c == nil {
			if c, err = ensure(s.Tmux, "", s.Version); err != nil {
				return err
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = c.Call(ctx, proto.CallRegister, args, &reg)
		cancel()
		if err != nil {
			return fmt.Errorf("register with towerd: %w", err)
		}
	}
	tmux.UseBin(reg.TmuxBin)
	bin := tmux.Bin()
	argv := append([]string{bin}, c.Env.Tmux...)
	argv = append(argv, AttachCommand(s.Session, s.Window, s.Inst, s.Note)...)
	config.Mark("shim: exec tmux")
	env := os.Environ()
	env = filterEnv(env, "TOWER_CLIENT", "TOWER_MKEY")
	return syscall.Exec(bin, argv, env)
}

// noteTime is how long an attach's note stays in the client's status
// line (ms).
const noteTime = "4000"

// AttachCommand is the tmux command list of an attach: by id, so a
// session killed meanwhile fails visibly; the instance check right
// after, since tmux skips the rest of a list after a failing command (a
// restarted server that reused the id detaches at once with exit 43); the
// window selected by id, so base-index never matters; and the loop's
// note, if any, in the new client's status line.
func AttachCommand(session, window, inst, note string) []string {
	a := []string{"attach-session", "-t", session}
	if inst != "" {
		a = append(a, ";", "if-shell", "-F", "#{!=:#{pid}:#{start_time},"+inst+"}", "detach-client -E 'exit 43'")
	}
	if window != "" {
		a = append(a, ";", "select-window", "-t", window)
	}
	if note != "" {
		a = append(a, ";", "display-message", "-d", noteTime, tmux.Literal(note))
	}
	return a
}

func filterEnv(env []string, drop ...string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		keep := true
		for _, d := range drop {
			if k == d {
				keep = false
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}

// waitGo turns the pty's echo and line editing off, says it is ready
// on out (where the attach's output goes: the pty, through ssh),
// reads the go line a byte at a time (nothing typed after it is taken
// from tmux), restores the modes and answers.
func waitGo(tty *os.File, out io.Writer) (*proto.GoLine, error) {
	fd := int(tty.Fd())
	old, err := getTermios(fd)
	if err != nil {
		return nil, fmt.Errorf("standby: %w", err)
	}
	raw := *old
	rawMode(&raw)
	if err := setTermios(fd, &raw); err != nil {
		return nil, fmt.Errorf("standby: %w", err)
	}
	restore := func() { setTermios(fd, old) }
	io.WriteString(out, relay.MarkerReady)
	config.Mark("standby: waiting")
	timer := time.AfterFunc(standbyLife, func() {
		restore()
		os.Exit(0)
	})
	line, err := readLine(tty)
	timer.Stop()
	restore()
	if err != nil {
		return nil, err
	}
	var g proto.GoLine
	if err := json.Unmarshal(line, &g); err != nil {
		return nil, fmt.Errorf("standby: bad go line: %w", err)
	}
	io.WriteString(out, relay.MarkerGo)
	return &g, nil
}

// readLine reads up to a newline one byte at a time.
func readLine(r io.Reader) ([]byte, error) {
	var line []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			if b[0] == '\n' || b[0] == '\r' {
				if len(line) == 0 {
					continue
				}
				return line, nil
			}
			line = append(line, b[0])
			if len(line) > 1<<16 {
				return nil, errors.New("standby: go line too long")
			}
		}
		if err != nil {
			return nil, err
		}
	}
}
