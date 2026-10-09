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
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The standby markers: the attach shim writes Ready once it waits for its
// go line, and Go once it has read it, just before it starts tmux. A shim
// whose client can be detached back into a standby writes Again before
// Ready; the standby it becomes writes the ended marker first:
// MarkerEnded, the go line's nonce and BEL. All are OSC sequences a
// terminal ignores; ReadUntil strips them.
const (
	MarkerReady = "\x1b]7193;tower-standby-ready\a"
	MarkerGo    = "\x1b]7193;tower-standby-go\a"
	MarkerAgain = "\x1b]7193;tower-standby-again\a"
	MarkerEnded = "\x1b]7193;tower-standby-ended;"
)

// Ended is the ended marker of the attach whose go line carried nonce.
func Ended(nonce string) string { return MarkerEnded + nonce + "\a" }

// Errors of a session.
var (
	ErrExited  = errors.New("relay: session exited")
	ErrTimeout = errors.New("relay: timed out")
	// ErrAbandoned is a relay ended by Abandon.
	ErrAbandoned = errors.New("relay: abandoned")
	// ErrReleased is a relay ended by Release: the session runs on.
	ErrReleased = errors.New("relay: released")
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

	pending []byte       // output read past a marker, relayed first
	relayed atomic.Int64 // bytes of output the current or last relay showed

	mu    sync.Mutex
	armed bool // Reuse was called and that relay has not ended
	relW  int  // the running relay's release pipe; -1: none
	early bool // released before the armed relay began

	exitR, exitW int // exitW is closed once the command has exited
	stopR, stopW int // stopW is closed by Terminate and Kill: input stops
	quitR, quitW int // quitW is closed by Abandon: the relay ends

	done      chan struct{}
	state     *os.ProcessState
	stopOnce  sync.Once
	quitOnce  sync.Once
	closeOnce sync.Once
}

// Start runs argv on a new pty with the given modes and size, set before
// the command starts so that ssh's pty request carries them. env is the
// command's environment (nil: this process's).
func Start(argv []string, env []string, modes Modes, rows, cols int) (*Session, error) {
	if len(argv) == 0 {
		return nil, errors.New("relay: empty command")
	}
	s := &Session{done: make(chan struct{}), relW: -1}
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
	if s.quitR, s.quitW, err = pipe(); err != nil {
		return fail(err)
	}
	fds = append(fds, s.quitR, s.quitW)

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

// Relayed counts the bytes of the command's output the current (or last)
// relay showed: none means the far side has drawn nothing in it.
func (s *Session) Relayed() int64 { return s.relayed.Load() }

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

// Abandon ends a running relay at once, without waiting for the command
// to exit: nothing more it writes reaches the terminal, and the terminal's
// input stays where it is. Relay returns ErrAbandoned. For a session ended
// for a switch: the next client need not wait for ssh's goodbye.
func (s *Session) Abandon() {
	s.stopOnce.Do(func() { unix.Close(s.stopW) })
	s.quitOnce.Do(func() { unix.Close(s.quitW) })
}

// Reuse arms the next relay of a session kept for another attach:
// Release can end it. Call it before that relay starts.
func (s *Session) Reuse() {
	s.mu.Lock()
	s.armed, s.early = true, false
	s.mu.Unlock()
}

// Release ends the relay Reuse armed at once, running or about to run,
// and leaves the command be: nothing more it writes reaches the terminal,
// and the terminal's input stays where it is. That relay returns
// ErrReleased. Once it has returned, Release does nothing.
func (s *Session) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.relW >= 0:
		unix.Close(s.relW)
		s.relW = -1
	case s.armed:
		s.early = true
	}
}

// Drain discards the command's output up to the ended marker carrying
// nonce: the released client's last output. Output after it is kept.
// Not while relaying.
func (s *Session) Drain(nonce string, timeout time.Duration) error {
	_, err := s.ReadUntil([]byte(Ended(nonce)), timeout)
	return err
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
		s.quitOnce.Do(func() { unix.Close(s.quitW) })
		for _, fd := range []int{s.out, s.exitR, s.stopR, s.quitR} {
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
//
// A relay armed by Reuse also ends at Release (ErrReleased), the command
// running on.
func (s *Session) Relay(t *Terminal) (int, error) {
	relR, err := s.startRelay()
	if err != nil {
		return -1, err
	}
	defer s.endRelay(relR)
	s.relayed.Store(0)
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
		s.input(t, quitR, relR)
	}()

	err = s.output(t, wakeR, relR)
	close(stop)
	unix.Close(quitW)
	signal.Stop(winch)
	wg.Wait()
	if err != nil {
		return -1, err
	}
	return s.ExitCode(), nil
}

// startRelay begins a relay: its release pipe's read end (-1 for a relay
// not armed). An armed relay released before it began ends at once.
func (s *Session) startRelay() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.armed {
		return -1, nil
	}
	if s.early {
		s.armed, s.early = false, false
		return -1, ErrReleased
	}
	r, w, err := pipe()
	if err != nil {
		s.armed = false
		return -1, err
	}
	s.relW = w
	return r, nil
}

func (s *Session) endRelay(relR int) {
	s.mu.Lock()
	if s.relW >= 0 {
		unix.Close(s.relW)
		s.relW = -1
	}
	s.armed, s.early = false, false
	s.mu.Unlock()
	if relR >= 0 {
		unix.Close(relR)
	}
}
