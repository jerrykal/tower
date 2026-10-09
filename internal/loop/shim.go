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
// (relay.MarkerGo); before ready, that its client can be detached back
// into a standby (relay.MarkerAgain), and as one, that it was
// (relay.Ended). The loop strips them.

// standbyLife is how long a standby nobody used waits before it exits.
const standbyLife = 12 * time.Hour

// standbyExit ends a standby nobody will use (a variable for tests).
var standbyExit = func() { os.Exit(0) }

// standbySilence is how long a standby waits without its loop's
// heartbeat (an empty line every standbyRefresh) before it exits.
const standbySilence = 30 * time.Second

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
	Pane    string // "%1": the pane picked in the dashboard, selected after the window
	Standby bool
	Version string
	Note    string // shown in the new client's status line as it attaches
	Ended   string // a standby detached from an attach: that attach's nonce
	Again   string // the attach's nonce: its client can be detached back into a standby
}

// RunShim registers the attach with the towerd here and execs the tmux
// client, so the registered pid is the client's. As a standby it first
// waits on its pty for the go line that names the attach. It returns only
// on failure.
//
// A go line with a nonce (Again) registers the client with it and with
// the command that makes it a standby again: a detach for a switch runs
// that in the client's place (detach-client -E), so the session stays for
// the next attach. That standby (Ended) writes the ended marker before it
// says it is ready.
func RunShim(s Shim, tty *os.File) error {
	if !s.Standby {
		return attach(nil, s)
	}
	c, err := ensure(s.Tmux, s.MKey, s.Version)
	if err != nil {
		return err
	}
	again := config.Flag("TOWER_REUSE", true) && resumeCommand(s, "0") != ""
	g, err := waitGo(tty, os.Stdout, s.Ended, again)
	if err != nil {
		return err
	}
	a := s
	a.Loop, a.Gen, a.Home, a.Inst, a.Session, a.Window, a.Pane = g.Loop, g.Gen, g.Home, g.Inst, g.Session, g.Window, g.Pane
	if g.MKey != "" {
		a.MKey = g.MKey
	}
	if g.Note != "" {
		a.Note = g.Note
	}
	if again && validNonce(g.Again) {
		a.Again = g.Again
	}
	config.Mark("standby: taken")
	return attach(c, a)
}

// validNonce reports whether a go line's nonce is one the home makes
// (lower-case hex), so it goes into a marker and a command as it is.
func validNonce(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, c := range n {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// resumeCommand is the shell command that turns an attach's client back
// into a standby that writes nonce's ended marker. tmux runs it with the
// session's default-shell, so every word is single-quoted, and a word
// that quoting could not keep whole in every shell (a quote or a
// backslash) means none.
func resumeCommand(s Shim, nonce string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	words := []string{exe, "attach", "--standby", "--ended", nonce}
	if s.MKey != "" {
		words = append(words, "--mkey", s.MKey)
	}
	if s.Tmux != nil {
		words = append(words, "--tmux", strings.Join(s.Tmux, " "))
	}
	var b strings.Builder
	b.WriteString("exec")
	for _, w := range words {
		if strings.ContainsAny(w, "'\\") {
			return ""
		}
		b.WriteString(" '" + w + "'")
	}
	return b.String()
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
	if s.Again != "" {
		args.Again, args.Resume = s.Again, resumeCommand(s, s.Again)
	}
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
	argv = append(argv, AttachCommand(s.Session, s.Window, s.Pane, s.Inst, s.Note)...)
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
// window selected by id, so base-index never matters, then the pane
// picked, if any; and the loop's note, if any, in the new client's status
// line.
func AttachCommand(session, window, pane, inst, note string) []string {
	a := []string{"attach-session", "-t", session}
	if inst != "" {
		a = append(a, ";", "if-shell", "-F", "#{!=:#{pid}:#{start_time},"+inst+"}", "detach-client -E 'exit 43'")
	}
	if window != "" {
		a = append(a, ";", "select-window", "-t", window)
	}
	if pane != "" {
		a = append(a, ";", "select-pane", "-t", pane)
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

// waitGo turns the pty's echo, line editing and signal keys off,
// discarding input typed before, and says on out (where the attach's
// output goes: the pty, through ssh), in one write, that the attach it
// was detached from ended (ended: its nonce), that it can go again (if
// again), and that it is ready. It reads the go line a byte at a time
// (nothing typed after it is taken from tmux), restores the modes and
// answers.
func waitGo(tty *os.File, out io.Writer, ended string, again bool) (*proto.GoLine, error) {
	fd := int(tty.Fd())
	old, err := getTermios(fd)
	if err != nil {
		return nil, fmt.Errorf("standby: %w", err)
	}
	raw := *old
	rawMode(&raw)
	if err := setTermiosFlush(fd, &raw); err != nil {
		return nil, fmt.Errorf("standby: %w", err)
	}
	restore := func() { setTermios(fd, old) }
	var say string
	if ended != "" {
		say = relay.Ended(ended)
	}
	if again {
		say += relay.MarkerAgain
	}
	io.WriteString(out, say+relay.MarkerReady)
	config.Mark("standby: waiting")
	timer := time.AfterFunc(standbyLife, func() {
		restore()
		os.Exit(0)
	})
	// The loop sends an empty line every few seconds while it keeps this
	// standby. Silence means the loop is gone: a session's hang-up does
	// not always reach here (Tailscale SSH keeps the pty of a session
	// whose client went away), and a shim nobody will use must not wait
	// out its 12 hours.
	silence := config.Duration("TOWER_STANDBY_SILENCE", standbySilence)
	idle := time.AfterFunc(silence, func() {
		restore()
		standbyExit()
	})
	defer func() {
		idle.Stop()
		timer.Stop()
		restore()
	}()
	for {
		line, err := readLine(tty, func() { idle.Reset(silence) })
		if err != nil {
			return nil, err
		}
		// A line that is not a go line is input typed for an earlier
		// client that came too late for it (the loop sends a newline
		// before a go line, to end any).
		var g proto.GoLine
		if json.Unmarshal(line, &g) != nil || g.Loop == "" || g.Session == "" {
			config.Mark("standby: not a go line")
			continue
		}
		io.WriteString(out, relay.MarkerGo)
		return &g, nil
	}
}

// readLine reads up to a newline one byte at a time; heard is called for
// every byte (the loop's heartbeats are empty lines).
func readLine(r io.Reader, heard func()) ([]byte, error) {
	var line []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			if heard != nil {
				heard()
			}
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
