package tmux

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed is returned by Do once the control client has ended.
var ErrClosed = errors.New("tmux: control client closed")

// Reply is the output of one command sent through a control client.
type Reply struct {
	Lines []string
	Err   bool // the block ended in %error
}

// Text is the reply's lines joined by newlines.
func (r Reply) Text() string { return strings.Join(r.Lines, "\n") }

// Note is a control-mode notification: "%session-changed $1 x" has Name
// "session-changed" and Args "$1 x".
type Note struct {
	Name string
	Args string
}

// Control is a tmux control-mode client (tmux -C). Every command goes on
// its own line and gets exactly one %begin…%end (or %error) block; tmux
// answers one client's commands in order, so replies are matched first in,
// first out. Blocks flagged 0 are not ours (the attach that started the
// client) and are dropped. A command whose caller gave up keeps its place
// in the queue, so a late reply still lines up with the right command.
type Control struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	name string

	mu      sync.Mutex
	waiting []chan Reply
	closed  bool

	notes chan Note
	lost  bool // a notification was dropped since the last Lost call
	done  chan struct{}
	err   error

	bytes atomic.Int64 // read from tmux so far
}

// noteBuffer is how many notifications wait for the reader of Notes.
const noteBuffer = 1024

// Attach starts a control client of s attached to session, with no pane
// output and no say in window sizes. It returns once tmux has answered a
// first command, which also tells the client's own name.
func Attach(s Server, session string) (*Control, error) {
	args := append(append([]string{}, s.Args...), "-C", "attach-session", "-f", "no-output,ignore-size", "-t", session)
	c := &Control{
		cmd:   exec.Command(s.bin(), args...),
		notes: make(chan Note, noteBuffer),
		done:  make(chan struct{}),
	}
	in, err := c.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := c.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c.in = in
	if err := c.cmd.Start(); err != nil {
		return nil, err
	}
	go c.read(out)
	r, err := c.DoTimeout("display-message -p '#{client_name}'", 5*time.Second)
	if err != nil || r.Err || len(r.Lines) != 1 {
		c.Kill()
		if err == nil {
			err = fmt.Errorf("tmux: control client did not attach: %s", r.Text())
		}
		return nil, err
	}
	c.name = r.Lines[0]
	return c, nil
}

// Name is the control client's tmux client name.
func (c *Control) Name() string { return c.name }

// Pid is the control client's process id.
func (c *Control) Pid() int { return c.cmd.Process.Pid }

// Notes delivers notifications in order.
func (c *Control) Notes() <-chan Note { return c.notes }

// Lost reports, and clears, whether notifications were dropped because
// nobody read Notes: the reader should then re-read everything.
func (c *Control) Lost() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.lost
	c.lost = false
	return l
}

// Done is closed when the client has ended.
func (c *Control) Done() <-chan struct{} { return c.done }

// Do sends one command line and waits for its reply.
func (c *Control) Do(line string) (Reply, error) { return c.DoTimeout(line, 0) }

// DoTimeout is Do giving up after d (0: never). A command given up on
// keeps its place, so later replies still match.
func (c *Control) DoTimeout(line string, d time.Duration) (Reply, error) {
	if strings.ContainsAny(line, "\n\r") {
		return Reply{}, errors.New("tmux: a control command must be one line")
	}
	ch := make(chan Reply, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Reply{}, ErrClosed
	}
	c.waiting = append(c.waiting, ch)
	_, err := io.WriteString(c.in, line+"\n")
	c.mu.Unlock()
	if err != nil {
		return Reply{}, ErrClosed
	}
	var timeout <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return Reply{}, ErrClosed
		}
		return r, nil
	case <-timeout:
		return Reply{}, fmt.Errorf("tmux: no reply to %q within %v", line, d)
	}
}

// DoMany sends lines back to back and waits for every reply, in order:
// one round trip to the server for a batch of reads. It gives up after d
// (0: never); replies not yet in are then missing from the result.
func (c *Control) DoMany(lines []string, d time.Duration) ([]Reply, error) {
	chans := make([]chan Reply, len(lines))
	var buf strings.Builder
	for i, l := range lines {
		if strings.ContainsAny(l, "\n\r") {
			return nil, errors.New("tmux: a control command must be one line")
		}
		chans[i] = make(chan Reply, 1)
		buf.WriteString(l)
		buf.WriteByte('\n')
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.waiting = append(c.waiting, chans...)
	_, err := io.WriteString(c.in, buf.String())
	c.mu.Unlock()
	if err != nil {
		return nil, ErrClosed
	}
	var timeout <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	out := make([]Reply, 0, len(lines))
	for _, ch := range chans {
		select {
		case r, ok := <-ch:
			if !ok {
				return out, ErrClosed
			}
			out = append(out, r)
		case <-timeout:
			return out, fmt.Errorf("tmux: no reply to %d commands within %v", len(lines)-len(out), d)
		}
	}
	return out, nil
}

// Bytes is how much the client has read from tmux: what the server sends
// a control client with no pane output.
func (c *Control) Bytes() int64 { return c.bytes.Load() }

func (c *Control) read(out io.Reader) {
	br := bufio.NewReaderSize(out, 64<<10)
	var block *Reply // inside a %begin block
	var ours bool
	var guard string // the block's "<time> <number> <flags>"
	for {
		line, err := br.ReadString('\n')
		c.bytes.Add(int64(len(line)))
		if err != nil {
			break
		}
		line = strings.TrimSuffix(line, "\n")
		if block != nil {
			// tmux does not escape command output: a pane's text can hold
			// "%end …" lines. Only the guard of this block's %begin ends it.
			if end, isErr := endOf(line, guard); end {
				block.Err = isErr
				if ours {
					c.deliver(*block)
				}
				block = nil
				continue
			}
			block.Lines = append(block.Lines, line)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "%begin "); ok {
			block = &Reply{}
			guard = rest
			f := strings.Fields(rest)
			ours = len(f) >= 3 && f[2] != "0"
			continue
		}
		if strings.HasPrefix(line, "%") {
			name, args, _ := strings.Cut(line[1:], " ")
			c.note(Note{Name: name, Args: args})
		}
	}
	c.mu.Lock()
	c.closed = true
	for _, ch := range c.waiting {
		close(ch)
	}
	c.waiting = nil
	c.mu.Unlock()
	c.err = c.cmd.Wait()
	close(c.done)
	close(c.notes)
}

// endOf reports whether line ends the block whose %begin carried guard
// ("<time> <number> <flags>"), and whether it ended in an error.
func endOf(line, guard string) (end, isErr bool) {
	switch line {
	case "%end " + guard:
		return true, false
	case "%error " + guard:
		return true, true
	}
	return false, false
}

func (c *Control) deliver(r Reply) {
	c.mu.Lock()
	if len(c.waiting) == 0 {
		c.mu.Unlock()
		return
	}
	ch := c.waiting[0]
	c.waiting = c.waiting[1:]
	c.mu.Unlock()
	ch <- r
}

func (c *Control) note(n Note) {
	select {
	case c.notes <- n:
	default:
		// Nobody is reading: drop the oldest so the newest gets through,
		// and remember to say so.
		select {
		case <-c.notes:
		default:
		}
		select {
		case c.notes <- n:
		default:
		}
		c.mu.Lock()
		c.lost = true
		c.mu.Unlock()
	}
}

// Close detaches the client by its own name and waits for it to end. A
// bare detach-client from a control client whose session is gone would
// detach the most recently active client instead: the user's terminal.
func (c *Control) Close() {
	select {
	case <-c.done:
		return
	default:
	}
	if c.name != "" {
		c.DoTimeout("detach-client -t "+Quote(c.name), 2*time.Second)
	}
	c.in.Close()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		c.Kill()
	}
}

// Kill ends the client process at once.
func (c *Control) Kill() {
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
	c.in.Close()
	<-c.done
}
