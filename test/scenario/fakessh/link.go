package main

import (
	"io"
	"math/rand/v2"
	"sync"
	"time"
)

// pipe copies one direction of a connection the way a shaped network
// would: each chunk delivered delay ± jitter after it was read (order
// kept), at most the bandwidth, at most a window of bytes in flight, and
// nothing at all while the link is stalled or frozen.
type pipe struct {
	w        *watch
	frozen   func() bool // the connection is half-open: nothing moves
	mu       sync.Mutex
	cond     *sync.Cond
	inflight int
}

type chunk struct {
	b  []byte
	at time.Time
}

func newPipe(w *watch, frozen func() bool) *pipe {
	p := &pipe{w: w, frozen: frozen}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// run copies src to dst until src ends, then calls done (which usually
// closes dst's write side).
func (p *pipe) run(dst io.Writer, src io.Reader, done func()) {
	q := make(chan chunk, 4096)
	go func() {
		defer close(q)
		var last time.Time
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				k := p.w.get()
				p.mu.Lock()
				for p.inflight > 0 && p.inflight+n > k.Window() {
					p.cond.Wait()
					k = p.w.get()
				}
				p.inflight += n
				p.mu.Unlock()
				d := time.Duration(k.DelayMs) * time.Millisecond
				if k.JitterMs > 0 {
					d += time.Duration(rand.IntN(2*k.JitterMs+1)-k.JitterMs) * time.Millisecond
				}
				at := time.Now().Add(max(d, 0))
				if k.BwKBps > 0 {
					tx := time.Duration(float64(n) / float64(k.BwKBps<<10) * float64(time.Second))
					if s := last.Add(tx); s.After(at) {
						at = s
					}
				}
				if at.Before(last) {
					at = last
				}
				last = at
				q <- chunk{b: append([]byte(nil), buf[:n]...), at: at}
			}
			if err != nil {
				return
			}
		}
	}()
	for c := range q {
		for {
			k := p.w.get()
			if !k.Stall && !p.frozen() {
				break
			}
			time.Sleep(pollEvery)
		}
		if d := time.Until(c.at); d > 0 {
			time.Sleep(d)
		}
		for {
			k := p.w.get()
			if !k.Stall && !p.frozen() {
				break
			}
			time.Sleep(pollEvery)
		}
		_, err := dst.Write(c.b)
		p.mu.Lock()
		p.inflight -= len(c.b)
		p.cond.Broadcast()
		p.mu.Unlock()
		if err != nil {
			// The reader is gone; drain so the source side never blocks.
			for c := range q {
				p.mu.Lock()
				p.inflight -= len(c.b)
				p.cond.Broadcast()
				p.mu.Unlock()
			}
			break
		}
	}
	if done != nil {
		done()
	}
}
