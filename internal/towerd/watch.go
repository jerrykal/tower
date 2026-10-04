package towerd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/tmux"
	"github.com/jerrykal/tower/internal/transport"
)

// towerSession is the hidden session towerd's control client attaches to.
const towerSession = "_tower"

// snapshot is towerd's picture of its own tmux at one re-read.
type snapshot struct {
	At       time.Time
	Inst     string
	NoServer bool
	Sessions []proto.Session // _tower left out
	Clients  []tclient       // control clients left out
}

// tclient is one tmux client as list-clients shows it.
type tclient struct {
	Pid     int
	Created string
	Name    string
	Session string
	Window  string
}

// key is what a re-read is compared on: an unchanged one is not
// published again. Ages go in as of ageRef, so the time between two reads
// is no change.
func (s *snapshot) key() string {
	b, _ := json.Marshal(struct {
		I string
		N bool
		S []proto.Session
		C []tclient
	}{s.Inst, s.NoServer, sessionsAt(s, ageRef), s.Clients})
	return string(b)
}

// session finds a session by id.
func (s *snapshot) session(id string) *proto.Session {
	if s == nil {
		return nil
	}
	for i := range s.Sessions {
		if s.Sessions[i].ID == id {
			return &s.Sessions[i]
		}
	}
	return nil
}

var errNoControl = errors.New("no tmux server")

// watcher owns the control client on towerd's own server: it finds or
// waits for the server, keeps _tower and the client, and re-reads the
// server on every change, paced.
type watcher struct {
	d    *Daemon
	srv  tmux.Server
	pace *pacer
	keys *keys

	readMu sync.Mutex // one re-read at a time, so an older never lands after a newer

	mu      sync.Mutex
	ctl     *tmux.Control
	stopped bool
	stopC   chan struct{}
	doneC   chan struct{}
	pollC   chan struct{}
	lastKey string
}

func newWatcher(d *Daemon) *watcher {
	w := &watcher{d: d, srv: d.tm, stopC: make(chan struct{}), doneC: make(chan struct{}), pollC: make(chan struct{}, 1)}
	w.keys = newKeys(w)
	w.pace = newPacer(50*time.Millisecond, 2, func() bool { w.reread(); return true })
	return w
}

// control is the current control client, or nil.
func (w *watcher) control() *tmux.Control {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ctl
}

// kick asks for a paced re-read.
func (w *watcher) kick() { w.pace.Kick() }

// poke ends a wait for a server early (a server towerd just started).
func (w *watcher) poke() {
	select {
	case w.pollC <- struct{}{}:
	default:
	}
}

func (w *watcher) isStopped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopped
}

func (w *watcher) run() {
	defer close(w.doneC)
	poll := config.Duration("TOWER_NOSRV_POLL", 2*time.Second)
	for !w.isStopped() {
		noServer, users, err := w.probe()
		if err != nil {
			w.d.logf("watch: probe: %v", err)
		}
		if noServer || users == 0 {
			w.publish(&snapshot{At: time.Now(), NoServer: noServer})
			if !w.sleep(poll) {
				return
			}
			continue
		}
		ctl, err := w.attach()
		if err != nil {
			if !w.isStopped() {
				w.d.logf("watch: attach: %v", err)
			}
			if !w.sleep(poll) {
				return
			}
			continue
		}
		w.follow(ctl)
	}
}

// sleep waits d, a poke, or the stop; false on stop.
func (w *watcher) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-w.pollC:
	case <-w.stopC:
		return false
	}
	return true
}

// probe lists the sessions with a command that never starts a server.
func (w *watcher) probe() (noServer bool, users int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := w.srv.Run(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		if tmux.NoServer(err) {
			return true, 0, nil
		}
		return false, 0, err
	}
	for _, n := range strings.Split(strings.TrimSpace(out), "\n") {
		if n != "" && n != towerSession {
			users++
		}
	}
	return false, users, nil
}

// attach makes _tower if it is missing, ends a control client a dead
// towerd left, attaches a new one, and installs keys and hooks.
func (w *watcher) attach() (*tmux.Control, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keep := transport.ShellQuote(w.d.self) + " _keep " + strconv.Itoa(os.Getpid()) + " " + transport.ShellQuote(w.d.env.StateDir)
	// -N: never start a server for it.
	_, err := w.srv.Run(ctx, "-N", "new-session", "-d", "-s", towerSession, "-x", "10", "-y", "3", keep,
		";", "set-option", "-t", "="+towerSession+":", "destroy-unattached", "off",
		";", "set-option", "-t", "="+towerSession+":", "remain-on-exit", "off",
		";", "set-option", "-t", "="+towerSession+":", "status", "off")
	if err != nil && !strings.Contains(err.Error(), "duplicate session") {
		return nil, fmt.Errorf("make %s: %w", towerSession, err)
	}
	w.killLeftover()
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return nil, errors.New("stopping")
	}
	w.mu.Unlock()
	ctl, err := tmux.Attach(w.srv, "="+towerSession)
	if err != nil {
		return nil, err
	}
	config.WriteFile(w.d.env.State("ctl.pid"), []byte(strconv.Itoa(ctl.Pid())+"\n"))
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		ctl.Close()
		return nil, errors.New("stopping")
	}
	w.ctl = ctl
	w.mu.Unlock()
	if r, err := ctl.DoTimeout("display-message -p '#{version}'", 5*time.Second); err == nil && !r.Err {
		w.d.mu.Lock()
		w.d.tmuxVer = r.Text()
		w.d.mu.Unlock()
	}
	w.keys.install(ctl)
	w.d.logf("watch: attached control client %s (pid %d)", ctl.Name(), ctl.Pid())
	w.reread()
	return ctl, nil
}

// killLeftover ends the control client in ctl.pid when it is a tmux
// client this towerd did not start: a towerd killed without closing its
// client leaves it behind, and tmux never finishes a control client whose
// reader is gone, which keeps the server alive.
func (w *watcher) killLeftover() {
	b, err := os.ReadFile(w.d.env.State("ctl.pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 || syscall.Kill(pid, 0) != nil {
		return
	}
	if ppid(pid) == os.Getpid() || !isControlClient(pid) {
		return
	}
	w.d.logf("watch: ending the control client %d a previous towerd left", pid)
	syscall.Kill(pid, syscall.SIGKILL)
}

// isControlClient reports whether pid runs `tmux … -C attach…`.
func isControlClient(pid int) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	s := string(out)
	return strings.Contains(s, "tmux") && strings.Contains(s, " -C ")
}

func ppid(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// follow takes the control client's notifications until it ends or the
// watch stops. Every notification but pane output asks for a re-read.
func (w *watcher) follow(ctl *tmux.Control) {
	notes := ctl.Notes()
	for {
		select {
		case n, ok := <-notes:
			if !ok {
				w.mu.Lock()
				if w.ctl == ctl {
					w.ctl = nil
				}
				w.mu.Unlock()
				w.d.logf("watch: control client ended")
				return
			}
			switch n.Name {
			case "output", "extended-output", "pause", "continue", "begin", "end", "error":
				continue
			case "exit":
				continue // the reader closes Notes next
			}
			w.pace.Kick()
		case <-w.stopC:
			return
		}
	}
}

// Formats of a re-read: a tab between fields, the free-text field last.
const (
	fmtInst     = "#{pid}:#{start_time}"
	fmtSessions = "#{session_id}\t#{session_last_attached}\t#{session_created}\t#{session_attached}\t#{session_group}\t#{session_path}\t#{session_name}"
	fmtWindows  = "#{session_id}\t#{window_id}\t#{window_index}\t#{window_panes}\t#{window_active}\t#{window_bell_flag}\t#{window_activity_flag}\t#{window_silence_flag}\t#{window_name}"
	fmtClients  = "#{client_pid}\t#{client_created}\t#{client_control_mode}\t#{session_id}\t#{window_id}\t#{session_name}\t#{client_name}"
)

// reread reads the server through the control client and publishes the
// result. It also keeps _tower from holding a server with no other
// session, and moves any client that lands on _tower elsewhere.
func (w *watcher) reread() (*snapshot, error) {
	w.readMu.Lock()
	defer w.readMu.Unlock()
	ctl := w.control()
	if ctl == nil {
		return nil, errNoControl
	}
	reps, err := ctl.DoMany([]string{
		"display-message -p " + tmux.Quote(fmtInst),
		"list-sessions -F " + tmux.Quote(fmtSessions),
		"list-windows -a -F " + tmux.Quote(fmtWindows),
		"list-clients -F " + tmux.Quote(fmtClients),
	}, 5*time.Second)
	if err != nil {
		return nil, err
	}
	for _, r := range reps {
		if r.Err {
			return nil, fmt.Errorf("re-read: %s", r.Text())
		}
	}
	snap, towerID, onTower := parseRead(reps, time.Now())
	if len(snap.Sessions) == 0 {
		// The last user session is gone: _tower must not keep the server.
		w.release(ctl)
		snap = &snapshot{At: time.Now()}
		w.publishLocked(snap)
		return snap, nil
	}
	for _, c := range onTower {
		if r, err := ctl.Do("switch-client -c " + tmux.Quote(c) + " -l"); err != nil || r.Err {
			ctl.Do("switch-client -c " + tmux.Quote(c) + " -t " + snap.Sessions[0].ID)
		}
	}
	_ = towerID
	w.publishLocked(snap)
	return snap, nil
}

// release detaches the control client and kills _tower, which ends a
// server with no other session.
func (w *watcher) release(ctl *tmux.Control) {
	w.mu.Lock()
	if w.ctl == ctl {
		w.ctl = nil
	}
	w.mu.Unlock()
	ctl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w.srv.Run(ctx, "-N", "kill-session", "-t", "="+towerSession)
	os.Remove(w.d.env.State("ctl.pid"))
	w.d.logf("watch: no session but %s: released the server", towerSession)
}

func parseRead(reps []tmux.Reply, now time.Time) (snap *snapshot, towerID string, onTower []string) {
	snap = &snapshot{At: now}
	if len(reps[0].Lines) > 0 {
		snap.Inst = reps[0].Lines[0]
	}
	nowMs := now.UnixMilli()
	idx := map[string]int{}
	for _, l := range reps[1].Lines {
		f, err := tmux.Fields(l, 7)
		if err != nil {
			continue
		}
		if f[6] == towerSession {
			towerID = f[0]
			continue
		}
		last, _ := strconv.ParseInt(f[1], 10, 64)
		if last == 0 {
			last, _ = strconv.ParseInt(f[2], 10, 64)
		}
		att, _ := strconv.Atoi(f[3])
		idx[f[0]] = len(snap.Sessions)
		snap.Sessions = append(snap.Sessions, proto.Session{
			ID: f[0], Name: f[6], Path: f[5], Group: f[4], Attached: att,
			Ago: max(0, nowMs-last*1000),
		})
	}
	for _, l := range reps[2].Lines {
		f, err := tmux.Fields(l, 9)
		if err != nil {
			continue
		}
		i, ok := idx[f[0]]
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(f[2])
		p, _ := strconv.Atoi(f[3])
		snap.Sessions[i].Windows = append(snap.Sessions[i].Windows, proto.Window{
			ID: f[1], Index: n, Panes: p, Name: f[8],
			Active: f[4] == "1", Bell: f[5] == "1", Activity: f[6] == "1", Silence: f[7] == "1",
		})
	}
	for _, l := range reps[3].Lines {
		f, err := tmux.Fields(l, 7)
		if err != nil || f[2] == "1" {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		if f[5] == towerSession {
			onTower = append(onTower, f[6])
		}
		snap.Clients = append(snap.Clients, tclient{Pid: pid, Created: f[1], Session: f[3], Window: f[4], Name: f[6]})
	}
	slices.SortFunc(snap.Clients, func(a, b tclient) int { return strings.Compare(a.Name, b.Name) })
	return snap, towerID, onTower
}

// publish hands a snapshot to the daemon, in re-read order.
func (w *watcher) publish(s *snapshot) {
	w.readMu.Lock()
	defer w.readMu.Unlock()
	w.publishLocked(s)
}

func (w *watcher) publishLocked(s *snapshot) {
	k := s.key()
	same := k == w.lastKey
	w.lastKey = k
	w.d.publish(s, !same)
}

// refresh re-reads at once (not paced) and returns the snapshot: what
// an action does before it answers, so the rows read after its answer
// show its result.
func (w *watcher) refresh() (*snapshot, error) { return w.reread() }

// waitAttached waits until the watch has a control client, for a server
// that has just started.
func (w *watcher) waitAttached(ctx context.Context) bool {
	w.poke()
	for {
		if w.control() != nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// stop ends the watch: keys and hooks back, the control client detached
// by name, then _tower killed.
func (w *watcher) stop() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stopped = true
	close(w.stopC)
	w.mu.Unlock()
	w.pace.Stop()
	select {
	case <-w.doneC:
	case <-time.After(3 * time.Second):
	}
	w.readMu.Lock()
	defer w.readMu.Unlock()
	w.mu.Lock()
	ctl := w.ctl
	w.ctl = nil
	w.mu.Unlock()
	if ctl == nil {
		return
	}
	w.keys.restore(ctl)
	ctl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w.srv.Run(ctx, "-N", "kill-session", "-t", "="+towerSession)
	os.Remove(w.d.env.State("ctl.pid"))
}
