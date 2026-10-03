package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/relay"
	"github.com/jerrykal/tower/internal/tmux"
	"github.com/jerrykal/tower/internal/ui"
)

// Timings of the attach loop (protocol.md, "After an attach ends", "No
// flash between clients").
const (
	beatEvery     = 5 * time.Second        // the heartbeat
	seenSettle    = 150 * time.Millisecond // release this long after the home sees the new client
	releaseAfter  = 1500 * time.Millisecond
	backoffFirst  = time.Second
	backoffCap    = 30 * time.Second
	prepareRetry  = 4 * time.Second  // a failed prepare pauses at most this long
	stableAttach  = 10 * time.Second // attached this long starts the backoff afresh
	endClientWait = 3 * time.Second  // an old client ending, before it is killed
)

// Options configure the attach loop.
type Options struct {
	Dash bool        // tower dash: start at the picker; esc there attaches to the last target
	Env  *config.Env // the home: this machine and tmux server
}

// Run is the attach loop on this process's terminal (stdin and stdout):
// it attaches the terminal to one target at a time, as its home decides,
// until the user detaches, quits the picker or nothing is left. It
// returns the process's exit code.
func Run(ctx context.Context, o Options) int {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGTERM)
	defer stop()
	l, err := start(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tower:", err)
		return 1
	}
	code, note := l.run(ctx, o.Dash)
	l.close()
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	return code
}

// attachLoop is one loop: its terminal, its home and its attaches.
type attachLoop struct {
	env  *config.Env
	c    *client.Client
	id   string
	term *relay.Terminal
	tty  int         // the terminal's descriptor (stdin)
	orig relay.Modes // the terminal's modes as the loop found them
	last proto.Ref   // the home's last target when the loop started

	sync    bool // TOWER_SYNC: frame holds
	eager   bool // TOWER_EAGER: the loop ends the old client of a switch itself
	relayOn bool // TOWER_RELAY: remote attaches on a pty of the loop's own

	views *viewer
	sb    *standbys // nil: no standbys
	beat  chan struct{}

	mu   sync.Mutex
	gen  int
	cur  proto.Ref
	prev proto.Ref
	hold uint64 // the frame hold made last and not released, 0 for none

	pidFile string
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func start(ctx context.Context, o Options) (*attachLoop, error) {
	tty := int(os.Stdin.Fd())
	orig, err := relay.GetModes(tty)
	if err != nil {
		return nil, errors.New("the attach loop needs a terminal")
	}
	term, err := relay.NewTerminal(os.Stdin, os.Stdout)
	if err != nil {
		return nil, err
	}
	c := client.New(o.Env)
	st, err := c.Ensure(ctx)
	if err != nil {
		return nil, err
	}
	tmux.UseBin(st.TmuxBin)
	l := &attachLoop{
		env: o.Env, c: c, id: config.NewID(), term: term, tty: tty, orig: orig,
		sync:    config.Flag("TOWER_SYNC", true),
		eager:   config.Flag("TOWER_EAGER", true),
		relayOn: config.Flag("TOWER_RELAY", true),
		beat:    make(chan struct{}, 1),
	}
	var ack proto.LoopAck
	if err := l.call(ctx, proto.CallLoop, proto.LoopBeat{ID: l.id}, &ack); err != nil {
		return nil, fmt.Errorf("towerd: %w", err)
	}
	l.last = ack.Last
	l.pidFile = o.Env.State("loop-" + l.id + ".pid")
	config.WriteFile(l.pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"))

	bg, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.views = newViewer(l)
	l.wg.Add(2)
	go func() { defer l.wg.Done(); l.views.run(bg) }()
	go func() { defer l.wg.Done(); l.heartbeat(bg) }()
	if l.relayOn && config.Flag("TOWER_STANDBY", true) {
		l.sb = newStandbys(l)
		l.wg.Add(1)
		go func() { defer l.wg.Done(); l.sb.run(bg) }()
	}
	return l, nil
}

// close ends the standbys, says goodbye to the home, releases any hold
// and gives the terminal back as the loop found it.
func (l *attachLoop) close() {
	if l.sb != nil {
		l.sb.closeAll()
	}
	l.cancel()
	l.wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	l.call(ctx, proto.CallLoopBye, proto.LoopArgs{ID: l.id}, nil)
	cancel()
	l.release()
	l.restoreModes()
	os.Remove(l.pidFile)
}

func (l *attachLoop) call(ctx context.Context, op string, args, result any) error {
	return l.c.Call(ctx, op, args, result)
}

// heartbeat tells the home about the loop every beatEvery, and at once
// when asked (the home came back), so a restarted home relearns it.
func (l *attachLoop) heartbeat(ctx context.Context) {
	t := time.NewTicker(beatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.beat:
		}
		l.mu.Lock()
		b := proto.LoopBeat{ID: l.id, Gen: l.gen, Cur: l.cur, Prev: l.prev}
		l.mu.Unlock()
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		l.call(cctx, proto.CallLoop, b, nil)
		cancel()
	}
}

func (l *attachLoop) beatNow() {
	select {
	case l.beat <- struct{}{}:
	default:
	}
}

// Frame holds (protocol.md, "No flash between clients").

// holdFrame holds the terminal's frame (unless TOWER_SYNC=0) and returns
// the hold's number.
func (l *attachLoop) holdFrame() uint64 {
	if !l.sync {
		return 0
	}
	n := l.term.Hold()
	l.mu.Lock()
	l.hold = n
	l.mu.Unlock()
	return n
}

// releaseHold ends hold n, unless a newer one was made since.
func (l *attachLoop) releaseHold(n uint64) {
	if n == 0 {
		return
	}
	l.term.Release(n)
	l.mu.Lock()
	if l.hold == n {
		l.hold = 0
	}
	l.mu.Unlock()
}

// release ends the hold that is on, if any: before the picker and an exit.
func (l *attachLoop) release() {
	l.mu.Lock()
	n := l.hold
	l.mu.Unlock()
	l.releaseHold(n)
}

func (l *attachLoop) restoreModes() { relay.SetModes(l.tty, l.orig) }

func (l *attachLoop) rawModes() { relay.SetModes(l.tty, l.orig.Raw()) }

func (l *attachLoop) size() (int, int) {
	rows, cols, err := relay.GetSize(int(os.Stdout.Fd()))
	if err != nil || rows == 0 {
		return 24, 80
	}
	return rows, cols
}

// steps of the state machine.
type step int

const (
	stepPicker step = iota
	stepPrepare
)

// run is the state machine (protocol.md, "After an attach ends"). It
// returns the exit code and a note to print once the terminal is back.
func (l *attachLoop) run(ctx context.Context, dash bool) (int, string) {
	target := l.last
	st := stepPrepare
	if dash || target.IsZero() || config.Flag("TOWER_TEST_PICKER", false) {
		st = stepPicker
	}
	var (
		note    string        // for the picker, or the next client's status line
		from    proto.Ref     // where the last attach was: a failed hand-off goes back there
		handoff bool          // the target is a hand-off's, not the user's pick
		retry   bool          // reconnecting after a lost connection
		backoff time.Duration // the reconnect's next pause
	)
	for ctx.Err() == nil {
		if st == stepPicker {
			l.release()
			l.restoreModes()
			ch, err := ui.Pick(ctx, l.c, ui.PickOptions{Loop: l.id, Note: note, Dash: dash})
			dash, note, handoff, retry, backoff = false, "", false, false, 0
			switch {
			case errors.Is(err, ui.ErrQuit):
				return 0, ""
			case err != nil:
				if ctx.Err() != nil {
					return 0, ""
				}
				return 1, "tower: " + err.Error()
			case ch.Last:
				if target = l.last; target.IsZero() {
					return 0, ""
				}
			default:
				target = ch.Target
			}
			st = stepPrepare
		}

		p, err := l.prepare(ctx, target)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return 0, ""
			case retry:
				// The host is not back yet: pause, then try again.
				l.release()
				d := min(max(backoff, backoffFirst), prepareRetry)
				if l.pause(ctx, target, d, "tower: "+err.Error()) {
					st = stepPicker
				}
				backoff = nextBackoff(backoff)
				continue
			case handoff && !from.IsZero() && !from.SameSession(target):
				// A hand-off whose target cannot take it goes back where
				// the terminal was, saying why.
				target, note, handoff = from, err.Error(), false
				continue
			}
			note, st = err.Error(), stepPicker
			continue
		}

		res := l.attach(ctx, p, note)
		note = ""
		if res.err != nil {
			l.release()
			l.restoreModes()
			note, st = res.err.Error(), stepPicker
			continue
		}
		if res.gaveUp != "" {
			// Back where the terminal was, saying why; or the picker.
			if handoff && !from.IsZero() && !from.SameSession(target) {
				target, note, handoff, retry = from, res.gaveUp, false, false
				continue
			}
			note, st, retry = res.gaveUp, stepPicker, false
			continue
		}
		from = p.Target
		next := l.after(ctx, p.Gen, res)
		switch next.Do {
		case proto.NextHandoff:
			target, note, handoff, retry = next.Target, next.Note, true, false
		case proto.NextReconnect:
			target, handoff = next.Target, false
			if res.took > stableAttach || !retry {
				backoff = 0
			}
			retry = true
			if backoff > 0 {
				l.release()
				if l.pause(ctx, target, backoff, fmt.Sprintf("tower: lost %s; reconnecting", target.String())) {
					st = stepPicker
					continue
				}
			}
			backoff = nextBackoff(backoff)
		case proto.NextPicker:
			note, st, retry = next.Note, stepPicker, false
		default: // exit
			return 0, next.Note
		}
	}
	return 0, ""
}

func nextBackoff(d time.Duration) time.Duration {
	if d == 0 {
		return backoffFirst
	}
	return min(2*d, backoffCap)
}

// prepare asks the home for the attach to target.
func (l *attachLoop) prepare(ctx context.Context, target proto.Ref) (*proto.Prepared, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if l.sb != nil {
		l.sb.reserve(target.Host)
	}
	var p proto.Prepared
	if err := l.call(cctx, proto.CallPrepare, proto.PrepareArgs{Loop: l.id, Target: target}, &p); err != nil {
		if l.sb != nil {
			l.sb.reserve("")
		}
		return nil, err
	}
	l.mu.Lock()
	l.gen = p.Gen
	if !l.cur.IsZero() && !l.cur.SameSession(p.Target) {
		l.prev = l.cur
	}
	l.cur = p.Target
	l.mu.Unlock()
	return &p, nil
}

// after tells the home how the attach ended and returns what comes next.
// A home that cannot be reached means the picker.
func (l *attachLoop) after(ctx context.Context, gen int, res attachResult) *proto.Next {
	var next proto.Next
	for i := 0; ; i++ {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := l.call(cctx, proto.CallAfter, proto.AfterArgs{Loop: l.id, Gen: gen, Code: res.code, Ended: res.ended}, &next)
		cancel()
		if err == nil {
			return &next
		}
		if ctx.Err() != nil || i == 2 {
			return &proto.Next{Do: proto.NextPicker, Note: "towerd: " + err.Error()}
		}
		// A home being replaced: ensure it and say who we are again.
		ectx, ecancel := context.WithTimeout(ctx, 5*time.Second)
		l.c.Ensure(ectx)
		ecancel()
		l.beatNow()
		time.Sleep(200 * time.Millisecond)
	}
}

// pause waits d before a reconnect, showing why. It ends early when the
// home's view shows a new link to the target's host; it returns true
// when the user pressed ctrl-c (the picker).
func (l *attachLoop) pause(ctx context.Context, target proto.Ref, d time.Duration, why string) bool {
	l.rawModes()
	fmt.Fprintf(os.Stdout, "\r\n%s (ctrl-c: the picker)\r\n", why)
	link := 0
	if v, _ := l.views.get(); v != nil {
		if h := v.View.HostByID(target.Host); h != nil {
			link = h.Link
		}
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	keys := make(chan bool, 1)
	go func() { keys <- waitCtrlC(ctx, l.tty) }()
	for {
		v, changed := l.views.get()
		if v != nil {
			if h := v.View.HostByID(target.Host); h != nil && h.Link > link && h.Reachable() {
				return false
			}
		}
		select {
		case <-ctx.Done():
			return false
		case c := <-keys:
			if c {
				return true
			}
		case <-changed:
		}
	}
}
