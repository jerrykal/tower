package relay

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Sequences the loop writes to its terminal.
const (
	// SyncBegin starts synchronized output (DEC mode 2026): the terminal
	// keeps showing its last frame until SyncEnd.
	SyncBegin = "\x1b[?2026h"
	// SyncEnd ends synchronized output.
	SyncEnd = "\x1b[?2026l"

	// ClientRestore puts a terminal back the way a tmux client leaves it
	// when it exits, for a client the loop hung up, whose own restore
	// never arrives. In order: the whole screen as the scroll region,
	// attributes off, G0 to ASCII, cursor keys and keypad back to normal,
	// the cursor's default shape, cursor shown, the mouse modes off (X10/normal,
	// button, any-event, SGR, UTF-8), bracketed paste off, focus events
	// off, extended keys off, and out of the alternate screen (which
	// brings the shell's screen and cursor back). It leaves synchronized
	// output alone: the loop writes it inside a hold.
	ClientRestore = "\x1b[r" + "\x1b[m" + "\x1b(B" + "\x1b[?1l\x1b>" +
		"\x1b[0 q" + "\x1b[?25h" +
		"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1005l" +
		"\x1b[?2004l" + "\x1b[?1004l" + "\x1b[>4m" + "\x1b[?1049l"
)

// PlaceWait is how long one of the loop's writes waits for the relayed
// output to reach a boundary before it goes out anyway.
const PlaceWait = 50 * time.Millisecond

// ErrRelaying is a second relay onto a terminal that already has one.
var ErrRelaying = errors.New("relay: terminal already relaying")

// Terminal is the loop's terminal. While a session is relayed onto it,
// the relay owns its output and the loop's own writes go in between whole
// escape sequences and characters of the relayed output; otherwise the
// loop's writes go straight out.
type Terminal struct {
	In, Out *os.File
	in, out int

	mu       sync.Mutex
	relaying bool
	queue    []byte    // the loop's writes waiting for a boundary
	queueN   int64     // how many writes queue holds
	since    time.Time // when the oldest of them was made
	wakeW    int       // the relay's wake pipe while relaying
	holds    uint64    // the newest hold's number
	held     bool      // the newest hold has not been released

	queued atomic.Bool // queue is not empty
	tr     tracker     // the relayed output, owned by the relay
	behind bool        // the queue met a sequence and waits for its end, owned by the relay
	sent   int64       // relayed bytes written, owned by the relay
	from   int64       // sent when the relay met the queue, -1 before; owned by the relay

	// Counts of the loop's writes during relays, for tests: written at
	// once, after waiting for a sequence to end, and after PlaceWait; and
	// the most relayed bytes a write waited through (at most the rest of
	// one sequence, unless the tracker missed its end).
	placed, waited, forced, through atomic.Int64
}

// NewTerminal makes the Terminal reading in and writing out (usually the
// same tty).
func NewTerminal(in, out *os.File) (*Terminal, error) {
	ifd, err := fileFd(in)
	if err != nil {
		return nil, err
	}
	ofd, err := fileFd(out)
	if err != nil {
		return nil, err
	}
	return &Terminal{In: in, Out: out, in: ifd, out: ofd, wakeW: -1}, nil
}

// Write writes the loop's own bytes b: at once, or while relaying at the
// relayed output's next boundary, or after PlaceWait if none comes. b
// should be whole sequences. Writes keep their order. Write does not wait
// for a relayed terminal; outside a relay it writes before returning, and
// a terminal that has gone away loses the bytes.
func (t *Terminal) Write(b []byte) {
	t.mu.Lock()
	t.write(b)
	t.mu.Unlock()
}

// write is Write with t.mu held.
func (t *Terminal) write(b []byte) {
	if !t.relaying {
		writeAll(t.out, b)
		return
	}
	if len(t.queue) == 0 {
		t.since = time.Now()
	}
	t.queue = append(t.queue, b...)
	t.queueN++
	t.queued.Store(true)
	unix.Write(t.wakeW, []byte{0}) // a full pipe is already awake
}

// Hold starts holding the terminal's frame (SyncBegin) and returns the
// hold's number for Release.
func (t *Terminal) Hold() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.holds++
	t.held = true
	t.write([]byte(SyncBegin))
	return t.holds
}

// Release ends hold n (SyncEnd), unless a newer hold has been made since
// or n was released already: a release belongs to its hold.
func (t *Terminal) Release(n uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n != t.holds || !t.held {
		return
	}
	t.held = false
	t.write([]byte(SyncEnd))
}

// RestoreClient writes ClientRestore, for a tmux client the loop hung up.
func (t *Terminal) RestoreClient() { t.Write([]byte(ClientRestore)) }

// begin hands the terminal's output to a relay, which wakes on wakeR.
func (t *Terminal) begin() (wakeR int, err error) {
	r, w, err := pipe()
	if err != nil {
		return -1, err
	}
	if err := unix.SetNonblock(w, true); err != nil {
		unix.Close(r)
		unix.Close(w)
		return -1, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.relaying {
		unix.Close(r)
		unix.Close(w)
		return -1, ErrRelaying
	}
	t.relaying = true
	t.wakeW = w
	t.tr.reset()
	t.from = -1
	return r, nil
}

// end takes the terminal's output back from the relay. A relay cut off
// inside a sequence leaves the terminal's parser inside it; CAN ends it
// there, so the loop's writes and the next client's output mean what they
// say. Writes still queued go out after it.
func (t *Terminal) end(wakeR int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tr.inSequence() {
		writeAll(t.out, []byte{bCAN})
	}
	if len(t.queue) > 0 {
		writeAll(t.out, t.queue)
		t.queue = t.queue[:0]
		t.queueN = 0
		t.queued.Store(false)
	}
	t.tr.reset()
	t.relaying = false
	unix.Close(t.wakeW)
	unix.Close(wakeR)
	t.wakeW = -1
}

// deadline is when the oldest queued write goes out regardless.
func (t *Terminal) deadline() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.since.Add(PlaceWait)
}

// flush writes the queued writes; the relay calls it at a boundary, or
// with force once they have waited PlaceWait.
func (t *Terminal) flush(force bool) error {
	t.mu.Lock()
	q, n := t.queue, t.queueN
	t.queue, t.queueN = nil, 0
	t.queued.Store(false)
	t.mu.Unlock()
	if len(q) == 0 {
		return nil
	}
	switch {
	case force:
		t.forced.Add(n)
	case t.behind:
		t.waited.Add(n)
	default:
		t.placed.Add(n)
	}
	t.behind = false
	if t.from >= 0 {
		t.through.Store(max(t.through.Load(), t.sent-t.from))
		t.from = -1
	}
	// The terminal parses these too: follow them, so that a forced write
	// that ended a sequence early leaves the tracker where the terminal is.
	t.tr.feed(q)
	return writeAll(t.out, q)
}

// emit writes relayed output p to the terminal, putting queued writes in
// at the first boundary, or at once if they have waited PlaceWait.
func (t *Terminal) emit(p []byte) error {
	for {
		if t.queued.Load() {
			if t.from < 0 {
				t.from = t.sent
			}
			var err error
			switch {
			case t.tr.ground():
				err = t.flush(false)
			case !time.Now().Before(t.deadline()):
				err = t.flush(true)
			}
			if err != nil {
				return err
			}
		}
		if len(p) == 0 {
			return nil
		}
		if !t.queued.Load() {
			t.tr.feed(p)
			t.sent += int64(len(p))
			return writeAll(t.out, p)
		}
		// Inside a sequence with writes waiting: up to its end.
		t.behind = true
		if t.from < 0 {
			t.from = t.sent
		}
		i := t.tr.feedToGround(p)
		t.sent += int64(i)
		if err := writeAll(t.out, p[:i]); err != nil {
			return err
		}
		p = p[i:]
	}
}
