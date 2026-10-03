package relay

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// collector collects what the terminal emulator side of a test terminal
// receives, without ever making the terminal wait.
type collector struct {
	mu     sync.Mutex
	buf    []byte
	closed bool
	more   chan struct{} // a token when buf grew or the terminal closed
}

func collect(tt *testTerminal) *collector {
	c := &collector{more: make(chan struct{}, 1)}
	go func() {
		b := make([]byte, 64<<10)
		for {
			n, err := unix.Read(tt.mfd, b)
			if err == unix.EINTR {
				continue
			}
			c.mu.Lock()
			c.buf = append(c.buf, b[:max(n, 0)]...)
			c.closed = err != nil || n == 0
			closed := c.closed
			c.mu.Unlock()
			select {
			case c.more <- struct{}{}:
			default:
			}
			if closed {
				return
			}
		}
	}()
	return c
}

// take waits until done(buf) gives the length to take from the front of
// the received bytes, and takes it.
func (c *collector) take(t *testing.T, d time.Duration, what string, done func([]byte) int) []byte {
	t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		c.mu.Lock()
		n := done(c.buf)
		if n >= 0 {
			got := slices.Clone(c.buf[:n])
			c.buf = c.buf[n:]
			c.mu.Unlock()
			return got
		}
		closed, have := c.closed, c.buf
		c.mu.Unlock()
		if closed {
			t.Fatalf("terminal closed waiting for %s; got %d bytes: %.200q", what, len(have), have)
		}
		select {
		case <-c.more:
		case <-timer.C:
			c.mu.Lock()
			defer c.mu.Unlock()
			t.Fatalf("no %s within %v; got %d bytes: %.200q", what, d, len(c.buf), c.buf)
		}
	}
}

// until reads until the received bytes contain want, and returns them
// up to its end.
func (c *collector) until(t *testing.T, want string, d time.Duration) string {
	t.Helper()
	return string(c.take(t, d, fmt.Sprintf("%q", want), func(b []byte) int {
		if i := bytes.Index(b, []byte(want)); i >= 0 {
			return i + len(want)
		}
		return -1
	}))
}

// exactly reads n bytes.
func (c *collector) exactly(t *testing.T, n int, d time.Duration) []byte {
	t.Helper()
	return c.take(t, d, fmt.Sprintf("%d bytes", n), func(b []byte) int {
		if len(b) >= n {
			return n
		}
		return -1
	})
}

func sh(script string) []string { return []string{"/bin/sh", "-c", script} }

func TestStartModesAndSize(t *testing.T) {
	modes := cookedModes(t)
	modes.t.Lflag &^= unix.ECHO
	s, err := Start(sh("stty -a; stty size"), nil, modes, 33, 101)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, err := s.ReadUntil([]byte("33 101"), 5*time.Second)
	if err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if !strings.Contains(string(out), "-echo ") {
		t.Fatalf("echo not off on the session's pty: %q", out)
	}
	<-s.Done()
	if s.ExitCode() != 0 {
		t.Fatalf("exit %d", s.ExitCode())
	}
}

func TestStartControllingTerminal(t *testing.T) {
	// The pty is the session's controlling terminal and stderr; stdout is
	// not the pty.
	s := startSession(t, sh(`test -t 0 && test -t 2 && ! test -t 1 && ps -o tty= -p $$`), nil, cookedModes(t))
	out, err := s.ReadUntil([]byte("\n"), 5*time.Second)
	if err != nil {
		t.Fatalf("%v %q", err, out)
	}
	if name := strings.TrimPrefix(s.PtyName(), "/dev/"); !strings.Contains(string(out), strings.TrimPrefix(name, "tty")) {
		t.Fatalf("controlling terminal %q, pty %q", out, s.PtyName())
	}
}

func TestReadUntil(t *testing.T) {
	s := startSession(t, sh(`stty -echo; printf 'before'; printf '\033]7193;tower-standby-ready\007after'; IFS= read -r l; printf "<%s>" "$l"; printf '\033]7193;tower-standby-go\007'; sleep 0.2; printf tail`), nil, cookedModes(t))
	before, err := s.ReadUntil([]byte(MarkerReady), 5*time.Second)
	if err != nil || string(before) != "before" {
		t.Fatalf("ready: %q %v", before, err)
	}
	if _, err := s.ReadUntil([]byte(MarkerGo), 100*time.Millisecond); err != ErrTimeout {
		t.Fatalf("go before the line: %v", err)
	}
	if err := s.Send([]byte(`{"s":"$0"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadUntil([]byte(MarkerGo), 5*time.Second)
	if err != nil || !strings.HasPrefix(string(got), "after") || !strings.HasSuffix(string(got), `<{"s":"$0"}>`) {
		t.Fatalf("go: %q %v", got, err)
	}
	tt := newTestTerminal(t, 24, 80)
	c := collect(tt)
	res := <-relay(s, tt.Terminal)
	if res.err != nil || res.code != 0 {
		t.Fatalf("relay: %+v", res)
	}
	if got := c.until(t, "tail", 2*time.Second); got != "tail" {
		t.Fatalf("relayed %q", got)
	}
	if _, err := s.ReadUntil([]byte("x"), time.Second); err != ErrExited {
		t.Fatalf("after exit: %v", err)
	}
}

func TestReadUntilExit(t *testing.T) {
	s := startSession(t, sh(`printf partial; exit 3`), nil, cookedModes(t))
	start := time.Now()
	if _, err := s.ReadUntil([]byte(MarkerReady), 10*time.Second); err != ErrExited {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("waited for the timeout after the exit")
	}
	if s.ExitCode() != 3 {
		t.Fatalf("exit %d", s.ExitCode())
	}
}

func TestRelayExitStatus(t *testing.T) {
	for _, c := range []struct {
		name   string
		script string
		end    func(*Session)
		want   int
	}{
		{"exit 0", "exit 0", nil, 0},
		{"exit 42", "exit 42", nil, 42},
		{"exit 255", "exit 255", nil, 255},
		{"terminate", "sleep 30", (*Session).Terminate, 128 + int(syscall.SIGTERM)},
		{"kill", "trap '' TERM; sleep 30", (*Session).Kill, 128 + int(syscall.SIGKILL)},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := startSession(t, sh(c.script), nil, cookedModes(t))
			tt := newTestTerminal(t, 24, 80)
			collect(tt)
			ch := relay(s, tt.Terminal)
			if c.end != nil {
				time.Sleep(50 * time.Millisecond)
				c.end(s)
			}
			select {
			case res := <-ch:
				if res.err != nil || res.code != c.want {
					t.Fatalf("relay: %+v, want %d", res, c.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("relay did not end")
			}
		})
	}
}

func TestRelayStderrThroughPty(t *testing.T) {
	// ssh writes its own messages to its terminal; they reach the terminal
	// even when written right before the exit.
	s := startSession(t, sh(`printf out; printf 'Connection closed.' >&2; exit 255`), nil, cookedModes(t))
	tt := newTestTerminal(t, 24, 80)
	c := collect(tt)
	if res := <-relay(s, tt.Terminal); res.code != 255 {
		t.Fatalf("relay: %+v", res)
	}
	c.until(t, "Connection closed.", 2*time.Second)
}

func TestRelayLeavesInputAlone(t *testing.T) {
	// Keys typed once the relay has stopped, or after Terminate, stay in
	// the terminal for whatever has it next.
	s := startSession(t, sh(`trap '' TERM; stty raw -echo; printf R; sleep 0.4`), nil, cookedModes(t))
	tt := newTestTerminal(t, 24, 80)
	c := collect(tt)
	ch := relay(s, tt.Terminal)
	c.until(t, "R", 5*time.Second)
	s.Terminate() // ignored: the session runs on, the input stops
	time.Sleep(20 * time.Millisecond)
	if _, err := unix.Write(tt.mfd, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if res := <-ch; res.code != 0 {
		t.Fatalf("relay: %+v", res)
	}
	if _, err := unix.Write(tt.mfd, []byte("def")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 6)
	if _, err := io.ReadFull(tt.pty.Slave, got); err != nil || string(got) != "abcdef" {
		t.Fatalf("terminal input %q %v", got, err)
	}
}

func TestRelayWindowSize(t *testing.T) {
	s := startSession(t, sh(`sleep 30`), nil, cookedModes(t))
	tt := newTestTerminal(t, 50, 132)
	collect(tt)
	ch := relay(s, tt.Terminal)
	size := func(r, c int) func() bool {
		return func() bool {
			rows, cols, err := GetSize(s.master)
			return err == nil && rows == r && cols == c
		}
	}
	if !waitFor(2*time.Second, size(50, 132)) {
		t.Fatal("the pty did not take the terminal's size at the start")
	}
	if err := SetSize(fdOf(t, tt.pty.Slave), 40, 100); err != nil {
		t.Fatal(err)
	}
	// The test's terminal is nobody's controlling terminal: send the
	// SIGWINCH a terminal emulator's resize would bring.
	syscall.Kill(os.Getpid(), syscall.SIGWINCH)
	if !waitFor(2*time.Second, size(40, 100)) {
		t.Fatal("the pty did not follow the terminal's size")
	}
	s.Terminate()
	<-ch
}

func TestRelayTwice(t *testing.T) {
	// One terminal, two sessions in turn; the second's output is not cut.
	tt := newTestTerminal(t, 24, 80)
	c := collect(tt)
	for _, word := range []string{"one", "two"} {
		s := startSession(t, sh("printf '\\033]0;"+word+"'"), nil, cookedModes(t)) // ends inside an OSC
		if res := <-relay(s, tt.Terminal); res.code != 0 {
			t.Fatalf("relay: %+v", res)
		}
		// The relay ended inside the OSC: CAN ends it before what follows.
		c.until(t, "\x1b]0;"+word+"\x18", 2*time.Second)
	}
	tt.Write([]byte("after"))
	c.until(t, "after", 2*time.Second)
}

func TestRelayHoldDuringSequence(t *testing.T) {
	// A hold made while the host is in the middle of an OSC goes out
	// after it; one made while it is in an unterminated one goes after
	// PlaceWait.
	s := startSession(t, sh(`stty raw -echo; printf '\033]0;ti'; IFS= read -r x; printf 'tle\007'; IFS= read -r x; printf '\033]0;open'; sleep 0.5; exit 0`), nil, cookedModes(t))
	tt := newTestTerminal(t, 24, 80)
	c := collect(tt)
	ch := relay(s, tt.Terminal)
	c.until(t, "\x1b]0;ti", 5*time.Second)
	h := tt.Hold()
	time.Sleep(5 * time.Millisecond) // well inside PlaceWait
	unix.Write(tt.mfd, []byte("\n"))
	c.until(t, "tle\a"+SyncBegin, 5*time.Second)
	unix.Write(tt.mfd, []byte("\n"))
	c.until(t, "\x1b]0;open", 5*time.Second)
	start := time.Now()
	tt.Release(h)
	c.until(t, SyncEnd, 5*time.Second)
	if d := time.Since(start); d < PlaceWait-5*time.Millisecond || d > PlaceWait+100*time.Millisecond {
		t.Fatalf("the forced write took %v, want about %v", d, PlaceWait)
	}
	if res := <-ch; res.code != 0 {
		t.Fatalf("relay %+v", res)
	}
	if tt.waited.Load() != 1 || tt.forced.Load() != 1 {
		t.Fatalf("waited %d forced %d", tt.waited.Load(), tt.forced.Load())
	}
}

func TestStartFails(t *testing.T) {
	if _, err := Start([]string{"/nonexistent/ssh"}, nil, cookedModes(t), 24, 80); err == nil {
		t.Fatal("started a missing command")
	}
	if _, err := Start(nil, nil, cookedModes(t), 24, 80); err == nil {
		t.Fatal("started nothing")
	}
}
