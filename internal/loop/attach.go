package loop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/relay"
	"github.com/jerrykal/tower/internal/tmux"
	"github.com/jerrykal/tower/internal/transport"
)

// standbyWait is how long a standby has to answer its go line: 300ms plus
// two of the link's slow round trips, or TOWER_STANDBY_TIMEOUT.
func standbyWait(p *proto.Prepared) time.Duration {
	return config.Duration("TOWER_STANDBY_TIMEOUT", 300*time.Millisecond+2*time.Duration(p.RTT)*time.Millisecond)
}

// attachResult is how an attach ended.
type attachResult struct {
	code   int
	ended  bool          // the loop ended the client for a stored switch
	took   time.Duration // how long it ran
	err    error         // it could not start
	gaveUp string        // its host stopped answering before its client was seen
}

// running is one attach in progress, whatever runs it.
type running struct {
	done chan int // its exit status, once
	end  func()   // ends the client for a switch
	kill func()   // ends it for certain
	// restore writes the client's terminal restore when the client went
	// without writing its own: a remote client hung up or lost.
	restore func(code int, ended bool)
}

// attach runs one attach to its end, with wait-switch alongside: a switch
// stored for it holds the frame, confirms, and (when the home says so)
// ends the client here. The old client gone, the frame is held again for
// whatever comes next.
func (l *attachLoop) attach(ctx context.Context, p *proto.Prepared, note string) attachResult {
	config.Mark("attach")
	start := time.Now()
	var r *running
	var err error
	switch {
	case p.Local:
		r, err = l.startChild(withNoteArgv(p.Argv, note, false), true)
	case !l.relayOn:
		r, err = l.startChild(withNoteArgv(p.Argv, note, true), false)
	default:
		r, err = l.startRelayed(p, note)
	}
	if l.sb != nil {
		l.sb.reserve("")
	}
	if err != nil {
		return attachResult{err: err}
	}
	actx, acancel := context.WithCancel(ctx)
	defer acancel()
	l.mu.Lock()
	hold := l.hold
	l.mu.Unlock()
	if hold != 0 {
		go l.releaseLater(actx, hold, p.Gen)
	}
	sw := make(chan struct{}, 1)
	if l.eager {
		// With nobody waiting, the home leaves ending the client to the
		// dashboard.
		go l.waitSwitch(actx, p.Gen, sw)
	}
	lost := make(chan string, 1)
	if !p.Local {
		go l.watchHost(actx, p, lost)
	}

	ended := false
	gaveUp := ""
	var code int
wait:
	for {
		select {
		case code = <-r.done:
			break wait
		case why := <-lost:
			// Its host stalled or went down before the client came up:
			// the attach would hang; give it up now.
			config.Mark("attach given up: " + why)
			gaveUp = why
			r.kill()
			code = <-r.done
			break wait
		case <-sw:
			n := l.holdFrame()
			config.Mark("switch stored: hold")
			var hr proto.HeldReply
			hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			l.call(hctx, proto.CallHeld, proto.GenArgs{Loop: l.id, Gen: p.Gen}, &hr)
			cancel()
			if hr.End {
				config.Mark("end attach")
				ended = true
				r.end()
				code = waitDone(r, endClientWait)
				break wait
			}
			// The dashboard ends the client; should nothing end it, the
			// hold does not outstay its welcome.
			go func() {
				select {
				case <-time.After(releaseAfter):
					l.releaseHold(n)
				case <-actx.Done():
				}
			}()
		case <-ctx.Done():
			r.end()
			code = waitDone(r, endClientWait)
			break wait
		}
	}
	acancel()
	config.Mark("exited " + strconv.Itoa(code))
	l.holdFrame()
	r.restore(code, ended)
	if ended {
		code = 42
	}
	return attachResult{code: code, ended: ended, took: time.Since(start), gaveUp: gaveUp}
}

// watchHost says why, should the attach's host stop answering (stalled,
// down) before the home has seen the attach's client.
func (l *attachLoop) watchHost(ctx context.Context, p *proto.Prepared, lost chan<- string) {
	for {
		d, changed := l.views.get()
		if d != nil {
			if lp := d.View.LoopByID(l.id); lp != nil && lp.Gen == p.Gen && lp.Seen {
				return
			}
			if h := d.View.HostByID(p.Target.Host); h != nil && !h.Reachable() {
				why := h.Name + " is " + h.Status
				switch {
				case h.Status == proto.StatusStalled:
					why = h.Name + " is not responding"
				case h.Reason != "":
					why += ": " + h.Reason
				}
				lost <- why
				return
			}
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
	}
}

// waitDone waits d for the attach to end, then kills it.
func waitDone(r *running, d time.Duration) int {
	select {
	case code := <-r.done:
		return code
	case <-time.After(d):
		r.kill()
		return <-r.done
	}
}

// waitSwitch asks the home, for as long as the attach runs, to say when
// a switch is stored for it. A home that answers at once without one has
// forgotten the loop (it restarted): the loop beats, then asks again.
func (l *attachLoop) waitSwitch(ctx context.Context, gen int, out chan<- struct{}) {
	for ctx.Err() == nil {
		start := time.Now()
		var w proto.SwitchWake
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := l.call(cctx, proto.CallWaitSwitch, proto.GenArgs{Loop: l.id, Gen: gen}, &w)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil && w.Switch {
			out <- struct{}{}
			return
		}
		if time.Since(start) < 500*time.Millisecond {
			l.beatNow()
			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
}

// releaseLater is the fallback release of hold n for the attach gen: 150ms
// after the home sees its client, or after 1.5s. The new client's own
// first synced frame usually ends the hold before.
func (l *attachLoop) releaseLater(ctx context.Context, n uint64, gen int) {
	deadline := time.NewTimer(releaseAfter)
	defer deadline.Stop()
	for {
		d, changed := l.views.get()
		if d != nil {
			if lp := d.View.LoopByID(l.id); lp != nil && lp.Gen == gen && lp.Seen {
				select {
				case <-time.After(seenSettle):
				case <-deadline.C:
				case <-ctx.Done():
					return
				}
				l.releaseHold(n)
				return
			}
		}
		select {
		case <-changed:
		case <-deadline.C:
			l.releaseHold(n)
			return
		case <-ctx.Done():
			return
		}
	}
}

// startChild runs argv with the terminal as its stdio: a local attach
// (the shim, which execs tmux), or with TOWER_RELAY=0 ssh given the
// terminal.
func (l *attachLoop) startChild(argv []string, local bool) (*running, error) {
	l.restoreModes()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	r := &running{done: make(chan int, 1)}
	go func() {
		cmd.Wait()
		r.done <- exitStatus(cmd.ProcessState)
	}()
	r.kill = func() { cmd.Process.Kill() }
	if local {
		r.end = func() { l.detachLocal(cmd.Process.Pid) }
		r.restore = func(int, bool) {}
	} else {
		r.end = func() { cmd.Process.Signal(syscall.SIGTERM) }
		r.restore = l.remoteRestore(nil)
	}
	return r, nil
}

// remoteRestore writes a remote client's terminal restore when its own
// never came: the loop hung it up for a switch, or the connection was
// lost; not for a session that drew nothing (drawn nil: unknown).
func (l *attachLoop) remoteRestore(drawn func() bool) func(int, bool) {
	return func(code int, ended bool) {
		if (ended || code == 255) && (drawn == nil || drawn()) {
			l.term.RestoreClient()
		}
	}
}

func exitStatus(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// detachLocal ends the loop's local tmux client (the shim's pid became
// the client) through its server, so tmux restores the terminal itself;
// with -E the client prints nothing as it goes.
func (l *attachLoop) detachLocal(pid int) {
	sv := tmux.Server{Args: l.env.Tmux}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, _ := sv.Run(ctx, "list-clients", "-F", "#{client_pid} #{client_name}")
	for _, line := range strings.Split(out, "\n") {
		p, name, ok := strings.Cut(line, " ")
		if ok && p == strconv.Itoa(pid) {
			if _, err := sv.Run(ctx, "detach-client", "-t", name, "-E", "exit 42"); err == nil {
				return
			}
		}
	}
	// No such client yet (the shim had not become it): end the shim.
	syscall.Kill(pid, syscall.SIGTERM)
}

// startRelayed runs a remote attach on a pty of the loop's own, relayed:
// the host's standby when it is ready and made for this terminal, else a
// new session with the attach's command.
func (l *attachLoop) startRelayed(p *proto.Prepared, note string) (*running, error) {
	rows, cols := l.size()
	var s *relay.Session
	if l.sb != nil {
		if sb := l.sb.take(p.Target.Host, p.Key); sb != nil {
			config.Mark("attach: standby")
			sb.SetSize(rows, cols)
			config.Mark("standby: go")
			err := sb.Send([]byte(withNoteGo(p.Go, note)))
			if err == nil {
				_, err = sb.ReadUntil([]byte(relay.MarkerGo), standbyWait(p))
			}
			if err == nil {
				s = sb
			} else {
				config.Mark("standby did not answer")
				sb.Kill()
				go sb.Close()
			}
			l.sb.kick()
		}
	}
	if s == nil {
		config.Mark("attach: session")
		var err error
		s, err = relay.Start(withNoteArgv(p.Argv, note, true), os.Environ(), l.orig, rows, cols)
		if err != nil {
			return nil, err
		}
	}
	l.rawModes()
	// Ending the client for a switch hangs ssh up and stops relaying at
	// once: the next client need not wait for ssh to go.
	end := func() {
		s.Terminate()
		s.Abandon()
	}
	drawn := func() bool { return s.Relayed() > 0 }
	r := &running{done: make(chan int, 1), end: end, kill: s.Kill, restore: l.remoteRestore(drawn)}
	go func() {
		code, err := s.Relay(l.term)
		switch {
		case errors.Is(err, relay.ErrAbandoned):
			go reap(s)
		case err != nil:
			// The terminal is gone: so is the attach.
			s.Kill()
			code = 255
			s.Close()
		default:
			s.Close()
		}
		r.done <- code
	}()
	return r, nil
}

// reap waits for an abandoned session's ssh to exit, a few seconds at
// most, then releases it.
func reap(s *relay.Session) {
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
	}
	s.Close()
}

// withNoteArgv adds the note to an attach command: the shim's own argv
// (local), or the remote command after ssh's "--" (its last word).
func withNoteArgv(argv []string, note string, remote bool) []string {
	if note == "" {
		return argv
	}
	out := append([]string(nil), argv...)
	if remote {
		out[len(out)-1] += " --note " + transport.ShellQuote(note)
		return out
	}
	return append(out, "--note", note)
}

// withNoteGo adds the note to a standby's go line.
func withNoteGo(line, note string) string {
	if note == "" {
		return line
	}
	var g proto.GoLine
	if json.Unmarshal([]byte(line), &g) != nil {
		return line
	}
	g.Note = note
	b, _ := json.Marshal(g)
	return string(b)
}

// waitCtrlC waits for ctrl-c on the terminal (raw) until ctx ends.
func waitCtrlC(ctx context.Context, fd int) bool {
	for ctx.Err() == nil {
		b, err := readKey(ctx, fd)
		if err != nil {
			return false
		}
		if b == 3 {
			return true
		}
	}
	return false
}
