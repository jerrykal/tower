package towerd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/stream"
	"github.com/jerrykal/tower/internal/transport"
)

// Transport is how the home reaches its hosts: ssh in production, an
// in-process pipe in tests.
type Transport interface {
	// Dial starts the remote command on h and returns its pipes.
	Dial(h config.Host, remote string) (*Pipe, error)
	// Probe opens a fresh session on h's master: whether ssh still
	// answers when the stream does not.
	Probe(ctx context.Context, h config.Host) error
	// Exit makes h's master exit, ending every session on it.
	Exit(ctx context.Context, h config.Host) error
	// AttachArgv is the command line of an interactive session on h.
	AttachArgv(h config.Host, remote string) []string
	// Sweep removes control sockets nobody listens on.
	Sweep()
	// Run runs one command on h with stdin and returns its stdout (an
	// install's checks and upload).
	Run(ctx context.Context, h config.Host, remote string, stdin io.Reader) (string, error)
}

// Pipe is a running remote command.
type Pipe struct {
	R      io.Reader      // its stdout
	W      io.WriteCloser // its stdin
	Stderr io.Reader      // may be nil
	Wait   func() int     // its exit status, once it ended
	Kill   func()
}

type sshTransport struct {
	ssh *transport.SSH
	cm  string
}

func newSSHTransport(cm string) *sshTransport { return &sshTransport{ssh: transport.New(cm), cm: cm} }

func (t *sshTransport) Dial(h config.Host, remote string) (*Pipe, error) {
	cmd := t.ssh.Stream(h, remote)
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errp, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var once sync.Once
	exited := make(chan struct{})
	p := &Pipe{R: out, W: in, Stderr: errp}
	code := -1
	p.Wait = func() int {
		once.Do(func() {
			if err := cmd.Wait(); err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					code = ee.ExitCode()
				}
			} else {
				code = 0
			}
			close(exited)
		})
		<-exited
		return code
	}
	p.Kill = func() {
		cmd.Process.Signal(syscall.SIGTERM)
		go func() {
			select {
			case <-exited:
			case <-time.After(time.Second):
				cmd.Process.Kill()
			}
		}()
	}
	return p, nil
}

func (t *sshTransport) Probe(ctx context.Context, h config.Host) error { return t.ssh.Probe(ctx, h) }
func (t *sshTransport) Exit(ctx context.Context, h config.Host) error  { return t.ssh.Exit(ctx, h) }
func (t *sshTransport) AttachArgv(h config.Host, remote string) []string {
	return t.ssh.Attach(h, remote)
}
func (t *sshTransport) Sweep() { transport.Sweep(t.cm) }
func (t *sshTransport) Run(ctx context.Context, h config.Host, remote string, stdin io.Reader) (string, error) {
	return t.ssh.Run(ctx, h, remote, stdin)
}

// link is the home's connection to one host: ssh, the bridge, the stream,
// liveness and reconnects. Its fields are guarded by Daemon.mu.
type link struct {
	h    *homeRole
	cfg  config.Host
	pace *pacer // view pushes, 150ms in a burst

	status, reason, warn string
	id, mkey, os, tmuxV  string
	version              string
	proto                int
	inst                 string
	nosrv                bool
	sessions             []proto.Session
	clients              []proto.Client // the host's last client list
	stateAt              time.Time
	heard                time.Time // last heard from the host (wall clock, for hosts.json)
	conn                 *stream.Conn
	upAt                 time.Time
	attempts, gen        int
	states               int
	gotState             chan struct{}
	lastView             string
	rx, tx               atomic.Int64

	installed string // "installed <version>" once this home installed its build here
	installs  int    // successful installs since the host last came up; one that did not take fails the host

	givenUp  string        // why the home gave the current stream up
	resetC   chan struct{} // closed when a master reset under way is done
	kickC    chan struct{}
	stopC    chan struct{}
	stopOnce sync.Once
	running  bool
}

func newLink(h *homeRole, cfg config.Host) *link {
	l := &link{h: h, cfg: cfg, status: proto.StatusConnecting, kickC: make(chan struct{}, 1), stopC: make(chan struct{})}
	if !cfg.On() {
		l.status = proto.StatusOff
	}
	l.pace = newPacer(150*time.Millisecond, 2, l.pushView)
	return l
}

// fromCache starts a link from what the home last knew of its host. The
// cached ages are as of Heard; they move on from there. The cache is not
// changed: a link made from it again shifts the same ages again from the
// same moment.
func (l *link) fromCache(c cachedHost) {
	l.id, l.os, l.tmuxV, l.version, l.mkey, l.inst = c.ID, c.OS, c.Tmux, c.Version, c.MKey, c.Inst
	l.stateAt = time.Now()
	l.sessions = sessionsShift(c.Sessions, 0)
	if c.Heard > 0 {
		l.heard = time.UnixMilli(c.Heard)
		l.sessions = sessionsShift(c.Sessions, time.Since(l.heard).Milliseconds())
	}
}

// toCache is what the home keeps of the host: its sessions with ages as
// of when it was last heard (or now, for a host never heard).
func (l *link) toCache() cachedHost {
	at := l.heard
	if at.IsZero() {
		at = time.Now()
	}
	c := cachedHost{ID: l.id, OS: l.os, Tmux: l.tmuxV, Version: l.version, MKey: l.mkey, Inst: l.inst,
		Sessions: sessionsShift(l.sessions, at.Sub(l.stateAt).Milliseconds()), Heard: at.UnixMilli()}
	return c
}

func sessionsShift(ss []proto.Session, ms int64) []proto.Session {
	out := make([]proto.Session, len(ss))
	copy(out, ss)
	for i := range out {
		out[i].Ago += ms
	}
	return out
}

// host is the link's host as the view lists it. Call with mu held.
func (l *link) host(now time.Time) proto.Host {
	hst := proto.Host{
		ID: l.id, Name: l.cfg.Name, Status: l.status, Reason: l.reason, OS: l.os, Tmux: l.tmuxV,
		Version: l.version, MKey: l.mkey, Inst: l.inst, NoServer: l.nosrv, Link: l.gen,
		Sessions: sessionsShift(l.sessions, now.Sub(l.stateAt).Milliseconds()),
	}
	if l.status == proto.StatusDup {
		// Its sessions are listed under the alias that linked.
		hst.Sessions = nil
	}
	if l.status != proto.StatusUp && !l.heard.IsZero() {
		hst.Seen = now.Sub(l.heard).Milliseconds()
	}
	if l.conn != nil {
		hst.RTT = l.conn.SlowRTT().Milliseconds()
	}
	return hst
}

// status_ is the link's line in the full status. Call with mu held.
func (l *link) status_() proto.LinkStatus {
	s := proto.LinkStatus{
		Name: l.cfg.Name, Status: l.status, Reason: l.reason, ID: l.id, MKey: l.mkey, Inst: l.inst,
		Version: l.version, OS: l.os, Proto: l.proto, Attempts: l.attempts, Link: l.gen, States: l.states,
		Rx: l.rx.Load(), Tx: l.tx.Load(), Sessions: len(l.sessions), Warn: l.warn,
	}
	if l.nosrv && l.status == proto.StatusUp {
		s.Status = "nosrv"
	}
	if l.conn != nil {
		s.RTT = l.conn.SlowRTT().Milliseconds()
		s.Offset = l.conn.Offset().Milliseconds()
		s.Stalled = l.conn.Stalled()
	}
	return s
}

// standbyKey says which link a standby belongs to: one made for an
// earlier link, another towerd or another command is never used. Call
// with mu held.
func (l *link) standbyKey() string {
	return fmt.Sprintf("%d|%s|%s|%s|%s", l.gen, l.id, l.version, l.cfg.Tmux, l.h.towerCommand(l.cfg))
}

func (l *link) start() {
	l.h.d.mu.Lock()
	if l.running {
		l.h.d.mu.Unlock()
		return
	}
	l.running = true
	l.h.d.mu.Unlock()
	go l.run()
}

func (l *link) stop() {
	l.stopOnce.Do(func() { close(l.stopC) })
	l.pace.Stop()
	l.h.d.mu.Lock()
	conn := l.conn
	if !l.cfg.On() {
		l.status = proto.StatusOff
	}
	l.h.d.mu.Unlock()
	if conn != nil {
		conn.Close(errors.New("link stopped"))
	}
}

func (l *link) stopped() bool {
	select {
	case <-l.stopC:
		return true
	default:
		return false
	}
}

// kick ends a backoff wait at once.
func (l *link) kick() {
	select {
	case l.kickC <- struct{}{}:
	default:
	}
}

func (l *link) logf(f string, a ...any) {
	l.h.d.logf("link %s: "+f, append([]any{l.cfg.Name}, a...)...)
}

// outcome is how one connection ended.
type outcome struct {
	upFor   time.Duration
	givenUp bool
	failed  bool
	missing bool // the shell found no tower binary (127) on an unpinned host
}

// run connects, and reconnects with backoff from TOWER_BACKOFF_BASE (1s)
// doubling to TOWER_BACKOFF_CAP (2m) with jitter, reset only after
// TOWER_STABLE (30s) up. A link given up as dead retries at once; one
// that was up for the stable period within 200ms.
func (l *link) run() {
	base := config.Duration("TOWER_BACKOFF_BASE", time.Second)
	limit := config.Duration("TOWER_BACKOFF_CAP", 2*time.Minute)
	stable := config.Duration("TOWER_STABLE", 30*time.Second)
	backoff := base
	for !l.stopped() {
		o := l.connect()
		if l.stopped() {
			return
		}
		if o.missing {
			// The build is not there yet: install it and connect again
			// at once, or wait the cap after a failed install.
			o.givenUp = l.install()
			o.failed = !o.givenUp
		}
		var wait time.Duration
		switch {
		case o.upFor >= stable:
			backoff = base
			wait = 100 * time.Millisecond
		case o.givenUp:
			wait = 0
		case o.failed:
			wait = limit
		default:
			wait = backoff/2 + rand.N(backoff/2+1)
			backoff = min(2*backoff, limit)
		}
		l.h.d.mu.Lock()
		resetC := l.resetC
		l.h.d.mu.Unlock()
		if resetC != nil {
			// A new stream would ride the dead master: wait for its reset.
			select {
			case <-resetC:
			case <-l.stopC:
				return
			}
		}
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-l.kickC:
			case <-l.stopC:
				t.Stop()
				return
			}
			t.Stop()
		}
	}
}

// countReader and countWriter count a stream's bytes.
type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type countWriter struct {
	w io.WriteCloser
	n *atomic.Int64
}

func (c countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

func (c countWriter) Close() error { return c.w.Close() }

// connect runs one connection from ssh to its end.
func (l *link) connect() outcome {
	d := l.h.d
	d.mu.Lock()
	l.attempts++
	if l.status != proto.StatusDup && l.status != proto.StatusFailed {
		l.status = proto.StatusConnecting
	}
	l.givenUp = ""
	l.gotState = make(chan struct{})
	gotState := l.gotState
	d.bump()
	d.mu.Unlock()
	l.h.kickViews()

	l.h.tr.Sweep()
	remote := l.h.towerCommand(l.cfg, "towerd", "--stdio", "--tmux", l.cfg.Tmux)
	p, err := l.h.tr.Dial(l.cfg, remote)
	if err != nil {
		l.down(transport.Down, err.Error())
		return outcome{}
	}
	var tail tailBuf
	tsCheck := make(chan struct{})
	stderrDone := make(chan struct{})
	if p.Stderr != nil {
		go func() {
			defer close(stderrDone)
			sc := bufio.NewScanner(p.Stderr)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			closed := false
			for sc.Scan() {
				line := sc.Text()
				tail.add(line)
				if !closed && transport.TailscaleCheck(tail.String()) {
					closed = true
					close(tsCheck)
					p.Kill()
				}
			}
		}()
	} else {
		close(stderrDone)
	}
	helloC := make(chan *proto.Hello, 1)
	var conn *stream.Conn
	conn = stream.New(countReader{p.R, &l.rx}, countWriter{p.W, &l.tx}, stream.Options{
		Silence: homeSilence(),
		Closer:  p.Kill,
		Log:     func(f string, a ...any) { l.logf(f, a...) },
		OnMsg: func(m *proto.Msg) {
			switch m.T {
			case proto.THello:
				if m.Hello != nil {
					select {
					case helloC <- m.Hello:
					default:
					}
				}
			case proto.TState:
				if m.State != nil {
					l.takeState(conn, m.State)
				}
			case proto.TRelay:
				if m.Req != nil {
					go l.h.serveRelay(l, m.Req)
				}
			}
		},
		OnStall: func(stalled bool) { l.onStall(conn, stalled) },
	})
	d.mu.Lock()
	l.conn = conn
	d.mu.Unlock()
	conn.Start()
	lo, hi := protoRange()
	sent := time.Now()
	conn.Send(&proto.Msg{T: proto.THello, Hello: &proto.Hello{Min: lo, Max: hi, ID: d.id, Name: d.name, As: l.cfg.Name, Version: d.version}})

	result := outcome{}
	finish := func(class, reason string) outcome {
		conn.Close(errors.New(reason))
		// ssh's last words (the reason it failed) are read before Wait:
		// Wait closes the stderr pipe, dropping what was not read yet. A
		// process left holding stderr (a master ssh forked) bounds it.
		select {
		case <-stderrDone:
		case <-time.After(2 * time.Second):
		}
		code := p.Wait()
		select {
		case <-tsCheck:
			class, reason = transport.Down, transport.Classify(l.cfg.Name, code, tail.String()).Reason
		default:
			if class == "" {
				f := transport.Classify(l.cfg.Name, code, tail.String())
				class, reason = f.Class, f.Reason
				if code == 0 && strings.TrimSpace(tail.String()) == "" {
					reason = "the stream ended"
				}
			}
		}
		d.mu.Lock()
		if l.conn == conn {
			l.conn = nil
		}
		given := l.givenUp
		d.mu.Unlock()
		if given != "" {
			result.givenUp = true
			class, reason = transport.Down, given
		}
		result.failed = class == transport.Failed
		if code == 127 && !result.givenUp && l.cfg.Tower == "" {
			// An unpinned host lacking this build: run() installs it.
			result.missing, result.failed = true, false
			return result
		}
		if result.givenUp {
			l.setStatus(proto.StatusConnecting, reason)
		} else if class != "dup" {
			l.down(class, reason)
		}
		return result
	}

	hello, ok := l.awaitHello(conn, helloC, tsCheck)
	if !ok {
		return finish("", "")
	}
	if hello.Err != "" {
		l.logf("refused: %s", hello.Err)
		return finish(transport.Failed, hello.Err+": install the same tower on both sides")
	}
	conn.SetHelloRTT(time.Since(sent))
	conn.Live()
	d.mu.Lock()
	dupOf := ""
	for _, name := range l.h.order {
		o := l.h.links[name]
		if o != nil && o != l && o.id == hello.ID && o.conn != nil && (o.status == proto.StatusUp || o.status == proto.StatusStalled || o.status == proto.StatusConnecting) {
			dupOf = o.cfg.Name
		}
	}
	l.id, l.mkey, l.os, l.version, l.proto = hello.ID, hello.MKey, hello.OS, hello.Version, hello.Proto
	if hello.Tmux != "" {
		l.tmuxV = hello.Tmux
	}
	l.warn = l.installed
	if hello.Version != d.version {
		l.warn = fmt.Sprintf("tower %s there, %s here (protocol %d)", hello.Version, d.version, hello.Proto)
	}
	d.mu.Unlock()
	if dupOf != "" {
		d.mu.Lock()
		l.sessions = nil
		d.mu.Unlock()
		l.setStatus(proto.StatusDup, "dup: same towerd as "+dupOf)
		finish("dup", "")
		result.failed = false
		return result
	}
	// Up once the first state is in, so the host never shows up empty.
	select {
	case <-gotState:
	case <-conn.Done():
		return finish("", "")
	case <-l.stopC:
		return finish(transport.Down, "link stopped")
	case <-time.After(15*time.Second - time.Since(sent)):
		l.logf("no state after the hello")
		return finish(transport.Down, "the host sent no state")
	}
	d.mu.Lock()
	l.status, l.reason = proto.StatusUp, ""
	l.upAt = time.Now()
	l.installs = 0
	l.gen++
	l.heard = time.Now()
	// A first state that came in before the hello was taken: its clients
	// apply now that the host's id is known.
	l.h.applyClients(l.id, l.cfg.Name, l.inst, l.sessions, l.clients)
	d.bump()
	d.mu.Unlock()
	l.logf("up: towerd %s, tower %s, protocol %d, link %d", hello.ID, hello.Version, hello.Proto, l.gen)
	l.h.kickViews()
	go l.h.save(true)
	select {
	case <-conn.Done():
	case <-l.stopC:
	}
	d.mu.Lock()
	result.upFor = time.Since(l.upAt)
	l.heard = time.Now()
	d.mu.Unlock()
	reason := ""
	if err := conn.Err(); errors.Is(err, stream.ErrSilent) {
		// Silent for the whole keepalive (a sleep): presumed half-open,
		// so its master goes too.
		l.resetMaster("silent too long")
		reason = "not heard for " + homeSilence().String()
		d.mu.Lock()
		if l.givenUp == "" {
			l.givenUp = reason
		}
		d.mu.Unlock()
	}
	if l.stopped() {
		return finish(transport.Down, "link stopped")
	}
	return finish("", reason)
}

// install puts this build on the host and reports whether the link should
// connect again at once. A build still missing after an install fails the
// host instead of installing for ever.
func (l *link) install() bool {
	v := l.h.inst.Version
	d := l.h.d
	d.mu.Lock()
	// A build installed once and still missing (a full disk, a noexec
	// root) fails the host until it comes up or its entry changes; a
	// failed install is simply tried again at the backoff cap.
	again := l.installs > 0
	d.mu.Unlock()
	if again {
		l.down(transport.Failed, fmt.Sprintf("tower is not installed on %s: installing %s there did not take", l.cfg.Name, v))
		return false
	}
	l.setStatus(proto.StatusInstalling, "installing tower "+v+"…")
	l.logf("installing tower %s", v)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-l.stopC:
			cancel()
		case <-ctx.Done():
		}
	}()
	if err := l.h.inst.Install(ctx, hostRunner{l.h.tr, l.cfg}, l.cfg.Name); err != nil {
		l.down(transport.Failed, err.Error())
		return false
	}
	l.logf("installed tower %s in %v", v, time.Since(start).Round(time.Millisecond))
	d.mu.Lock()
	l.installs++
	l.installed = "installed " + v
	d.mu.Unlock()
	return true
}

// hostRunner runs commands on one host through the home's transport.
type hostRunner struct {
	tr Transport
	h  config.Host
}

func (r hostRunner) Run(ctx context.Context, remote string, stdin io.Reader) (string, error) {
	return r.tr.Run(ctx, r.h, remote, stdin)
}

// awaitHello waits for the remote's hello, at most 15s.
func (l *link) awaitHello(conn *stream.Conn, helloC chan *proto.Hello, ts chan struct{}) (*proto.Hello, bool) {
	t := time.NewTimer(15 * time.Second)
	defer t.Stop()
	select {
	case h := <-helloC:
		return h, true
	case <-conn.Done():
	case <-ts:
	case <-t.C:
		l.logf("no hello within 15s")
	case <-l.stopC:
	}
	return nil, false
}

func (l *link) setStatus(status, reason string) {
	l.h.d.mu.Lock()
	l.status, l.reason = status, reason
	l.h.d.bump()
	l.h.d.mu.Unlock()
	l.h.kickViews()
}

func (l *link) down(class, reason string) {
	status := proto.StatusDown
	if class == transport.Failed {
		status = proto.StatusFailed
	}
	if !l.cfg.On() {
		status = proto.StatusOff
	}
	l.logf("%s: %s", status, reason)
	l.setStatus(status, reason)
}

// takeState stores a host's state and follows its loops' clients.
func (l *link) takeState(conn *stream.Conn, st *proto.State) {
	d := l.h.d
	d.mu.Lock()
	if l.conn != conn {
		d.mu.Unlock()
		return
	}
	l.inst, l.nosrv, l.sessions, l.clients, l.stateAt = st.Inst, st.NoServer, st.Sessions, st.Clients, time.Now()
	l.heard = time.Now()
	l.states++
	if l.id != "" {
		l.h.applyClients(l.id, l.cfg.Name, st.Inst, st.Sessions, st.Clients)
	}
	if l.gotState != nil {
		select {
		case <-l.gotState:
		default:
			close(l.gotState)
		}
	}
	d.bump()
	d.mu.Unlock()
	l.h.kickViews()
	go l.h.save(false)
}

// pushView sends the merged view to the host if it changed, and reports
// whether it sent.
func (l *link) pushView() bool {
	d := l.h.d
	d.mu.Lock()
	conn := l.conn
	var m *proto.Msg
	if conn != nil && l.status == proto.StatusUp || conn != nil && l.status == proto.StatusStalled {
		m = l.viewMsgLocked(false)
	}
	d.mu.Unlock()
	if m != nil {
		conn.Send(m)
	}
	return m != nil
}

// viewMsgLocked builds the view message for this host; nil when it is
// unchanged since the last one sent (unless always). Call with mu held.
func (l *link) viewMsgLocked(always bool) *proto.Msg {
	key, _ := json.Marshal(l.h.buildView(ageRef))
	if !always && string(key) == l.lastView {
		return nil
	}
	l.lastView = string(key)
	v := l.h.buildView(time.Now())
	l.h.viewSeq++
	v.Seq = l.h.viewSeq
	v.Pad = testPad()
	return &proto.Msg{T: proto.TView, View: &v}
}

// onStall follows the stream's stall mark. A stall starts a probe of ssh
// and a give-up timer; a stall after a network change, with the host not
// heard since, gives up at once.
func (l *link) onStall(conn *stream.Conn, stalled bool) {
	d := l.h.d
	d.mu.Lock()
	if l.conn != conn {
		d.mu.Unlock()
		return
	}
	if !stalled {
		if l.status == proto.StatusStalled {
			l.status = proto.StatusUp
			d.bump()
		}
		d.mu.Unlock()
		l.logf("heard again")
		l.h.kickViews()
		return
	}
	if l.status != proto.StatusUp {
		d.mu.Unlock()
		return
	}
	l.status = proto.StatusStalled
	l.h.abortSwitches(l.id, l.cfg.Name+" is not responding")
	heard := conn.Heard()
	netAt := l.h.netAt
	d.bump()
	d.mu.Unlock()
	l.logf("stalled (silent %v)", conn.Silent().Round(time.Millisecond))
	l.h.kickViews()
	if netAt.After(heard) {
		go l.giveUp(conn, true, "silent since the network changed")
		return
	}
	go l.watchStall(conn, heard)
}

// watchStall probes ssh while the host is stalled and gives the link up
// after 3s plus four slow round trips: only the stream if ssh answered
// (the host's towerd is wedged, its attaches are fine), else the master
// too.
func (l *link) watchStall(conn *stream.Conn, heard time.Time) {
	var probeOK atomic.Bool
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second+2*conn.SlowRTT())
		defer cancel()
		if err := l.h.tr.Probe(ctx, l.cfg); err == nil {
			probeOK.Store(true)
			l.logf("stalled, but ssh answers: the stream alone will be given up")
		}
	}()
	giveUp := time.NewTimer(conn.GiveUpAfter())
	defer giveUp.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-conn.Done():
			return
		case <-l.stopC:
			return
		case <-giveUp.C:
			l.giveUp(conn, !probeOK.Load(), fmt.Sprintf("not responding for %v", conn.Silent().Round(100*time.Millisecond)))
			return
		case <-tick.C:
			if !conn.Stalled() {
				return
			}
			l.h.d.mu.Lock()
			netAt := l.h.netAt
			l.h.d.mu.Unlock()
			if netAt.After(heard) {
				l.giveUp(conn, true, "silent since the network changed")
				return
			}
		}
	}
}

// giveUp ends the stream; with master, also makes ssh's master exit,
// since a new stream would ride the dead master and hang.
func (l *link) giveUp(conn *stream.Conn, master bool, why string) {
	d := l.h.d
	d.mu.Lock()
	if l.conn != conn || l.givenUp != "" {
		d.mu.Unlock()
		return
	}
	l.givenUp = why
	d.mu.Unlock()
	l.logf("given up (%s), master too: %v", why, master)
	if master {
		l.resetMaster(why)
	}
	conn.Close(errors.New("given up: " + why))
}

// resetMaster runs ssh -O exit in the background; the next connect waits
// for it.
func (l *link) resetMaster(why string) {
	d := l.h.d
	d.mu.Lock()
	if l.resetC != nil {
		d.mu.Unlock()
		return
	}
	done := make(chan struct{})
	l.resetC = done
	d.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := l.h.tr.Exit(ctx, l.cfg); err != nil {
			l.logf("master exit (%s): %v", why, err)
		}
		d.mu.Lock()
		l.resetC = nil
		d.mu.Unlock()
		close(done)
	}()
}

// netChanged is a network change: a stalled host not heard since is
// given up now; a host that is down tries again now.
func (l *link) netChanged() {
	d := l.h.d
	d.mu.Lock()
	conn, status := l.conn, l.status
	d.mu.Unlock()
	switch {
	case conn != nil && status == proto.StatusStalled:
		l.giveUp(conn, true, "silent since the network changed")
	case conn == nil:
		l.kick()
	}
}

// reset is a wake: the stream closes and the master exits at once.
func (l *link) reset(why string) {
	d := l.h.d
	d.mu.Lock()
	conn := l.conn
	if conn != nil && l.givenUp == "" {
		l.givenUp = why
	}
	d.mu.Unlock()
	l.resetMaster(why)
	if conn != nil {
		conn.Close(errors.New(why))
	}
	l.kick()
}

// tailBuf keeps the last lines of ssh's stderr.
type tailBuf struct {
	mu    sync.Mutex
	lines []string
}

func (t *tailBuf) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, s)
	if len(t.lines) > 20 {
		t.lines = t.lines[len(t.lines)-20:]
	}
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}
