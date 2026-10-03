# stream

One home ↔ remote peer over a byte stream (ssh's pipes at the home, the
bridge's socket connection at the remote): JSON lines in both directions,
with a writer that never blocks its caller, request/answer matching,
keepalive, the stall mark and the peer's clock offset. It knows nothing of
homes and remotes; `towerd` gives it a handler.

## API

```go
type Options struct {
    Ping      time.Duration // interval; 0 = no pings yet (see Live)
    Silence   time.Duration // give up after this long without a byte (15s home, 35s remote)
    MaxQueue  int           // drop the stream above this many queued messages (4096)
    OnMsg     func(*proto.Msg)   // every message except ping, pong, ack; called in order
    OnStall   func(stalled bool) // the stall mark changed
    Log       func(format string, args ...any)
}

func New(r io.Reader, w io.WriteCloser, o Options) *Conn

func (c *Conn) Start()                         // starts the reader and the writer
func (c *Conn) Live()                          // hello done: ping now and every o.Ping
func (c *Conn) Send(m *proto.Msg)              // queue; never blocks
func (c *Conn) SendNow(m *proto.Msg)           // a snapshot in order, ahead of an answer
func (c *Conn) Request(ctx context.Context, t string, r *proto.Request) (*proto.Ack, error)
func (c *Conn) Answer(a *proto.Ack)            // an ack, in order
func (c *Conn) Drain(d time.Duration) bool     // wait until everything queued is written
func (c *Conn) Close(err error)
func (c *Conn) Done() <-chan struct{}
func (c *Conn) Err() error

func (c *Conn) Stalled() bool
func (c *Conn) Heard() time.Time               // last byte read (monotonic)
func (c *Conn) SlowRTT() time.Duration         // second slowest of the last 8, at least 1s floor applied by callers
func (c *Conn) Offset() time.Duration          // peer clock minus ours
func (c *Conn) ToLocal(peerMs int64) int64     // a peer deadline in our clock
func (c *Conn) ToPeer(localMs int64) int64
func (c *Conn) Margin() time.Duration          // one slow RTT, at least 200ms
```

## Writer

One goroutine owns `w`. The queue holds ordered messages (`exec`, `relay`,
`ack`, `ping`, `pong`, and snapshots sent with `SendNow`) and one pending
slot per snapshot type (`state`, `view`). `Send` of a snapshot replaces the
pending one of its type; `SendNow` puts the snapshot in the ordered queue
and clears the pending one of its type. The writer drains the ordered
queue first, then the pending snapshots, encoding each into one line. A
full queue (`MaxQueue`) closes the stream with an error. Lines have no size
limit.

## Reader

One goroutine reads lines (a `bufio.Reader` with no line limit). Every byte
read updates `Heard` and clears a stall; a partial line counts. Pings are
answered with a pong (through the ordered queue) at once; pongs feed the
RTT window and the clock offset; acks resolve the waiting `Request`; every
other message goes to `OnMsg`, in order, on the reader goroutine. Handlers
must not block: anything slow is handed to a goroutine of the caller's.
Unknown types are logged once per type and dropped.

## Liveness

After `Live`, a ticker sends a ping every `o.Ping` and checks:

- **Silence**: no byte for `o.Silence` → `Close(ErrSilent)`.
- **Stall**: no byte for one ping interval plus `max(1s, 3 × SlowRTT)` (the
  hello's round trip before the first pong) → mark stalled and call `OnStall(true)`; the next byte
  clears it and calls `OnStall(false)`.

`towerd` decides what a stall means (probe, give up); the stream only marks
it.

**RTT and offset.** Each ping carries a sequence number; its pong carries
the peer's wall clock. The RTT window keeps the last 8 round trips;
`SlowRTT` is the second slowest. The offset is the midpoint estimate of the
ping with the smallest RTT among the last 8, skipping pings that spanned a
stall.

## Requests

`Request` assigns an id if the request has none, keeps back the margin
(one slow round trip, at least 200ms) for the answer, converts the
deadline to the peer's clock, queues it, and waits for the ack, the
context, the deadline, or the stream's end. A stall aborts every waiting
request at once with `ErrStalled`. The receiver gets the deadline in its
own clock and uses it as is.

## Concurrency

- Reader and writer goroutines; a liveness ticker; `Request` callers block
  on a per-request channel.
- State (RTT window, pending requests, stall mark) is under one mutex;
  callbacks are called without it.
- `Close` is idempotent: it closes `w`, fails pending requests, and closes
  `Done`. The reader ends on EOF or error and closes the stream.
