// Package stream is one home ↔ remote peer over a byte stream: JSON lines
// both ways, a writer that never blocks its caller, requests matched to
// their answers, keepalive, the stall mark and the peer's clock offset.
// It knows nothing of homes and remotes; towerd gives it a handler.
package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// Errors a stream ends or a request fails with.
var (
	ErrSilent   = errors.New("stream: peer silent too long")
	ErrQueue    = errors.New("stream: too many messages queued for the peer")
	ErrStalled  = errors.New("stream: peer stalled")
	ErrClosed   = errors.New("stream: closed")
	ErrDeadline = errors.New("stream: deadline passed")
)

// Options configure a Conn.
type Options struct {
	Ping     time.Duration // keepalive interval once Live (default 1s, TOWER_PING)
	Silence  time.Duration // close after this long without a byte (default 15s)
	MaxQueue int           // close above this many queued messages (default 4096)
	// OnMsg gets every message except ping, pong and ack, in order, on the
	// reader goroutine: it must not block.
	OnMsg func(*proto.Msg)
	// OnStall is told when the stall mark changes.
	OnStall func(stalled bool)
	// Closer tears the transport down (kills ssh, closes the socket).
	Closer func()
	Log    func(format string, args ...any)
}

// window is how many recent pings the RTT and offset estimates keep.
const window = 8

type pingRec struct {
	sent  time.Time
	epoch uint64
}

type sample struct {
	rtt    time.Duration
	offset time.Duration
}

// Conn is one stream peer.
type Conn struct {
	o    Options
	r    io.Reader
	w    io.Writer
	base time.Time

	heard atomic.Int64 // ns since base of the last byte read

	mu      sync.Mutex
	queue   []*proto.Msg
	snaps   map[string]*proto.Msg // newest unsent state and view
	pending map[string]chan *proto.Ack
	pings   map[uint64]pingRec
	nextN   uint64
	samples []sample
	helloRT time.Duration
	stalled bool
	epoch   uint64 // stalls so far
	stallC  chan struct{}
	live    bool
	unknown map[string]bool
	closed  bool
	err     error

	wake chan struct{}
	done chan struct{}
}

// New makes a stream reading lines from r and writing them to w.
func New(r io.Reader, w io.Writer, o Options) *Conn {
	if o.Ping <= 0 {
		o.Ping = Interval()
	}
	if o.Silence <= 0 {
		o.Silence = 15 * time.Second
	}
	if o.MaxQueue <= 0 {
		o.MaxQueue = 4096
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	c := &Conn{
		o: o, r: r, w: w, base: time.Now(),
		snaps:   map[string]*proto.Msg{},
		pending: map[string]chan *proto.Ack{},
		pings:   map[uint64]pingRec{},
		unknown: map[string]bool{},
		stallC:  make(chan struct{}),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	c.heard.Store(0)
	return c
}

// Interval is the keepalive interval: TOWER_PING (a Go duration or
// milliseconds), else 1s.
func Interval() time.Duration {
	if d := config.Duration("TOWER_PING", time.Second); d > 0 {
		return d
	}
	return time.Second
}

// Now is tower's wall clock in unix milliseconds. TOWER_TEST_SKEW (ms)
// moves it, so tests can run a host whose clock is off.
func Now() int64 { return time.Now().UnixMilli() + skew }

var skew = func() int64 {
	v, _ := strconv.ParseInt(os.Getenv("TOWER_TEST_SKEW"), 10, 64)
	return v
}()

// Start runs the reader and the writer.
func (c *Conn) Start() {
	c.heard.Store(int64(time.Since(c.base)))
	go c.readLoop()
	go c.writeLoop()
}

// Live starts the keepalive: a ping now and every Ping, the silence and
// stall checks. Call it once the hello is done.
func (c *Conn) Live() {
	c.mu.Lock()
	if c.live || c.closed {
		c.mu.Unlock()
		return
	}
	c.live = true
	c.mu.Unlock()
	go c.keepalive()
}

// SetHelloRTT seeds the round-trip estimate with the hello's, until the
// first pong.
func (c *Conn) SetHelloRTT(d time.Duration) {
	c.mu.Lock()
	c.helloRT = d
	c.mu.Unlock()
}

// Done is closed when the stream has ended.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the stream ended.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Heard is when the last byte arrived.
func (c *Conn) Heard() time.Time { return c.base.Add(time.Duration(c.heard.Load())) }

// Silent is how long since the last byte arrived.
func (c *Conn) Silent() time.Duration {
	return time.Since(c.base) - time.Duration(c.heard.Load())
}

// Stalled reports the stall mark.
func (c *Conn) Stalled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stalled
}

// Close ends the stream: pending requests fail, the transport is torn
// down. It is safe to call more than once.
func (c *Conn) Close(err error) {
	if err == nil {
		err = ErrClosed
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = err
	pend := c.pending
	c.pending = map[string]chan *proto.Ack{}
	c.mu.Unlock()
	close(c.done)
	for _, ch := range pend {
		close(ch)
	}
	if cl, ok := c.w.(io.Closer); ok {
		cl.Close()
	}
	if c.o.Closer != nil {
		c.o.Closer()
	}
}

// Send queues m. A state or view replaces the newest unsent one of its
// type; every other message keeps its order. Send never blocks.
func (c *Conn) Send(m *proto.Msg) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if m.T == proto.TState || m.T == proto.TView {
		c.snaps[m.T] = m
	} else {
		c.queue = append(c.queue, m)
	}
	over := len(c.queue) > c.o.MaxQueue
	c.mu.Unlock()
	if over {
		go c.Close(ErrQueue)
		return
	}
	c.kick()
}

// SendNow queues a snapshot in order with the other messages, replacing
// an unsent one of its type: what goes ahead of an answer (read your
// writes).
func (c *Conn) SendNow(m *proto.Msg) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	delete(c.snaps, m.T)
	c.queue = append(c.queue, m)
	c.mu.Unlock()
	c.kick()
}

func (c *Conn) kick() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Conn) writeLoop() {
	for {
		select {
		case <-c.wake:
		case <-c.done:
			return
		}
		for {
			m := c.next()
			if m == nil {
				break
			}
			b, err := json.Marshal(m)
			if err != nil {
				c.o.Log("stream: cannot encode %s: %v", m.T, err)
				continue
			}
			if _, err := c.w.Write(append(b, '\n')); err != nil {
				c.Close(err)
				return
			}
		}
	}
}

// next pops the next message to write: ordered ones first, then the
// newest snapshots.
func (c *Conn) next() *proto.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if len(c.queue) > 0 {
		m := c.queue[0]
		c.queue[0] = nil
		c.queue = c.queue[1:]
		return m
	}
	for _, t := range []string{proto.TState, proto.TView} {
		if m := c.snaps[t]; m != nil {
			delete(c.snaps, t)
			return m
		}
	}
	return nil
}

// heardReader notes the time of every read that returned bytes: any byte
// counts as a sign of life, so a large line crawling in is not silence.
type heardReader struct{ c *Conn }

func (h heardReader) Read(p []byte) (int, error) {
	n, err := h.c.r.Read(p)
	if n > 0 {
		h.c.heard.Store(int64(time.Since(h.c.base)))
		h.c.unstall()
	}
	return n, err
}

func (c *Conn) readLoop() {
	br := bufio.NewReaderSize(heardReader{c}, 64<<10)
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// A long line: gather it whole.
			buf := append([]byte(nil), line...)
			for err == bufio.ErrBufferFull {
				line, err = br.ReadSlice('\n')
				buf = append(buf, line...)
			}
			line = buf
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			c.Close(err)
			return
		}
		var m proto.Msg
		if err := json.Unmarshal(line, &m); err != nil {
			c.o.Log("stream: bad line (%d bytes): %v", len(line), err)
			continue
		}
		c.dispatch(&m)
	}
}

func (c *Conn) dispatch(m *proto.Msg) {
	switch m.T {
	case proto.TPing:
		if m.Ping != nil {
			c.queueOrdered(&proto.Msg{T: proto.TPong, Ping: &proto.Ping{N: m.Ping.N, Clock: Now()}})
		}
	case proto.TPong:
		if m.Ping != nil {
			c.pong(m.Ping)
		}
	case proto.TAck:
		if m.Ack != nil {
			c.mu.Lock()
			ch := c.pending[m.Ack.ID]
			delete(c.pending, m.Ack.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m.Ack
			}
		}
	case proto.THello, proto.TState, proto.TView, proto.TExec, proto.TRelay:
		if c.o.OnMsg != nil {
			c.o.OnMsg(m)
		}
	default:
		c.mu.Lock()
		first := !c.unknown[m.T]
		c.unknown[m.T] = true
		c.mu.Unlock()
		if first {
			c.o.Log("stream: ignoring unknown message type %q", m.T)
		}
	}
}

func (c *Conn) queueOrdered(m *proto.Msg) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.queue = append(c.queue, m)
	c.mu.Unlock()
	c.kick()
}

// Answer queues an ack, in order with the other messages.
func (c *Conn) Answer(a *proto.Ack) { c.queueOrdered(&proto.Msg{T: proto.TAck, Ack: a}) }

func (c *Conn) pong(p *proto.Ping) {
	now := time.Now()
	c.mu.Lock()
	rec, ok := c.pings[p.N]
	delete(c.pings, p.N)
	if !ok || rec.epoch != c.epoch || c.stalled {
		// Unknown, or the ping spanned a stall: its round trip says how
		// long the stall was, not how far the peer is.
		c.mu.Unlock()
		return
	}
	rtt := now.Sub(rec.sent)
	mid := rec.sent.Add(rtt / 2).UnixMilli()
	if skew != 0 {
		mid += skew
	}
	off := time.Duration(p.Clock-mid) * time.Millisecond
	c.samples = append(c.samples, sample{rtt: rtt, offset: off})
	if len(c.samples) > window {
		c.samples = c.samples[len(c.samples)-window:]
	}
	c.mu.Unlock()
}

// SlowRTT is a slow recent round trip: the second slowest of the last 8,
// the hello's before the first pong, else 1s.
func (c *Conn) SlowRTT() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slowRTT()
}

func (c *Conn) slowRTT() time.Duration {
	n := len(c.samples)
	if n == 0 {
		if c.helloRT > 0 {
			return c.helloRT
		}
		return time.Second
	}
	r := make([]time.Duration, n)
	for i, s := range c.samples {
		r[i] = s.rtt
	}
	slices.Sort(r)
	if n >= 2 {
		return r[n-2]
	}
	return r[0]
}

// Offset is the peer's clock minus ours, from the fastest recent ping.
func (c *Conn) Offset() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset()
}

func (c *Conn) offset() time.Duration {
	best := -1
	for i, s := range c.samples {
		if best < 0 || s.rtt < c.samples[best].rtt {
			best = i
		}
	}
	if best < 0 {
		return 0
	}
	return c.samples[best].offset
}

// ToLocal converts a deadline in the peer's clock (unix ms) to ours.
func (c *Conn) ToLocal(peerMs int64) int64 {
	if peerMs == 0 {
		return 0
	}
	return peerMs - c.Offset().Milliseconds()
}

// ToPeer converts a deadline in our clock (unix ms) to the peer's.
func (c *Conn) ToPeer(localMs int64) int64 {
	if localMs == 0 {
		return 0
	}
	return localMs + c.Offset().Milliseconds()
}

// Margin is what a hop keeps back of a deadline for the answer to get
// home in time: one slow recent round trip, at least 200ms.
func (c *Conn) Margin() time.Duration { return max(200*time.Millisecond, c.SlowRTT()) }

// StallAfter is how long the peer may be silent before it is marked
// stalled: a ping interval plus three slow round trips, at least a second
// of them.
func (c *Conn) StallAfter() time.Duration {
	return c.o.Ping + max(time.Second, 3*c.SlowRTT())
}

// GiveUpAfter is how long a stall may last before the home gives the link
// up: 3s plus four slow round trips.
func (c *Conn) GiveUpAfter() time.Duration { return 3*time.Second + 4*c.SlowRTT() }

func (c *Conn) keepalive() {
	ping := time.NewTicker(c.o.Ping)
	defer ping.Stop()
	check := time.NewTicker(min(c.o.Ping/4, 50*time.Millisecond))
	defer check.Stop()
	c.ping()
	for {
		select {
		case <-c.done:
			return
		case <-ping.C:
			c.ping()
		case <-check.C:
			silent := c.Silent()
			if silent > c.o.Silence {
				c.Close(ErrSilent)
				return
			}
			if silent > c.StallAfter() {
				c.stall()
			}
		}
	}
}

func (c *Conn) ping() {
	c.mu.Lock()
	c.nextN++
	n := c.nextN
	c.pings[n] = pingRec{sent: time.Now(), epoch: c.epoch}
	for k := range c.pings {
		if k+4*window < n {
			delete(c.pings, k)
		}
	}
	c.mu.Unlock()
	c.queueOrdered(&proto.Msg{T: proto.TPing, Ping: &proto.Ping{N: n}})
}

func (c *Conn) stall() {
	c.mu.Lock()
	if c.stalled || c.closed {
		c.mu.Unlock()
		return
	}
	c.stalled = true
	c.epoch++
	close(c.stallC)
	c.mu.Unlock()
	c.o.Log("stream: peer stalled (silent %v)", c.Silent().Round(time.Millisecond))
	if c.o.OnStall != nil {
		c.o.OnStall(true)
	}
}

func (c *Conn) unstall() {
	c.mu.Lock()
	if !c.stalled {
		c.mu.Unlock()
		return
	}
	c.stalled = false
	c.stallC = make(chan struct{})
	c.mu.Unlock()
	c.o.Log("stream: peer heard again")
	if c.o.OnStall != nil {
		c.o.OnStall(false)
	}
}

// Request sends req as message type t (exec or relay) and waits for its
// ack. req.Deadline is in our clock; the peer gets it converted, less the
// margin its answer needs to get back. A stalled peer fails it at once,
// and a stall while it waits aborts it.
func (c *Conn) Request(ctx context.Context, t string, req *proto.Request) (*proto.Ack, error) {
	r := *req
	if r.ID == "" {
		r.ID = config.NewID() + config.NewID()
	}
	ch := make(chan *proto.Ack, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if c.stalled {
		c.mu.Unlock()
		return nil, ErrStalled
	}
	stallC := c.stallC
	var wait <-chan time.Time
	if r.Deadline != 0 {
		left := time.Duration(r.Deadline-Now()) * time.Millisecond
		if left <= 0 {
			c.mu.Unlock()
			return nil, ErrDeadline
		}
		margin := max(200*time.Millisecond, c.slowRTT())
		r.Deadline = r.Deadline - margin.Milliseconds() + c.offset().Milliseconds()
		timer := time.NewTimer(left)
		defer timer.Stop()
		wait = timer.C
	}
	c.pending[r.ID] = ch
	c.queue = append(c.queue, &proto.Msg{T: t, Req: &r})
	c.mu.Unlock()
	c.kick()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, r.ID)
		c.mu.Unlock()
	}
	select {
	case a, ok := <-ch:
		if !ok {
			return nil, c.closedErr()
		}
		return a, nil
	case <-stallC:
		forget()
		return nil, ErrStalled
	case <-wait:
		forget()
		return nil, ErrDeadline
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.closedErr()
	}
}

func (c *Conn) closedErr() error {
	if err := c.Err(); err != nil {
		return err
	}
	return ErrClosed
}
