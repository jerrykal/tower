package relay

import (
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipeTerminal is a Terminal writing into a pipe whose contents the test
// reads back.
type pipeTerminal struct {
	*Terminal
	mu  sync.Mutex
	got []byte
	r   *os.File
	eof chan struct{}
}

func newPipeTerminal(t *testing.T) *pipeTerminal {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	term, err := NewTerminal(os.Stdin, w)
	if err != nil {
		t.Fatal(err)
	}
	pt := &pipeTerminal{Terminal: term, r: r, eof: make(chan struct{})}
	go func() {
		defer close(pt.eof)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			pt.mu.Lock()
			pt.got = append(pt.got, buf[:n]...)
			pt.mu.Unlock()
			if err == io.EOF || err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { w.Close(); <-pt.eof; r.Close() })
	return pt
}

// written waits for the terminal to have received want, and fails on
// anything else.
func (pt *pipeTerminal) expect(t *testing.T, want string) {
	t.Helper()
	ok := waitFor(2*time.Second, func() bool {
		pt.mu.Lock()
		defer pt.mu.Unlock()
		return len(pt.got) >= len(want)
	})
	pt.mu.Lock()
	got := string(pt.got)
	pt.got = nil
	pt.mu.Unlock()
	if !ok || got != want {
		t.Fatalf("terminal got\n%q\nwant\n%q", got, want)
	}
}

func TestHoldRelease(t *testing.T) {
	pt := newPipeTerminal(t)
	h1 := pt.Hold()
	pt.expect(t, SyncBegin)
	h2 := pt.Hold()
	if h2 == h1 {
		t.Fatal("two holds share a number")
	}
	pt.expect(t, SyncBegin)
	pt.Release(h1) // an older hold's release ends nothing
	pt.Write([]byte("x"))
	pt.expect(t, "x")
	pt.Release(h2)
	pt.expect(t, SyncEnd)
	pt.Release(h2) // released already
	pt.Write([]byte("y"))
	pt.expect(t, "y")
	pt.RestoreClient()
	pt.expect(t, ClientRestore)
}

func TestClientRestore(t *testing.T) {
	// The restore is whole sequences, leaves synchronized output alone and
	// ends out of the alternate screen with the mouse and paste modes off.
	var tr tracker
	tr.feed([]byte(ClientRestore))
	if !tr.ground() {
		t.Fatal("the restore ends inside a sequence")
	}
	if strings.Contains(ClientRestore, "2026") {
		t.Fatal("the restore touches synchronized output")
	}
	for _, s := range []string{"\x1b[?1049l", "\x1b[?1000l", "\x1b[?1002l", "\x1b[?1003l", "\x1b[?1006l", "\x1b[?2004l", "\x1b[?25h"} {
		if !strings.Contains(ClientRestore, s) {
			t.Errorf("the restore lacks %q", s)
		}
	}
	if !strings.HasSuffix(ClientRestore, "\x1b[?1049l") {
		t.Error("the restore does not end leaving the alternate screen")
	}
}

// The placement, driven by hand: what the relay's output loop does with
// each read.
func TestPlacement(t *testing.T) {
	pt := newPipeTerminal(t)
	wake, err := pt.begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pt.begin(); err != ErrRelaying {
		t.Fatalf("second relay: %v", err)
	}

	// At a boundary, the next output carries the write first.
	pt.emit([]byte("ab"))
	h := pt.Hold()
	pt.emit([]byte("c"))
	pt.expect(t, "ab"+SyncBegin+"c")

	// Inside a CSI, it waits for the final byte.
	pt.emit([]byte("\x1b[3"))
	pt.Release(h)
	pt.emit([]byte("1mX"))
	pt.expect(t, "\x1b[31m"+SyncEnd+"X")

	// Inside a character.
	pt.emit([]byte("中"[:1]))
	pt.Write([]byte("W"))
	pt.emit([]byte("中"[1:] + "z"))
	pt.expect(t, "中W"+"z")

	// Inside an OSC across reads, until its BEL.
	pt.emit([]byte("\x1b]52;c;QUJD"))
	pt.Write([]byte("1"))
	pt.emit([]byte("REVG"))
	pt.Write([]byte("2"))
	pt.emit([]byte("\aq"))
	pt.expect(t, "\x1b]52;c;QUJDREVG\a12q")
	if pt.placed.Load() != 1 || pt.waited.Load() != 4 || pt.forced.Load() != 0 {
		t.Fatalf("placed %d waited %d forced %d", pt.placed.Load(), pt.waited.Load(), pt.forced.Load())
	}

	// An unterminated sequence holds a write PlaceWait at most.
	pt.emit([]byte("\x1b]0;tit"))
	pt.Write([]byte("F"))
	time.Sleep(PlaceWait + 5*time.Millisecond)
	pt.emit([]byte("le\a"))
	pt.expect(t, "\x1b]0;tit"+"F"+"le\a")
	if pt.forced.Load() != 1 {
		t.Fatalf("forced %d", pt.forced.Load())
	}

	// The relay ends inside a sequence: CAN ends it before the loop's
	// writes, which then go out.
	pt.emit([]byte("\x1b[1;"))
	pt.Write([]byte("E"))
	pt.end(wake)
	pt.expect(t, "\x1b[1;"+"\x18"+"E")

	// And writes go straight out again.
	pt.Write([]byte("D"))
	pt.expect(t, "D")
}
