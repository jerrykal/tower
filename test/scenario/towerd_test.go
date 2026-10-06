package scenario

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/stream"
)

// Helpers for scenarios that drive towerd directly: its process, its
// homes and registrations, and calls a dashboard or a loop would make.

// TowerdPid is this machine's pid of the process in the host's
// towerd.pid, or 0.
func (h *Host) TowerdPid() int {
	b, err := os.ReadFile(h.Paths().State("towerd.pid"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return h.HostPid(n)
}

// Alive reports whether pid runs.
func Alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// LiveHomes are the homes connected to the host's towerd.
func (h *Host) LiveHomes() []proto.HomeStatus {
	st := h.Status()
	if st == nil {
		return nil
	}
	var out []proto.HomeStatus
	for _, r := range st.Detail.Homes {
		if r.Live {
			out = append(out, r)
		}
	}
	return out
}

// Regs are the host's registrations.
func (h *Host) Regs() []proto.Client {
	if st := h.Status(); st != nil {
		return st.Detail.Clients
	}
	return nil
}

// HiddenSessions are the host's _tower sessions with their attached
// counts ("_tower:1").
func (h *Host) HiddenSessions() []string {
	out, _ := h.Tmux("list-sessions", "-F", "#{session_name}:#{session_attached}")
	var s []string
	for _, l := range strings.Fields(out) {
		if strings.HasPrefix(l, "_tower") {
			s = append(s, l)
		}
	}
	return s
}

// View is the dashboard's view call on the host's towerd for client (a
// TOWER_CLIENT value, or "").
func (h *Host) View(client string) *proto.Dash {
	var d proto.Dash
	if err := h.Call(proto.CallView, proto.ViewArgs{Client: client, Look: true}, &d); err != nil { // as a dashboard reads
		return nil
	}
	return &d
}

// HasSession reports whether the view lists session name on the host
// named host.
func HasSession(v *proto.View, host, name string) bool {
	if v == nil {
		return false
	}
	for _, hs := range v.Hosts {
		if hs.Name != host {
			continue
		}
		for _, s := range hs.Sessions {
			if s.Name == name {
				return true
			}
		}
	}
	return false
}

// HostIn is the host named name in a view.
func HostIn(v *proto.View, name string) *proto.Host {
	if v == nil {
		return nil
	}
	for i := range v.Hosts {
		if v.Hosts[i].Name == name {
			return &v.Hosts[i]
		}
	}
	return nil
}

// Ref is the target of session name on host target as home's view lists
// it.
func (w *World) Ref(home *Host, target, session string) proto.Ref {
	w.T.Helper()
	v := home.View("")
	hs := HostIn(&v.View, target)
	if hs == nil {
		w.T.Fatalf("%s's view has no host %s", home.Name, target)
	}
	for _, s := range hs.Sessions {
		if s.Name == session {
			return proto.Ref{Host: hs.ID, Name: hs.Name, Inst: hs.Inst, Session: s.ID, Label: s.Name}
		}
	}
	w.T.Fatalf("%s's view has no %s:%s", home.Name, target, session)
	return proto.Ref{}
}

// Act sends a dashboard's request to h's towerd as client, with a
// deadline ms from now (0: towerd's default).
func (h *Host) Act(client string, req proto.Request, ms int) (*proto.Ack, time.Duration) {
	req.Client = client
	if req.ID == "" {
		req.ID = config.NewID() + config.NewID()
	}
	if ms > 0 {
		req.Deadline = stream.Now() + int64(ms)
	}
	var ack proto.Ack
	start := time.Now()
	if err := h.Call(proto.CallAct, req, &ack); err != nil {
		return &proto.Ack{Err: err.Error()}, time.Since(start)
	}
	return &ack, time.Since(start)
}

// TowerBin runs another tower binary (an upgrade) on the host.
func (h *Host) TowerBin(bin string, args ...string) (string, error) {
	return h.Run(nil, 20*time.Second, append([]string{bin}, args...)...)
}

// ClientName is the tmux client name in a TOWER_CLIENT value.
func ClientName(id string) string {
	f := strings.SplitN(id, ":", 3)
	if len(f) != 3 {
		return ""
	}
	return f[2]
}

// Kill9 SIGKILLs pid.
func Kill9(pid int) { syscall.Kill(pid, syscall.SIGKILL) }

// WaitGone waits until pid has exited.
func (w *World) WaitGone(pid int, d time.Duration) bool {
	start := time.Now()
	for time.Since(start) < d {
		if !Alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TowerdProcs are the towerd processes (bridges left out) running with
// the host's TOWER_HOME and tmux socket.
func (h *Host) TowerdProcs() []int {
	return procsWhere(func(p proc) bool {
		if home, _ := p.getenv("TOWER_HOME"); home != h.HomeDir {
			return false
		}
		if len(p.argv) < 2 || p.argv[1] != "towerd" || slices.Contains(p.argv, "--stdio") {
			return false
		}
		tm, _ := p.getenv("TOWER_TMUX")
		return hasArgs(p.argv, "--tmux", "-L "+h.Sock) || !slices.Contains(p.argv, "--tmux") && tm == "-L "+h.Sock
	})
}

// Loop drives towerd's loop calls the way an attach loop does, without
// the terminal UI: beats, prepare, the attach in a terminal of its own,
// wait-switch, held and after. Scenarios use it where only towerd's side
// of a hand-off is under test.
type FakeLoop struct {
	w    *World
	Home *Host
	ID   string
	Gen  int
	Term *Term
	// KeepOld keeps earlier attaches' terminals running.
	KeepOld bool

	mu   sync.Mutex
	cur  proto.Ref
	prev proto.Ref
	stop chan struct{}
	n    int
}

// FakeLoop starts beating at home as a new loop.
func (w *World) FakeLoop(home *Host) *FakeLoop {
	l := &FakeLoop{w: w, Home: home, ID: config.NewID(), stop: make(chan struct{})}
	l.beat()
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				l.beat()
			}
		}
	}()
	w.T.Cleanup(func() { close(l.stop) })
	return l
}

func (l *FakeLoop) beat() {
	l.mu.Lock()
	b := proto.LoopBeat{ID: l.ID, Gen: l.Gen, Cur: l.cur, Prev: l.prev}
	l.mu.Unlock()
	l.Home.Call(proto.CallLoop, b, nil)
}

// Status is the loop at its home, or nil.
func (l *FakeLoop) Status() *proto.LoopStatus {
	for _, s := range l.w.Loops(l.Home) {
		if s.ID == l.ID {
			return &s
		}
	}
	return nil
}

// Prepare asks the home to prepare target.
func (l *FakeLoop) Prepare(target proto.Ref) (*proto.Prepared, error) {
	var p proto.Prepared
	err := l.Home.Call(proto.CallPrepare, proto.PrepareArgs{Loop: l.ID, Target: target}, &p)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.Gen = p.Gen
	if !l.cur.IsZero() && !l.cur.SameSession(p.Target) {
		l.prev = l.cur
	}
	l.cur = p.Target
	l.mu.Unlock()
	return &p, nil
}

// Attach prepares target and runs the attach in a new terminal, waiting
// until the target host has a client on it. The previous attach's
// terminal ends first, as a loop ends its old client.
func (l *FakeLoop) Attach(target proto.Ref, on *Host) *Term {
	l.w.T.Helper()
	p, err := l.Prepare(target)
	if err != nil {
		l.w.T.Fatalf("prepare %s: %v", target.String(), err)
	}
	if l.Term != nil && !l.KeepOld {
		killSessions(l.Term.Sock)
	}
	l.n++
	t := l.w.Term(fmt.Sprintf("%s-%d", l.ID[:4], l.n), l.Home, nil, p.Argv...)
	l.Term = t
	l.w.Eventually(8*time.Second, "a client on "+target.String(), func() bool {
		for _, c := range on.Clients() {
			if c == target.Label {
				return true
			}
		}
		return false
	})
	// The home sees the attach's client (what releases a loop's frame
	// hold when the new client draws nothing).
	l.w.Eventually(3*time.Second, "the home sees "+target.String()+"'s client", func() bool {
		s := l.Status()
		return s != nil && s.Gen == p.Gen && s.Seen
	})
	// As a loop's attach, settled.
	time.Sleep(300 * time.Millisecond)
	return t
}

// After reports the attach's end.
func (l *FakeLoop) After(code int, ended bool) *proto.Next {
	var n proto.Next
	if err := l.Home.Call(proto.CallAfter, proto.AfterArgs{Loop: l.ID, Gen: l.Gen, Code: code, Ended: ended}, &n); err != nil {
		return &proto.Next{Do: "error", Note: err.Error()}
	}
	return &n
}

// WaitSwitch calls wait-switch for the current attach in the background.
func (l *FakeLoop) WaitSwitch() <-chan bool {
	ch := make(chan bool, 1)
	gen := l.Gen
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var r proto.SwitchWake
		err := l.Home.Client().Call(ctx, proto.CallWaitSwitch, proto.GenArgs{Loop: l.ID, Gen: gen}, &r)
		ch <- err == nil && r.Switch
	}()
	return ch
}

// Held confirms the hold.
func (l *FakeLoop) Held() bool {
	var r proto.HeldReply
	l.Home.Call(proto.CallHeld, proto.GenArgs{Loop: l.ID, Gen: l.Gen}, &r)
	return r.End
}

// Handoff is a dashboard's ⏎ on target from the loop's client on
// from:session, as towerd sees it: the switch asked of from's towerd
// (relayed to the home), the loop's after with exit 42, and the attach
// that follows on to.
func (l *FakeLoop) Handoff(from *Host, session string, target proto.Ref, to *Host) {
	l.w.T.Helper()
	ids := from.ClientIDs(session)
	if len(ids) == 0 {
		l.w.T.Fatalf("no client on %s:%s", from.Name, session)
	}
	ack, _ := from.Act(ids[0], proto.Request{Op: proto.OpSwitch, Target: target, Nonce: config.NewID()}, 0)
	if !ack.OK {
		l.w.T.Fatalf("switch to %s: %s", target.String(), ack.Err)
	}
	next := l.After(42, false)
	if next.Do != proto.NextHandoff || next.Target.Session != target.Session {
		l.w.T.Fatalf("after the switch to %s: %+v", target.String(), next)
	}
	l.Attach(next.Target, to)
}

// Bye ends the loop at its home.
func (l *FakeLoop) Bye() { l.Home.Call(proto.CallLoopBye, proto.LoopArgs{ID: l.ID}, nil) }
