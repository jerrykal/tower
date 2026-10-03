package towerd

import (
	"sync"
	"time"
)

// pacer runs fn when kicked, paced by a token bucket: a kick after a
// quiet spell runs at once, a burst runs burst times at once and then once
// per every. Kicks that arrive while fn waits or runs fold into one more
// run, so fn always sees everything kicked before it started.
type pacer struct {
	every time.Duration
	burst int
	fn    func()

	kick chan struct{}
	stop chan struct{}
	once sync.Once
}

func newPacer(every time.Duration, burst int, fn func()) *pacer {
	p := &pacer{every: every, burst: burst, fn: fn, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	go p.run()
	return p
}

// Kick asks for a run; it never blocks.
func (p *pacer) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Stop ends the pacer; a run under way finishes.
func (p *pacer) Stop() { p.once.Do(func() { close(p.stop) }) }

func (p *pacer) run() {
	tokens := float64(p.burst)
	last := time.Now()
	for {
		select {
		case <-p.kick:
		case <-p.stop:
			return
		}
		now := time.Now()
		tokens = min(float64(p.burst), tokens+float64(now.Sub(last))/float64(p.every))
		last = now
		if tokens < 1 {
			wait := time.Duration((1 - tokens) * float64(p.every))
			select {
			case <-time.After(wait):
			case <-p.stop:
				return
			}
			now = time.Now()
			tokens = min(float64(p.burst), tokens+float64(now.Sub(last))/float64(p.every))
			last = now
		}
		tokens--
		// Kicks that came in while waiting are covered by this run.
		select {
		case <-p.kick:
		default:
		}
		p.fn()
	}
}
