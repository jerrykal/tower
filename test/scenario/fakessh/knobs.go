package main

import (
	"os"
	"sync"
	"time"

	"github.com/jerrykal/tower/test/scenario/fakenet"
)

// Knobs is one fake host's link and faults.
type Knobs = fakenet.Knobs

const pollEvery = fakenet.PollEvery

func loadKnobs(alias string) (*Knobs, error) { return fakenet.Load(os.Getenv("TOWER_FAKE_DIR"), alias) }

// watch keeps a host's knobs current for a live connection.
type watch struct {
	alias string
	mu    sync.Mutex
	k     Knobs
	stop  chan struct{}
}

func newWatch(alias string, k *Knobs) *watch {
	w := &watch{alias: alias, k: *k, stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(pollEvery)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-t.C:
				if k, err := loadKnobs(alias); err == nil {
					w.mu.Lock()
					w.k = *k
					w.mu.Unlock()
				}
			}
		}
	}()
	return w
}

func (w *watch) get() Knobs {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.k
}

func (w *watch) close() { close(w.stop) }
