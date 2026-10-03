package loop

import (
	"context"
	"os"
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

// watchCtrlC watches the terminal (raw) for the ctrl-c byte: hit is
// closed when it comes. stop ends the watch and returns once it has
// ended, so nothing typed after stop is taken from whatever has the
// terminal next.
func watchCtrlC(fd int) (hit <-chan struct{}, stop func()) {
	h := make(chan struct{})
	r, w, err := os.Pipe()
	if err != nil {
		return h, func() {}
	}
	quit := int(r.Fd())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(quit), Events: unix.POLLIN}}
		var b [1]byte
		for {
			fds[0].Revents, fds[1].Revents = 0, 0
			if _, err := unix.Poll(fds, -1); err == unix.EINTR {
				continue
			} else if err != nil {
				return
			}
			if fds[1].Revents != 0 || fds[0].Revents&unix.POLLIN == 0 {
				return // stopped, or the terminal hung up
			}
			if n, _ := unix.Read(fd, b[:]); n == 1 && b[0] == 3 {
				close(h)
				return
			}
		}
	}()
	return h, func() {
		w.Close()
		<-done
		r.Close()
	}
}

// sameModes reports whether the terminal's modes now are those a standby
// was made with (raw mode and the kernel's state bits aside).
func sameModes(fd int, m relay.Modes) bool {
	now, err := relay.GetModes(fd)
	return err == nil && now.Same(m)
}
