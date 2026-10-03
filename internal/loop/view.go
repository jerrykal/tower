package loop

import (
	"context"
	"sync"
	"time"

	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/relay"
	"golang.org/x/sys/unix"
)

// viewer keeps the home's latest view for the loop: whether the home has
// seen an attach's client (the fallback release), its hosts and links
// (standbys, the reconnect's pause), and the loop's own current and
// previous target as the home keeps them (the heartbeat's copy).
type viewer struct {
	l       *attachLoop
	mu      sync.Mutex
	d       *proto.Dash
	changed chan struct{} // closed and replaced on every new view
}

func newViewer(l *attachLoop) *viewer { return &viewer{l: l, changed: make(chan struct{})} }

// get returns the latest view (nil before the first) and a channel closed
// when a newer one arrives.
func (v *viewer) get() (*proto.Dash, <-chan struct{}) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.d, v.changed
}

func (v *viewer) set(d *proto.Dash) {
	v.mu.Lock()
	v.d = d
	close(v.changed)
	v.changed = make(chan struct{})
	v.mu.Unlock()
	if lp := d.View.LoopByID(v.l.id); lp != nil {
		l := v.l
		l.mu.Lock()
		if lp.Gen == l.gen {
			l.cur, l.prev = lp.Cur, lp.Prev
		}
		l.mu.Unlock()
	}
}

// run reads the view on every change of the home's (watch), until ctx
// ends.
func (v *viewer) run(ctx context.Context) {
	for ctx.Err() == nil {
		var d proto.Dash
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := v.l.call(cctx, proto.CallView, proto.ViewArgs{Loop: v.l.id}, &d)
		cancel()
		if err != nil {
			sleepCtx(ctx, 500*time.Millisecond)
			continue
		}
		v.set(&d)
		var w proto.WatchResult
		cctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		err = v.l.call(cctx, proto.CallWatch, proto.WatchArgs{Gen: d.Gen}, &w)
		cancel()
		if err != nil {
			sleepCtx(ctx, 500*time.Millisecond)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// readKey reads one byte from the terminal fd once one is there, until
// ctx ends.
func readKey(ctx context.Context, fd int) (byte, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for ctx.Err() == nil {
		fds[0].Revents = 0
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR || n == 0 {
			continue
		}
		if err != nil {
			return 0, err
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			return 0, unix.EIO
		}
		var b [1]byte
		if k, err := unix.Read(fd, b[:]); k == 1 {
			return b[0], nil
		} else if err != nil && err != unix.EAGAIN && err != unix.EINTR {
			return 0, err
		}
	}
	return 0, ctx.Err()
}

// sameModes reports whether the terminal's modes now are those a standby
// was made with (raw mode and the kernel's state bits aside).
func sameModes(fd int, m relay.Modes) bool {
	now, err := relay.GetModes(fd)
	return err == nil && now.Same(m)
}
