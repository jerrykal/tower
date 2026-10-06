package towerd

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/stream"
)

const (
	reapRetry   = 5 * time.Second // a detach not done is asked again after this
	reapOldPeer = time.Hour       // or after this, from a towerd that does not know it
)

// reapState is one stale client the home is detaching.
type reapState struct {
	next  time.Time // when to ask (again)
	tries int
}

// reapStaleLocked detaches this home's clients on remotes whose attach is
// over: their loop has prepared another, or the loop is gone. A loop ends
// a remote attach by ending its ssh, which rides a control master, and
// the session there can outlive it: tmux keeps that client attached,
// drawing for nobody (decision 112). Clients on this machine are left alone, since the
// loop detaches them itself and a client whose loop died still has the
// terminal. Hosts not up are skipped: their clients wait for the link.
// Call with mu held.
func (h *homeRole) reapStaleLocked() {
	now := time.Now()
	listed := map[string]bool{}
	for hostID, cs := range h.clientsB {
		if hostID == h.d.id {
			continue
		}
		if hs := h.hostOf(hostID); hs == nil || hs.unreachable() != "" || hs.link.conn == nil {
			continue
		}
		for _, c := range cs {
			if c.Home != h.d.id {
				continue
			}
			key := reapKey(hostID, c)
			listed[key] = true
			if !h.staleLocked(key, c, now) {
				continue
			}
			r := h.reaping[key]
			if r == nil {
				r = &reapState{}
				h.reaping[key] = r
			}
			if now.Before(r.next) {
				continue
			}
			r.next, r.tries = now.Add(reapRetry), r.tries+1
			go h.reap(hostID, c, key, r.tries)
		}
	}
	for key := range h.reaping {
		if !listed[key] {
			delete(h.reaping, key)
		}
	}
	for key := range h.orphans {
		if !listed[key] {
			delete(h.orphans, key)
		}
	}
}

// staleLocked reports whether c's attach is over. The home numbers a
// loop's attaches (prepare), so a client of any other generation than the
// loop's is not its current one: an older attach, or one from before the
// home restarted and numbered afresh. A client whose loop the home does
// not know is stale only once it has stayed unknown for loopTTL: a home
// that restarted, or that called a slow loop gone, learns a live loop
// again from its next beat.
func (h *homeRole) staleLocked(key string, c proto.Client, now time.Time) bool {
	if l := h.loops[c.Loop]; l != nil {
		delete(h.orphans, key)
		return c.Gen != l.gen
	}
	since, ok := h.orphans[key]
	if !ok {
		h.orphans[key] = now
		return false
	}
	return now.Sub(since) > loopTTL
}

func reapKey(hostID string, c proto.Client) string {
	return hostID + "|" + c.Name + "|" + strconv.Itoa(c.Pid)
}

// reap asks c's host to detach it; the try'th time for this client.
func (h *homeRole) reap(hostID string, c proto.Client, key string, try int) {
	ctx, cancel := context.WithTimeout(h.ctx, ackTimeout())
	defer cancel()
	h.d.mu.Lock()
	inst := ""
	if l := h.linkByIDLocked(hostID); l != nil {
		inst = l.inst
	}
	h.d.mu.Unlock()
	req := &proto.Request{
		ID:       config.NewID() + config.NewID(),
		Op:       proto.OpDetach,
		Deadline: stream.Now() + ackTimeout().Milliseconds(),
		Target:   proto.Ref{Host: hostID, Inst: inst},
		Client:   proto.ClientID{Pid: c.Pid, Name: c.Name}.String(),
		Loop:     c.Loop,
		Gen:      c.Gen,
		From:     h.d.id,
	}
	ack := h.route(ctx, req)
	if !ack.OK && strings.Contains(ack.Err, "unknown request") {
		// A towerd from before detach: ask again only after a while.
		h.d.mu.Lock()
		if r := h.reaping[key]; r != nil {
			r.next = time.Now().Add(reapOldPeer)
		}
		h.d.mu.Unlock()
	}
	if ack.OK || try == 1 {
		h.d.logf("home: detach stale client %s (loop %s gen %d) on %s: ok=%v %s%s", c.Name, c.Loop, c.Gen, hostID, ack.OK, ack.Err, ack.Note)
	}
}
