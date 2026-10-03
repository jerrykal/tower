// Package towerd is tower's daemon: one per user and tmux server. It
// watches its own tmux through a control client, keeps the registrations
// of the clients tower started there, binds tower's keys, serves local
// calls on a unix socket, plays the remote role for every home that
// connects and the home role once an attach loop starts on its machine.
package towerd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/tmux"
	"golang.org/x/sys/unix"
)

// Options start a towerd.
type Options struct {
	Env     *config.Env
	Version string
	// Bridged: started by a home's bridge, so remote-only until a loop or
	// a reload activates the home role.
	Bridged bool
	// Transport reaches the home's hosts; nil: ssh. Tests join daemons
	// in one process with it.
	Transport Transport
	// Self is the tower binary the daemon runs for helpers (_keep, the
	// attach shim, key bindings); empty: this executable.
	Self string
	// LogTo receives the log; nil: towerd.log in the state directory.
	LogTo io.Writer
	// Name is this host's label; empty: TOWER_TEST_NAME, else the short
	// host name.
	Name string
}

// ErrRunning is returned by Start when another towerd holds the lock.
var ErrRunning = errors.New("towerd already running")

// Daemon is one running towerd.
type Daemon struct {
	o       Options
	env     *config.Env
	id      string
	version string
	name    string // this host's label
	self    string
	tm      tmux.Server
	log     *log.Logger
	logFile *os.File
	started time.Time
	// towerEnv is the process's TOWER_* environment as it started, which
	// key bindings carry (a binding starts from the tmux server's
	// environment, not towerd's).
	towerEnv []string
	osName   string

	lock *os.File
	ln   net.Listener

	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
	stopWhy  string
	done     chan struct{}

	w       *watcher
	answers *answers

	mu        sync.Mutex // guards everything below; never held across I/O
	gen       uint64
	changed   chan struct{}
	snap      *snapshot
	looked    bool
	lookedC   chan struct{}
	regs      *registry
	homes     map[string]*homeRec // remote role, by home id|as
	home      *homeRole           // nil until active
	lastCall  time.Time
	calls     int
	tmuxVer   string
	bindState string
}

// Start takes the lock, listens and starts the watch. The daemon runs
// until Stop, a signal (Run), or its idle exit.
func Start(o Options) (*Daemon, error) {
	if o.Version == "" {
		o.Version = "0.0.0-dev"
	}
	e := o.Env
	d := &Daemon{
		o: o, env: e, version: o.Version, started: time.Now(),
		done: make(chan struct{}), changed: make(chan struct{}), lookedC: make(chan struct{}),
		homes: map[string]*homeRec{}, lastCall: time.Now(),
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	if err := d.takeLock(); err != nil {
		return nil, err
	}
	if err := d.openLog(); err != nil {
		d.lock.Close()
		return nil, err
	}
	id, err := config.TowerdID(e.StateDir)
	if err != nil {
		d.lock.Close()
		return nil, err
	}
	d.id = id
	d.name = o.Name
	if d.name == "" {
		d.name = localName()
	}
	d.self = o.Self
	if d.self == "" {
		d.self, _ = os.Executable()
	}
	d.osName = unameS()
	d.takeEnv()
	d.tm = tmux.Server{Bin: tmux.Bin(), Args: e.Tmux}

	os.Remove(e.Socket()) // only the lock holder gets here: a file left by a dead towerd
	ln, err := net.Listen("unix", e.Socket())
	if err != nil {
		d.lock.Close()
		return nil, fmt.Errorf("listen on %s: %w", e.Socket(), err)
	}
	os.Chmod(e.Socket(), 0o600)
	d.ln = ln
	config.WriteFile(e.State("towerd.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"))
	d.logf("towerd %s started: id %s, pid %d, tmux %v (%s), socket %s, bridged %v", d.version, d.id, os.Getpid(), e.Tmux, d.tm.Bin, e.Socket(), o.Bridged)

	d.regs = loadRegistry(e.State("clients.json"))
	d.answers = newAnswers()
	d.w = newWatcher(d)
	go d.w.run()
	if !o.Bridged {
		d.activateHome("started as a home")
	}
	go d.accept()
	go d.housekeeping()
	return d, nil
}

// Run is Start, then waits for the end, stopping on SIGTERM or SIGINT.
func Run(o Options) error {
	d, err := Start(o)
	if err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	go func() {
		select {
		case s := <-sig:
			d.Stop("signal " + s.String())
		case <-d.done:
		}
	}()
	d.Wait()
	return nil
}

// ID is the towerd id.
func (d *Daemon) ID() string { return d.id }

// Wait blocks until the daemon has stopped.
func (d *Daemon) Wait() { <-d.done }

func (d *Daemon) takeLock() error {
	f, err := os.OpenFile(d.env.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("%w for %s (%s)", ErrRunning, d.env.Tag, d.env.LockPath())
	}
	// The lock lives as long as this open file: the daemon keeps it for
	// its whole life (a collected *os.File would drop the flock).
	d.lock = f
	return nil
}

func (d *Daemon) openLog() error {
	if d.o.LogTo != nil {
		d.log = log.New(d.o.LogTo, "", log.LstdFlags|log.Lmicroseconds)
		return nil
	}
	p := d.env.State("towerd.log")
	if st, err := os.Stat(p); err == nil && st.Size() > 4<<20 {
		os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	d.logFile = f
	d.log = log.New(f, "", log.LstdFlags|log.Lmicroseconds)
	return nil
}

func (d *Daemon) logf(format string, args ...any) { d.log.Printf(format, args...) }

// takeEnv keeps the TOWER_* environment for key bindings, then drops what
// must not reach a tmux server towerd starts, its shells, or ssh.
func (d *Daemon) takeEnv() {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "TOWER_") && k != "TOWER_CLIENT" && k != "TOWER_FZF_BIN" {
			d.towerEnv = append(d.towerEnv, kv)
		}
	}
	for _, k := range []string{"TOWER_MKEY", "TOWER_TMUX_BIN", "TOWER_FZF_BIN", "TOWER_CLIENT", "TMUX", "TMUX_PANE"} {
		os.Unsetenv(k)
	}
}

// localName is this host's label: TOWER_TEST_NAME in the scenario suite,
// else the short host name.
func localName() string {
	if n := os.Getenv("TOWER_TEST_NAME"); n != "" {
		return n
	}
	h, err := os.Hostname()
	if err != nil {
		return "local"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// unameS is `uname -s`: Linux, Darwin.
func unameS() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Sysname[:])
}

// Stop ends the daemon: keys and hooks back, the control client detached
// by name, _tower killed, the socket closed, then links and streams.
func (d *Daemon) Stop(why string) {
	d.stopOnce.Do(func() {
		d.stopWhy = why
		go d.shutdown()
	})
}

func (d *Daemon) shutdown() {
	d.logf("stopping: %s", d.stopWhy)
	d.w.stop()
	d.ln.Close()
	if st, err := os.Lstat(d.env.Socket()); err == nil && st.Mode()&os.ModeSocket != 0 {
		os.Remove(d.env.Socket())
	}
	d.cancel()
	d.mu.Lock()
	h := d.home
	var conns []interface{ Close(error) }
	for _, r := range d.homes {
		if r.conn != nil {
			conns = append(conns, r.conn)
		}
	}
	d.mu.Unlock()
	if h != nil {
		h.stop()
	}
	for _, c := range conns {
		c.Close(errors.New("towerd stopping"))
	}
	d.regs.save()
	if b, err := os.ReadFile(d.env.State("towerd.pid")); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid()) {
		os.Remove(d.env.State("towerd.pid"))
	}
	d.logf("stopped")
	if d.logFile != nil {
		d.logFile.Close()
	}
	d.lock.Close()
	close(d.done)
}

// bump tells watch callers that what a dashboard reads may have changed.
// Call with mu held.
func (d *Daemon) bump() {
	d.gen++
	close(d.changed)
	d.changed = make(chan struct{})
}

func (d *Daemon) bumpNow() {
	d.mu.Lock()
	d.bump()
	d.mu.Unlock()
}

// housekeeping exits when idle or when the socket or state directory
// goes, and expires what has a time to live.
func (d *Daemon) housekeeping() {
	idle := config.Duration("TOWER_IDLE", 10*time.Minute)
	sock, _ := os.Stat(d.env.Socket())
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
		}
		if st, err := os.Stat(d.env.Socket()); err != nil || sock != nil && !os.SameFile(st, sock) {
			d.Stop("socket gone")
			return
		}
		if _, err := os.Stat(d.env.StateDir); err != nil {
			d.Stop("state directory gone")
			return
		}
		d.answers.expire()
		d.expireHomes()
		if h := d.homeRole(); h != nil {
			h.expireLoops()
		}
		if idle > 0 && d.idleFor() >= idle {
			d.Stop(fmt.Sprintf("idle for %v", idle))
			return
		}
	}
}

// idleFor is how long nothing has kept the daemon busy: no loop, no
// connected home, no live registration, no call.
func (d *Daemon) idleFor() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls > 0 || d.regs.count() > 0 {
		return 0
	}
	for _, r := range d.homes {
		if r.conn != nil {
			return 0
		}
	}
	if d.home != nil && len(d.home.loops) > 0 {
		return 0
	}
	return time.Since(d.lastCall)
}

func (d *Daemon) accept() {
	for {
		c, err := d.ln.Accept()
		if err != nil {
			if d.ctx.Err() == nil {
				d.logf("accept: %v", err)
				d.Stop("listener failed")
			}
			return
		}
		go d.serve(c)
	}
}

// serve answers one call on c: a request line, a reply line. A stream
// call turns the connection into a home's stream.
func (d *Daemon) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 64<<10)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	var call proto.Call
	if err := json.Unmarshal(line, &call); err != nil {
		writeReply(c, nil, fmt.Errorf("bad call: %v", err))
		return
	}
	if call.Op == proto.CallStream {
		d.serveStream(c, br)
		return
	}
	d.mu.Lock()
	d.calls++
	if call.Op != proto.CallStatus {
		d.lastCall = time.Now()
	}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.calls--
		if call.Op != proto.CallStatus {
			d.lastCall = time.Now()
		}
		d.mu.Unlock()
	}()
	// The caller closing its end cancels a long call.
	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	go func() {
		var b [1]byte
		br.Read(b[:])
		cancel()
	}()
	res, err := d.dispatch(ctx, &call)
	writeReply(c, res, err)
}

func writeReply(w io.Writer, res any, err error) {
	var r proto.Reply
	if err != nil {
		r.Err = err.Error()
	} else if res != nil {
		b, merr := json.Marshal(res)
		if merr != nil {
			r.Err = merr.Error()
		} else {
			r.Result = b
		}
	}
	b, _ := json.Marshal(r)
	w.Write(append(b, '\n'))
}

func decode(call *proto.Call, v any) error {
	if len(call.Args) == 0 {
		return nil
	}
	if err := json.Unmarshal(call.Args, v); err != nil {
		return fmt.Errorf("%s: bad arguments: %v", call.Op, err)
	}
	return nil
}

func (d *Daemon) dispatch(ctx context.Context, call *proto.Call) (any, error) {
	switch call.Op {
	case proto.CallStatus:
		var a proto.StatusArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		return d.status(a.Full), nil
	case proto.CallStop:
		var a proto.StopArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		if a.IfOlderThan != "" && !proto.Newer(a.IfOlderThan, d.version) {
			return nil, fmt.Errorf("towerd %s is not older than %s", d.version, a.IfOlderThan)
		}
		d.Stop("asked by tower " + call.Version)
		return struct{}{}, nil
	case proto.CallRegister:
		var a proto.RegisterArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		return d.register(a)
	case proto.CallView:
		var a proto.ViewArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		return d.view(a), nil
	case proto.CallWatch:
		var a proto.WatchArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		return d.watchGen(ctx, a.Gen), nil
	case proto.CallAct:
		var r proto.Request
		if err := decode(call, &r); err != nil {
			return nil, err
		}
		return d.act(ctx, &r), nil
	case proto.CallWake, proto.CallNetChange, proto.CallReload:
		h := d.homeRole()
		if call.Op == proto.CallReload {
			h = d.activateHome("reload")
			h.reload()
			return struct{}{}, nil
		}
		if h == nil {
			return struct{}{}, nil
		}
		if call.Op == proto.CallWake {
			h.wake("asked")
		} else {
			h.netChanged("asked")
		}
		return struct{}{}, nil
	case proto.CallPlant:
		if !config.Flag("TOWER_TEST_HOOKS", false) {
			return nil, errors.New("plant is a test hook (TOWER_TEST_HOOKS)")
		}
		var r proto.Request
		if err := decode(call, &r); err != nil {
			return nil, err
		}
		return d.activateHome("plant").plant(&r)
	}
	// The home's own calls: the loop activates the home role.
	if call.Op == proto.CallLoop {
		var a proto.LoopBeat
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		return d.activateHome("an attach loop").beat(a), nil
	}
	h := d.homeRole()
	switch call.Op {
	case proto.CallLoopBye, proto.CallPrepare, proto.CallAfter, proto.CallWaitSwitch, proto.CallHeld, proto.CallStandby:
		if h == nil {
			return nil, errors.New("this towerd is not a home: no attach loop runs here")
		}
	}
	switch call.Op {
	case proto.CallLoopBye:
		var a proto.LoopArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		h.bye(a.ID)
		return struct{}{}, nil
	case proto.CallPrepare:
		var a proto.PrepareArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		return h.prepare(ctx, a)
	case proto.CallAfter:
		var a proto.AfterArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		return h.after(ctx, a), nil
	case proto.CallWaitSwitch:
		var a proto.GenArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		return h.waitSwitch(ctx, a), nil
	case proto.CallHeld:
		var a proto.GenArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		return h.held(a), nil
	case proto.CallStandby:
		var a proto.LoopArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		return h.standby(a.ID), nil
	case proto.CallLast:
		var a proto.LastArgs
		if err := decode(call, &a); err != nil {
			return nil, err
		}
		return d.last(ctx, a.Client), nil
	}
	return nil, fmt.Errorf("unknown call %q (towerd %s)", call.Op, d.version)
}

// watchGen answers at once when gen is stale, else when it moves or after
// 20s.
func (d *Daemon) watchGen(ctx context.Context, gen uint64) proto.WatchResult {
	d.mu.Lock()
	cur, ch := d.gen, d.changed
	d.mu.Unlock()
	if gen != cur {
		return proto.WatchResult{Gen: cur}
	}
	t := time.NewTimer(20 * time.Second)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return proto.WatchResult{Gen: d.gen}
}

func (d *Daemon) homeRole() *homeRole {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.home
}

// activateHome starts the home role if it is not running.
func (d *Daemon) activateHome(why string) *homeRole {
	d.mu.Lock()
	if d.home != nil {
		h := d.home
		d.mu.Unlock()
		return h
	}
	h := newHome(d)
	d.home = h
	d.bump()
	d.mu.Unlock()
	d.logf("home role: active (%s)", why)
	h.start()
	return h
}

// status is the answer to status; full adds what towerd knows.
func (d *Daemon) status(full bool) *proto.Status {
	st := &proto.Status{ID: d.id, Version: d.version, Pid: os.Getpid(), MKey: d.env.MKey, Tag: d.env.Tag, TmuxBin: d.tm.Bin}
	d.mu.Lock()
	st.Home = d.home != nil
	d.mu.Unlock()
	if !full {
		return st
	}
	det := &proto.Detail{}
	d.mu.Lock()
	if s := d.snap; s != nil {
		det.Watch = proto.WatchStatus{Inst: s.Inst, NoServer: s.NoServer, Sessions: len(s.Sessions)}
	}
	det.Watch.Keys = d.bindState
	det.Clients = d.regs.clients()
	for _, r := range d.homes {
		hs := proto.HomeStatus{ID: r.id, As: r.as, Name: r.name, Live: r.conn != nil}
		if r.conn == nil {
			hs.Age = time.Since(r.gone).Milliseconds()
		}
		det.Homes = append(det.Homes, hs)
	}
	h := d.home
	d.mu.Unlock()
	if ctl := d.w.control(); ctl != nil {
		det.Watch.CtlPid, det.Watch.CtlName, det.Watch.CtlBytes = ctl.Pid(), ctl.Name(), ctl.Bytes()
	}
	if h != nil {
		h.detail(det)
	}
	st.Detail = det
	return st
}
