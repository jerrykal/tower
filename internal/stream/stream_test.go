package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

// pair connects two streams through in-memory pipes.
func pair(t *testing.T, ao, bo Options) (*Conn, *Conn) {
	t.Helper()
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	ao.Closer = func() { ar.Close(); aw.Close() }
	bo.Closer = func() { br.Close(); bw.Close() }
	a, b := New(ar, aw, ao), New(br, bw, bo)
	t.Cleanup(func() { a.Close(nil); b.Close(nil) })
	return a, b
}

type inbox struct {
	mu   sync.Mutex
	msgs []*proto.Msg
	ch   chan *proto.Msg
}

func newInbox() *inbox { return &inbox{ch: make(chan *proto.Msg, 1000)} }

func (i *inbox) on(m *proto.Msg) { i.ch <- m }

func (i *inbox) next(t *testing.T, d time.Duration) *proto.Msg {
	t.Helper()
	select {
	case m := <-i.ch:
		return m
	case <-time.After(d):
		t.Fatal("no message")
		return nil
	}
}

func TestOrderAndUnknown(t *testing.T) {
	in := newInbox()
	var logs []string
	var lmu sync.Mutex
	a, b := pair(t, Options{}, Options{OnMsg: in.on, Log: func(f string, args ...any) {
		lmu.Lock()
		logs = append(logs, f)
		lmu.Unlock()
	}})
	a.Start()
	b.Start()
	a.Send(&proto.Msg{T: proto.THello, Hello: &proto.Hello{ID: "a"}})
	a.Send(&proto.Msg{T: "future"})
	a.Send(&proto.Msg{T: "future"})
	for i := range 50 {
		a.Send(&proto.Msg{T: proto.TExec, Req: &proto.Request{ID: string(rune('A' + i))}})
	}
	if m := in.next(t, time.Second); m.T != proto.THello {
		t.Fatal(m.T)
	}
	for i := range 50 {
		m := in.next(t, time.Second)
		if m.T != proto.TExec || m.Req.ID != string(rune('A'+i)) {
			t.Fatalf("out of order at %d: %+v", i, m.Req)
		}
	}
	lmu.Lock()
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "unknown") {
			n++
		}
	}
	lmu.Unlock()
	if n != 1 {
		t.Fatalf("an unknown type is logged once, got %d", n)
	}
}

// A peer that reads slowly gets the newest snapshot, not every one.
func TestSnapshotsKeepNewest(t *testing.T) {
	pr, pw := io.Pipe()
	idle, _ := io.Pipe()
	c := New(idle, pw, Options{})
	defer c.Close(nil)
	// Nothing reads the pipe yet: the first write blocks the writer.
	c.Start()
	for i := 1; i <= 100; i++ {
		c.Send(&proto.Msg{T: proto.TState, State: &proto.State{Seq: uint64(i)}})
	}
	c.Send(&proto.Msg{T: proto.TExec, Req: &proto.Request{ID: "x"}})
	sc := bufio.NewScanner(pr)
	var seqs []uint64
	sawExec := false
	deadline := time.After(2 * time.Second)
	for !(sawExec && len(seqs) > 0 && seqs[len(seqs)-1] == 100) {
		lines := make(chan string, 1)
		go func() {
			if sc.Scan() {
				lines <- sc.Text()
			}
		}()
		select {
		case l := <-lines:
			var m proto.Msg
			json.Unmarshal([]byte(l), &m)
			if m.T == proto.TState {
				seqs = append(seqs, m.State.Seq)
			}
			if m.T == proto.TExec {
				sawExec = true
			}
		case <-deadline:
			t.Fatalf("got states %v, exec %v", seqs, sawExec)
		}
	}
	if len(seqs) > 3 {
		t.Fatalf("a slow peer got %d states, want the newest only: %v", len(seqs), seqs)
	}
}

// Both sides queue a large first message before either reads: no
// deadlock (two synchronous first writes filled both pipes).
func TestLargeFirstMessagesBothWays(t *testing.T) {
	ia, ib := newInbox(), newInbox()
	a, b := pair(t, Options{OnMsg: ia.on}, Options{OnMsg: ib.on})
	big := strings.Repeat("x", 2<<20)
	a.Send(&proto.Msg{T: proto.TView, View: &proto.View{Home: big}})
	b.Send(&proto.Msg{T: proto.TState, State: &proto.State{Inst: big}})
	a.Start()
	b.Start()
	if m := ib.next(t, 5*time.Second); len(m.View.Home) != len(big) {
		t.Fatal("view cut")
	}
	if m := ia.next(t, 5*time.Second); len(m.State.Inst) != len(big) {
		t.Fatal("state cut")
	}
}

func TestRequestAck(t *testing.T) {
	var b *Conn
	a, b := pair(t, Options{}, Options{OnMsg: func(m *proto.Msg) {
		if m.T == proto.TExec {
			go b.Answer(&proto.Ack{ID: m.Req.ID, OK: true, Note: m.Req.Name})
		}
	}})
	a.Start()
	b.Start()
	ack, err := a.Request(context.Background(), proto.TExec, &proto.Request{Op: proto.OpKill, Name: "n", Deadline: Now() + 2000})
	if err != nil || !ack.OK || ack.Note != "n" {
		t.Fatalf("%+v %v", ack, err)
	}
	// An unanswered request ends at its deadline.
	b2 := b
	_ = b2
	start := time.Now()
	_, err = a.Request(context.Background(), proto.TRelay, &proto.Request{Op: proto.OpKill, Deadline: Now() + 300})
	if err != ErrDeadline || time.Since(start) > time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
	if _, err := a.Request(context.Background(), proto.TExec, &proto.Request{Deadline: Now() - 1}); err != ErrDeadline {
		t.Fatalf("a request past its deadline is not sent: %v", err)
	}
}

// fakePeer answers pings with a clock offset and can go silent.
type fakePeer struct {
	r      *bufio.Reader
	w      io.Writer
	offset int64
	mu     sync.Mutex
	silent bool
	got    chan *proto.Msg
}

func newFakePeer(t *testing.T, offsetMs int64, o Options) (*Conn, *fakePeer) {
	cr, pw := io.Pipe()
	pr, cw := io.Pipe()
	c := New(cr, cw, o)
	p := &fakePeer{r: bufio.NewReader(pr), w: pw, offset: offsetMs, got: make(chan *proto.Msg, 100)}
	go p.run()
	t.Cleanup(func() { c.Close(nil); pr.Close(); pw.Close() })
	return c, p
}

func (p *fakePeer) run() {
	for {
		l, err := p.r.ReadBytes('\n')
		if err != nil {
			return
		}
		var m proto.Msg
		json.Unmarshal(l, &m)
		p.mu.Lock()
		silent := p.silent
		p.mu.Unlock()
		if silent {
			continue
		}
		if m.T == proto.TPing {
			b, _ := json.Marshal(proto.Msg{T: proto.TPong, Ping: &proto.Ping{N: m.Ping.N, Clock: Now() + p.offset}})
			p.w.Write(append(b, '\n'))
			continue
		}
		p.got <- &m
	}
}

func (p *fakePeer) setSilent(s bool) {
	p.mu.Lock()
	p.silent = s
	p.mu.Unlock()
}

func TestClockOffsetAndDeadlines(t *testing.T) {
	c, p := newFakePeer(t, 3_600_000, Options{Ping: 20 * time.Millisecond})
	c.Start()
	c.Live()
	time.Sleep(200 * time.Millisecond)
	off := c.Offset()
	if d := off - time.Hour; d < -50*time.Millisecond || d > 50*time.Millisecond {
		t.Fatalf("offset %v, want an hour", off)
	}
	now := Now()
	if got := c.ToPeer(now) - now; got < 3_599_950 || got > 3_600_050 {
		t.Fatalf("ToPeer moved by %d", got)
	}
	if back := c.ToLocal(c.ToPeer(now)); back != now {
		t.Fatalf("ToLocal(ToPeer(x)) = x+%d", back-now)
	}
	// The deadline the peer sees is in its clock, less the margin.
	go c.Request(context.Background(), proto.TExec, &proto.Request{ID: "r", Deadline: now + 1500})
	m := <-p.got
	want := now + 1500 - c.Margin().Milliseconds() + 3_600_000
	if d := m.Req.Deadline - want; d < -50 || d > 50 {
		t.Fatalf("peer deadline %d, want about %d", m.Req.Deadline, want)
	}
}

// The hello's round trip stands in until the first pong measures one.
func TestMeasured(t *testing.T) {
	c, _ := newFakePeer(t, 0, Options{Ping: 20 * time.Millisecond})
	c.Start()
	c.SetHelloRTT(5 * time.Second)
	select {
	case <-c.Measured():
		t.Fatal("measured before a ping")
	default:
	}
	if got := c.Margin(); got != 5*time.Second {
		t.Fatalf("margin %v before the first pong, want the hello's 5s", got)
	}
	c.Live()
	select {
	case <-c.Measured():
	case <-time.After(time.Second):
		t.Fatal("no round trip measured")
	}
	if got := c.Margin(); got != 200*time.Millisecond {
		t.Fatalf("margin %v after the first pong, want the floor", got)
	}
}

func TestStallAndRecovery(t *testing.T) {
	stalls := make(chan bool, 10)
	c, p := newFakePeer(t, 0, Options{Ping: 50 * time.Millisecond, Silence: 10 * time.Second, OnStall: func(s bool) { stalls <- s }})
	c.Start()
	c.Live()
	time.Sleep(300 * time.Millisecond)
	if c.Stalled() {
		t.Fatal("stalled while answering")
	}
	p.setSilent(true)
	// A request waiting when the stall is marked is aborted.
	errc := make(chan error, 1)
	go func() {
		_, err := c.Request(context.Background(), proto.TExec, &proto.Request{ID: "w", Deadline: Now() + 10000})
		errc <- err
	}()
	start := time.Now()
	select {
	case s := <-stalls:
		if !s {
			t.Fatal("expected a stall")
		}
		// Ping + max(1s, 3 RTT): about 1.05s here.
		if el := time.Since(start); el < 900*time.Millisecond || el > 1600*time.Millisecond {
			t.Fatalf("stall marked after %v", el)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no stall")
	}
	if err := <-errc; err != ErrStalled {
		t.Fatalf("waiting request: %v", err)
	}
	if _, err := c.Request(context.Background(), proto.TExec, &proto.Request{Deadline: Now() + 1000}); err != ErrStalled {
		t.Fatalf("a request to a stalled peer fails at once: %v", err)
	}
	p.setSilent(false)
	select {
	case s := <-stalls:
		if s {
			t.Fatal("expected recovery")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("not heard again")
	}
}

func TestSilenceCloses(t *testing.T) {
	c, p := newFakePeer(t, 0, Options{Ping: 50 * time.Millisecond, Silence: 400 * time.Millisecond})
	c.Start()
	c.Live()
	p.setSilent(true)
	select {
	case <-c.Done():
		if c.Err() != ErrSilent {
			t.Fatal(c.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("silent peer kept")
	}
}

func TestQueueLimitCloses(t *testing.T) {
	_, pw := io.Pipe() // never read
	idle, _ := io.Pipe()
	c := New(idle, pw, Options{MaxQueue: 10})
	c.Start()
	for range 20 {
		c.Send(&proto.Msg{T: proto.TExec, Req: &proto.Request{}})
	}
	select {
	case <-c.Done():
		if c.Err() != ErrQueue {
			t.Fatal(c.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("over-full queue kept")
	}
}

func TestSlowRTTIgnoresOneOutlier(t *testing.T) {
	c := New(strings.NewReader(""), io.Discard, Options{})
	if c.SlowRTT() != time.Second {
		t.Fatal("no samples: 1s")
	}
	c.SetHelloRTT(30 * time.Millisecond)
	if c.SlowRTT() != 30*time.Millisecond {
		t.Fatal("hello's RTT before the first pong")
	}
	for _, ms := range []int{10, 12, 11, 500, 13} {
		c.samples = append(c.samples, sample{rtt: time.Duration(ms) * time.Millisecond})
	}
	if c.SlowRTT() != 13*time.Millisecond {
		t.Fatalf("SlowRTT %v", c.SlowRTT())
	}
	if c.Margin() != 200*time.Millisecond {
		t.Fatal("margin floor")
	}
}

func TestDrainWaitsForTheWriter(t *testing.T) {
	in := newInbox()
	a, b := pair(t, Options{}, Options{OnMsg: in.on})
	a.Start()
	b.Start()
	for i := range 20 {
		a.Send(&proto.Msg{T: proto.TExec, Req: &proto.Request{ID: string(rune('a' + i))}})
	}
	if !a.Drain(2 * time.Second) {
		t.Fatal("not drained")
	}
	a.Close(nil)
	for range 20 {
		in.next(t, time.Second)
	}
}
