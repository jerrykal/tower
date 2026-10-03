// Package relay is the terminal side of a remote attach: terminal modes,
// ptys, running ssh on a pty of the loop's own, and relaying the loop's
// terminal to it byte-exact, with the loop's own writes placed between
// escape sequences of the relayed output.
package relay

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The standby markers: the attach shim writes Ready once it waits for its
// go line, and Go once it has read it, just before it starts tmux. Both
// are OSC sequences a terminal ignores; ReadUntil strips them.
const (
	MarkerReady = "\x1b]7193;tower-standby-ready\a"
	MarkerGo    = "\x1b]7193;tower-standby-go\a"
)

// Errors of a session.
var (
	ErrExited  = errors.New("relay: session exited")
	ErrTimeout = errors.New("relay: timed out")
)

// bufSize is the size of the relay's reads.
const bufSize = 32 << 10

// maxBefore bounds the output ReadUntil keeps while it looks for a marker.
const maxBefore = 1 << 20

// Session is a command (ssh) running on a pty of the loop's own: the
// pty's slave is its stdin, stderr and controlling terminal, and its
// stdout is a pipe.
type Session struct {
	cmd    *exec.Cmd
	master int // the pty's master, nonblocking
	out    int // the read end of the stdout pipe, nonblocking
	name   string

	pending []byte // output read past a marker, relayed first

	exitR, exitW int // exitW is closed once the command has exited
	stopR, stopW int // stopW is closed by Terminate and Kill: input stops

	done      chan struct{}
	state     *os.ProcessState
	stopOnce  sync.Once
	closeOnce sync.Once
}

// Start runs argv on a new pty with the given modes and size, set before
// the command starts so that ssh's pty request carries them. env is the
// command's environment (nil: this process's).
func Start(argv []string, env []string, modes Modes, rows, cols int) (*Session, error) {
	if len(argv) == 0 {
		return nil, errors.New("relay: empty command")
	}
	s := &Session{done: make(chan struct{})}
	var fds []int // closed if Start fails
	fail := func(err error) (*Session, error) {
		for _, fd := range fds {
			unix.Close(fd)
		}
		return nil, err
	}
	master, sfd, name, err := openPty()
	if err != nil {
		return nil, err
	}
	s.master, s.name = master, name
	fds = append(fds, master)
	slave := os.NewFile(uintptr(sfd), name)
	defer slave.Close()
	if err := SetModes(sfd, modes); err != nil {
		return fail(fmt.Errorf("relay: pty modes: %w", err))
	}
	if err := SetSize(sfd, rows, cols); err != nil {
		return fail(fmt.Errorf("relay: pty size: %w", err))
	}
	if err := unix.SetNonblock(master, true); err != nil {
		return fail(os.NewSyscallError("fcntl", err))
	}
	outR, outW, err := pipe()
	if err != nil {
		return fail(err)
	}
	s.out = outR
	fds = append(fds, outR)
	w := os.NewFile(uintptr(outW), "|1")
	defer w.Close()
	if s.exitR, s.exitW, err = pipe(); err != nil {
		return fail(err)
	}
	fds = append(fds, s.exitR, s.exitW)
	if s.stopR, s.stopW, err = pipe(); err != nil {
		return fail(err)
	}
	fds = append(fds, s.stopR, s.stopW)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, w, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	s.cmd = cmd
	go func() {
		cmd.Wait()
		s.state = cmd.ProcessState
		close(s.done)
		unix.Close(s.exitW)
	}()
	return s, nil
}

// Pid is the command's process id.
func (s *Session) Pid() int { return s.cmd.Process.Pid }

// PtyName is the path of the session's pty.
func (s *Session) PtyName() string { return s.name }

// Done is closed once the command has exited.
func (s *Session) Done() <-chan struct{} { return s.done }

// ExitCode is the command's exit status once Done: its exit code, or 128
// plus the signal that ended it, as a shell reports it.
func (s *Session) ExitCode() int {
	select {
	case <-s.done:
	default:
		return -1
	}
	if ws, ok := s.state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return s.state.ExitCode()
}

// SetSize sets the size of the session's pty; the command gets SIGWINCH.
func (s *Session) SetSize(rows, cols int) error { return SetSize(s.master, rows, cols) }

// ReadUntil reads the command's output until marker, which it strips,
// and returns the output before it. Output after the marker is kept for
// Relay. It fails with ErrExited when the command exits first, and with
// ErrTimeout after timeout (< 0: none). Not while relaying.
func (s *Session) ReadUntil(marker []byte, timeout time.Duration) ([]byte, error) {
	var end time.Time
	if timeout >= 0 {
		end = time.Now().Add(timeout)
	}
	buf := s.pending
	s.pending = nil
	chunk := make([]byte, 4096)
	exited := false
	for {
		if i := bytes.Index(buf, marker); i >= 0 {
			if rest := buf[i+len(marker):]; len(rest) > 0 {
				s.pending = append([]byte(nil), rest...)
			}
			return buf[:i], nil
		}
		if len(buf) > maxBefore {
			// Keep what could be the start of the marker.
			buf = append(buf[:0], buf[len(buf)-len(marker):]...)
		}
		n, err := readFd(s.out, chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			continue
		}
		switch {
		case err == unix.EAGAIN && exited:
			err = errEOF
		case err == unix.EAGAIN:
			wait := time.Duration(-1)
			if timeout >= 0 {
				if wait = time.Until(end); wait <= 0 {
					s.pending = buf
					return nil, ErrTimeout
				}
			}
			fds := []unix.PollFd{{Fd: int32(s.out), Events: unix.POLLIN}, {Fd: int32(s.exitR), Events: unix.POLLIN}}
			if _, err := poll(fds, wait); err != nil {
				return nil, err
			}
			// Once exited, read on until the pipe is empty.
			exited = fds[1].Revents != 0
			continue
		}
		s.pending = buf
		if !isEOF(err) {
			return nil, err
		}
		// Output has ended: the command is exiting, or has.
		var deadline <-chan time.Time
		if timeout >= 0 {
			timer := time.NewTimer(time.Until(end))
			defer timer.Stop()
			deadline = timer.C
		}
		select {
		case <-s.done:
			return nil, ErrExited
		case <-deadline:
			return nil, ErrTimeout
		}
	}
}

// Send writes line and a newline to the session's terminal, as if typed:
// the go line of a standby.
func (s *Session) Send(line []byte) error {
	b := append(append([]byte(nil), line...), '\n')
	return writeAll(s.master, b, s.exitR)
}

// Terminate sends SIGTERM to the command, and stops a running relay
// taking the terminal's input.
func (s *Session) Terminate() { s.signal(unix.SIGTERM) }

// Kill sends SIGKILL to the command (a stopped process loses a SIGTERM),
// and stops a running relay taking the terminal's input.
func (s *Session) Kill() { s.signal(unix.SIGKILL) }

func (s *Session) signal(sig syscall.Signal) {
	s.stopOnce.Do(func() { unix.Close(s.stopW) })
	s.cmd.Process.Signal(sig)
}

// Close kills the command if it still runs, waits for it, and releases
// the session's descriptors. Not while relaying.
//
// The pty's master closes before the wait: a process exiting closes its
// controlling terminal, which on macOS waits for the terminal's output to
// be read, and outside a relay nobody reads it.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		select {
		case <-s.done:
			unix.Close(s.master)
		default:
			s.Kill()
			unix.Close(s.master)
			<-s.done
		}
		s.stopOnce.Do(func() { unix.Close(s.stopW) })
		for _, fd := range []int{s.out, s.exitR, s.stopR} {
			unix.Close(fd)
		}
	})
}

// Relay relays t to the session until the command exits, and returns its
// exit status (see ExitCode):
//
//   - the terminal's input to the pty, read only when there is some, and
//     not after Terminate or Kill or once Relay returns, so a byte meant
//     for whatever has the terminal next stays in the terminal;
//   - the stdout pipe's and the pty's output to the terminal, with the
//     loop's writes (t.Write) put in between sequences;
//   - the terminal's window size to the pty, at the start and on every
//     SIGWINCH.
//
// The terminal's modes are the caller's: Relay neither makes it raw nor
// restores it. A terminal that stops reading stops Relay reading, and so
// the command's writes. An error writing to the terminal ends Relay with
// the command still running.
func (s *Session) Relay(t *Terminal) (int, error) {
	wakeR, err := t.begin()
	if err != nil {
		return -1, err
	}
	defer t.end(wakeR)
	quitR, quitW, err := pipe()
	if err != nil {
		return -1, err
	}
	defer unix.Close(quitR)

	syncSize := func() {
		if rows, cols, err := GetSize(t.out); err == nil {
			s.SetSize(rows, cols)
		}
	}
	syncSize()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, unix.SIGWINCH)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-winch:
				syncSize()
			case <-stop:
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		s.input(t, quitR)
	}()

	err = s.output(t, wakeR)
	close(stop)
	unix.Close(quitW)
	signal.Stop(winch)
	wg.Wait()
	if err != nil {
		return -1, err
	}
	return s.ExitCode(), nil
}
