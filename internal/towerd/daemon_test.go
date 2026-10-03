package towerd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/tmux"
)

// world is a few towerds in this process, each on a tmux server of its
// own, joined over their real sockets the way the bridge joins them.
type world struct {
	t     *testing.T
	root  string
	keep  string // the stand-in for tower _keep
	mu    sync.Mutex
	nodes map[string]*node
}

type node struct {
	w    *world
	name string
	env  *config.Env
	srv  tmux.Server
	d    *Daemon
	log  *lockedBuf
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ttw")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", root)
	t.Setenv("TMUX", "")
	t.Setenv("TOWER_TEST_HOOKS", "1")
	t.Setenv("TOWER_NOSRV_POLL", "200")
	t.Setenv("TOWER_BACKOFF_BASE", "100")
	t.Setenv("TOWER_BACKOFF_CAP", "1000")
	w := &world{t: t, root: root, nodes: map[string]*node{}}
	w.keep = filepath.Join(root, "keep")
	os.WriteFile(w.keep, []byte("#!/bin/sh\nexec sleep 600\n"), 0o755)
	t.Cleanup(func() {
		for _, n := range w.nodes {
			if n.d != nil {
				n.d.Stop("test over")
				n.d.Wait()
			}
			out, _ := n.srv.Run(context.Background(), "list-sessions", "-F", "#{session_id}")
			for _, id := range strings.Fields(out) {
				n.srv.Run(context.Background(), "kill-session", "-t", id)
			}
		}
		os.RemoveAll(root)
	})
	return w
}

// node makes a host with a tmux server holding sessions (none: no
// server), with conf as its tmux config.
func (w *world) node(name, conf string, sessions ...string) *node {
	w.t.Helper()
	dir := filepath.Join(w.root, name)
	sum := sha256.Sum256([]byte(dir))
	e := &config.Env{
		ConfigDir: filepath.Join(dir, "config"), StateRoot: filepath.Join(dir, "state"),
		Tmux: []string{"-L", "tw-" + name}, Tag: "L-tw-" + name, MKey: hex.EncodeToString(sum[:6]),
		RunDir: filepath.Join(dir, "run"),
	}
	e.StateDir = filepath.Join(e.StateRoot, "towerd", e.MKey+"-"+e.Tag)
	for _, d := range []string{e.ConfigDir, e.StateDir, e.CMDir()} {
		os.MkdirAll(d, 0o700)
	}
	n := &node{w: w, name: name, env: e, srv: tmux.Server{Bin: tmux.Resolve("tmux"), Args: e.Tmux}, log: &lockedBuf{}}
	cf := filepath.Join(dir, "tmux.conf")
	os.WriteFile(cf, []byte("set -g exit-empty on\nset -g default-shell /bin/sh\n"+conf), 0o600)
	for i, s := range sessions {
		args := []string{"new-session", "-d", "-s", s}
		if i == 0 {
			args = append([]string{"-f", cf}, args...)
		}
		if _, err := n.srv.Run(context.Background(), args...); err != nil {
			w.t.Fatal(err)
		}
	}
	w.mu.Lock()
	w.nodes[name] = n
	w.mu.Unlock()
	return n
}

func (n *node) start(bridged bool) *Daemon {
	n.w.t.Helper()
	d, err := Start(Options{Env: n.env, Version: "0.0.1-unit", Bridged: bridged, Transport: &inproc{w: n.w}, Self: n.w.keep, LogTo: n.log, Name: n.name})
	if err != nil {
		n.w.t.Fatal(err)
	}
	n.w.mu.Lock()
	n.d = d
	n.w.mu.Unlock()
	return d
}

func (n *node) hosts(hs ...config.Host) {
	if err := config.SaveHosts(n.env.HostsFile(), hs); err != nil {
		n.w.t.Fatal(err)
	}
}

func (n *node) remote() config.Host {
	return config.Host{Name: n.name, SSH: n.name, Tmux: strings.Join(n.env.Tmux, " ")}
}

func (n *node) tmux(args ...string) string {
	n.w.t.Helper()
	out, err := n.srv.Run(context.Background(), args...)
	if err != nil {
		n.w.t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func (w *world) eventually(d time.Duration, what string, cond func() bool) {
	w.t.Helper()
	start := time.Now()
	for time.Since(start) < d {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.t.Fatalf("%s: not within %v", what, d)
}

func (n *node) link(name string) proto.LinkStatus {
	for _, l := range n.d.status(true).Detail.Links {
		if l.Name == name {
			return l
		}
	}
	return proto.LinkStatus{}
}

func (n *node) ref(on *node, session string) proto.Ref {
	n.w.t.Helper()
	v := n.d.view(proto.ViewArgs{}).View
	for _, h := range v.Hosts {
		if h.ID != on.d.id {
			continue
		}
		for _, s := range h.Sessions {
			if s.Name == session {
				return proto.Ref{Host: h.ID, Name: h.Name, Inst: h.Inst, Session: s.ID, Label: s.Name}
			}
		}
	}
	n.w.t.Fatalf("%s's view has no %s:%s", n.name, on.name, session)
	return proto.Ref{}
}

func viewHas(v proto.View, host, session string) bool {
	for _, h := range v.Hosts {
		if h.Name != host {
			continue
		}
		for _, s := range h.Sessions {
			if s.Name == session {
				return true
			}
		}
	}
	return false
}

// inproc is the Transport of a world: a host's "ssh" connects to its
// towerd's socket and sends the stream call, as the bridge does.
type inproc struct {
	w *world
}

func (p *inproc) Dial(h config.Host, remote string) (*Pipe, error) {
	p.w.mu.Lock()
	n := p.w.nodes[h.Target()]
	p.w.mu.Unlock()
	if n == nil {
		return nil, fmt.Errorf("ssh: Could not resolve hostname %s", h.Target())
	}
	if n.d == nil {
		n.start(true)
	}
	c, err := net.Dial("unix", n.env.Socket())
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(proto.Call{Op: proto.CallStream, Version: "0.0.1-unit"})
	c.Write(append(b, '\n'))
	var once sync.Once
	done := make(chan struct{})
	end := func() { once.Do(func() { c.Close(); close(done) }) }
	return &Pipe{R: c, W: c, Wait: func() int { end(); <-done; return 255 }, Kill: end}, nil
}

func (p *inproc) Probe(ctx context.Context, h config.Host) error { return nil }
func (p *inproc) Exit(ctx context.Context, h config.Host) error  { return nil }
func (p *inproc) AttachArgv(h config.Host, remote string) []string {
	return []string{"ssh", "-t", h.Target(), "--", remote}
}
func (p *inproc) Sweep() {}

var _ io.Writer = (*lockedBuf)(nil)

func TestHomeAndRemote(t *testing.T) {
	w := newWorld(t)
	a := w.node("A", "", "alpha", "apple")
	b := w.node("B", "", "bravo")
	a.hosts(b.remote())
	a.start(false)
	w.eventually(5*time.Second, "B up", func() bool { return a.link("B").Status == proto.StatusUp })
	if !viewHas(a.d.view(proto.ViewArgs{}).View, "B", "bravo") {
		t.Fatal("the home's view lacks B:bravo")
	}
	if b.d.status(false).Home {
		t.Fatal("a towerd started by a bridge plays home")
	}
	w.eventually(2*time.Second, "B holds the home's view", func() bool {
		return viewHas(b.d.view(proto.ViewArgs{}).View, "A", "alpha")
	})

	// A change on B reaches the home, which reaches B's held view.
	start := time.Now()
	b.tmux("new-session", "-d", "-s", "made")
	w.eventually(2*time.Second, "B:made in the home's view", func() bool { return viewHas(a.d.view(proto.ViewArgs{}).View, "B", "made") })
	t.Logf("a new session on B in the home's view after %v", time.Since(start))

	// Read your writes: the rows read right after the answer show it.
	ack := a.d.act(context.Background(), &proto.Request{Op: proto.OpNew, Target: proto.Ref{Host: b.d.id}, Name: "fresh"})
	if !ack.OK || ack.Ref == nil || ack.Ref.Label != "fresh" {
		t.Fatalf("new on B: %+v", ack)
	}
	if !viewHas(a.d.view(proto.ViewArgs{}).View, "B", "fresh") {
		t.Fatal("the home answered before its view had B:fresh")
	}

	// A dashboard on B acts on A through the home; B's rows show the
	// result as soon as the answer is in.
	b.d.register(proto.RegisterArgs{Pid: 999_999, Loop: "L1", Gen: 1, Home: a.d.id})
	cl := "999999:1:/dev/fake"
	ack = b.d.act(context.Background(), &proto.Request{Op: proto.OpRename, Client: cl, Target: a.ref(a, "apple"), Name: "apricot"})
	if !ack.OK {
		t.Fatalf("rename on A from B: %+v", ack)
	}
	if !viewHas(b.d.view(proto.ViewArgs{Client: cl}).View, "A", "apricot") {
		t.Fatal("B's view after the answer lacks A:apricot")
	}
	// A repeat of a request id runs once.
	req := &proto.Request{ID: "same-id", Op: proto.OpNew, Target: proto.Ref{Host: b.d.id}, Name: "once"}
	a1 := a.d.act(context.Background(), req)
	req2 := *req
	a2 := a.d.act(context.Background(), &req2)
	if !a1.OK || !a2.OK || a1.Ref.Session != a2.Ref.Session {
		t.Fatalf("a repeat: %+v %+v", a1, a2)
	}
	if n := strings.Count(b.tmux("list-sessions", "-F", "#{session_name}"), "once"); n != 1 {
		t.Fatalf("%d sessions named once", n)
	}

	// The home stops: B keeps its view, marked, and refuses relays fast.
	a.d.Stop("test")
	a.d.Wait()
	w.eventually(2*time.Second, "B sees the home go", func() bool {
		return strings.Contains(b.d.view(proto.ViewArgs{Client: cl}).Note, "not connected")
	})
	if v := b.d.view(proto.ViewArgs{Client: cl}); !viewHas(v.View, "A", "alpha") {
		t.Fatal("B dropped the home's last view")
	}
	start = time.Now()
	ack = b.d.act(context.Background(), &proto.Request{Op: proto.OpKill, Client: cl, Target: proto.Ref{Host: "elsewhere", Session: "$0"}})
	if ack.OK || !strings.Contains(ack.Err, "not connected") || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("a request with the home gone: %+v after %v", ack, time.Since(start))
	}
}

func TestLoopDecisions(t *testing.T) {
	w := newWorld(t)
	a := w.node("A", "", "alpha", "apple", "avocado")
	a.start(false)
	h := a.d.homeRole()
	ctx := context.Background()
	w.eventually(3*time.Second, "A's first look", func() bool { return len(a.d.snapshotNow().Sessions) == 3 })
	h.beat(proto.LoopBeat{ID: "L"})
	alpha, apple := a.ref(a, "alpha"), a.ref(a, "apple")

	p, err := h.prepare(ctx, proto.PrepareArgs{Loop: "L", Target: alpha})
	if err != nil || !p.Local || p.Gen != 1 || !strings.Contains(strings.Join(p.Argv, " "), "attach --loop L --gen 1 --home "+a.d.id) {
		t.Fatalf("prepare: %+v %v", p, err)
	}
	if p.Argv[len(p.Argv)-1] != alpha.Session {
		t.Fatalf("the shim attaches by id: %v", p.Argv)
	}
	stale := apple
	stale.Inst = "1:1"
	if _, err := h.prepare(ctx, proto.PrepareArgs{Loop: "L", Target: stale}); err == nil || !strings.Contains(err.Error(), "restarted since it was listed") {
		t.Fatalf("a stale instance: %v", err)
	}
	gone := apple
	gone.Session = "$99"
	if _, err := h.prepare(ctx, proto.PrepareArgs{Loop: "L", Target: gone}); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("a gone session: %v", err)
	}

	// Exit 42 alone is never trusted.
	if n := h.after(ctx, proto.AfterArgs{Loop: "L", Gen: 1, Code: 42}); n.Do != proto.NextPicker || !strings.Contains(n.Note, "(no request)") {
		t.Fatalf("a bare 42: %+v", n)
	}
	if n := h.after(ctx, proto.AfterArgs{Loop: "L", Gen: 1, Code: 43}); n.Do != proto.NextPicker || !strings.Contains(n.Note, "restarted") {
		t.Fatalf("43: %+v", n)
	}
	// The session still exists: exit, as tmux does.
	if n := h.after(ctx, proto.AfterArgs{Loop: "L", Gen: 1, Code: 0}); n.Do != proto.NextExit || n.Note != "" {
		t.Fatalf("a detach: %+v", n)
	}

	// A dashboard's switch with no loop waiting: stored, the dashboard
	// ends the client, and the 42 that follows hands off.
	a.d.register(proto.RegisterArgs{Pid: 4242, Loop: "L", Gen: 1, Home: a.d.id})
	cl := "4242:1:/dev/fake"
	ack := a.d.act(ctx, &proto.Request{Op: proto.OpSwitch, Client: cl, Target: apple, Nonce: "n"})
	if !ack.OK || ack.Ended {
		t.Fatalf("switch: %+v", ack)
	}
	if n := h.after(ctx, proto.AfterArgs{Loop: "L", Gen: 1, Code: 42}); n.Do != proto.NextHandoff || n.Target.Session != apple.Session {
		t.Fatalf("42 after a stored switch: %+v", n)
	}
	// A switch stored then a plain detach: discarded, and said so.
	a.d.act(ctx, &proto.Request{Op: proto.OpSwitch, Client: cl, Target: apple})
	if n := h.after(ctx, proto.AfterArgs{Loop: "L", Gen: 1, Code: 0}); n.Do != proto.NextExit || !strings.Contains(n.Note, "discarded") {
		t.Fatalf("a detach with a switch pending: %+v", n)
	}
	// Too old.
	t.Setenv("TOWER_HANDOFF_TTL", "30")
	a.d.act(ctx, &proto.Request{Op: proto.OpSwitch, Client: cl, Target: apple})
	time.Sleep(60 * time.Millisecond)
	if n := h.after(ctx, proto.AfterArgs{Loop: "L", Gen: 1, Code: 42}); n.Do != proto.NextPicker || !strings.Contains(n.Note, "s old") {
		t.Fatalf("a late 42: %+v", n)
	}
	os.Unsetenv("TOWER_HANDOFF_TTL")

	// A new attach: the old attach's dashboard is refused before anything
	// detaches, and a switch planted for it is ignored.
	p, _ = h.prepare(ctx, proto.PrepareArgs{Loop: "L", Target: apple})
	ack = a.d.act(ctx, &proto.Request{Op: proto.OpSwitch, Client: cl, Target: alpha})
	if ack.OK || !strings.Contains(ack.Err, "earlier attach (gen 1, now 2)") {
		t.Fatalf("a stale dashboard: %+v", ack)
	}
	h.plant(&proto.Request{Loop: "L", Gen: 1, Target: alpha, Nonce: "planted"})
	if n := h.after(ctx, proto.AfterArgs{Loop: "L", Gen: 2, Code: 42}); n.Do != proto.NextPicker || !strings.Contains(n.Note, "earlier attach") {
		t.Fatalf("a planted switch of another attach: %+v", n)
	}

	// The eager hand-off: the loop waits, is woken, holds, and is told to
	// end the client; the dashboard hears it ended.
	a.d.register(proto.RegisterArgs{Pid: 4343, Loop: "L", Gen: p.Gen, Home: a.d.id})
	woke := make(chan bool, 1)
	go func() { woke <- h.waitSwitch(ctx, proto.GenArgs{Loop: "L", Gen: p.Gen}).Switch }()
	time.Sleep(20 * time.Millisecond)
	ackC := make(chan *proto.Ack, 1)
	go func() {
		ackC <- a.d.act(ctx, &proto.Request{Op: proto.OpSwitch, Client: "4343:1:/dev/fake2", Target: alpha})
	}()
	if !<-woke {
		t.Fatal("wait-switch did not wake")
	}
	if !h.held(proto.GenArgs{Loop: "L", Gen: p.Gen}).End {
		t.Fatal("held while the switch waits: end it")
	}
	if h.held(proto.GenArgs{Loop: "L", Gen: p.Gen}).End {
		t.Fatal("a second held is not told to end the client again")
	}
	if ack := <-ackC; !ack.OK || !ack.Ended {
		t.Fatalf("eager switch: %+v", ack)
	}
	if n := h.after(ctx, proto.AfterArgs{Loop: "L", Gen: p.Gen, Code: 0, Ended: true}); n.Do != proto.NextHandoff || n.Target.Session != alpha.Session {
		t.Fatalf("after an eager hand-off: %+v", n)
	}

	// The session ends: the previous session nobody is on.
	h.prepare(ctx, proto.PrepareArgs{Loop: "L", Target: alpha})
	p, _ = h.prepare(ctx, proto.PrepareArgs{Loop: "L", Target: a.ref(a, "avocado")})
	a.tmux("kill-session", "-t", "avocado")
	w.eventually(2*time.Second, "avocado gone", func() bool { return a.d.snapshotNow().session(p.Target.Session) == nil })
	n := h.after(ctx, proto.AfterArgs{Loop: "L", Gen: p.Gen, Code: 0})
	if n.Do != proto.NextHandoff || n.Target.Label != "alpha" || !strings.Contains(n.Note, "ended; now on A:alpha") {
		t.Fatalf("after the session ended: %+v", n)
	}
}

func TestKeysTakenAndPutBack(t *testing.T) {
	w := newWorld(t)
	free := w.node("K", "", "k1")
	user := w.node("U", "bind L display-message 'the users L'\nset-hook -g alert-silence[7193] 'display-message mine'\n", "u1")
	free.start(false)
	user.start(false)
	w.eventually(3*time.Second, "keys bound", func() bool {
		return strings.Contains(free.tmux("list-keys", "-T", "root"), "TOWER_CLIENT") &&
			strings.Contains(user.tmux("list-keys", "-T", "root"), "TOWER_CLIENT")
	})
	if _, cmd := binding(strings.Split(free.tmux("list-keys", "-T", "prefix"), "\n"), "prefix", "L"); !strings.Contains(cmd, " last") {
		t.Fatalf("prefix L on a free server: %q", cmd)
	}
	if _, cmd := binding(strings.Split(user.tmux("list-keys", "-T", "prefix"), "\n"), "prefix", "L"); !strings.Contains(cmd, "the users L") {
		t.Fatalf("the user's L was taken: %q", cmd)
	}
	if !strings.Contains(free.tmux("show-hooks", "-g", "alert-bell"), "tower-alert") {
		t.Fatal("no alert hook")
	}
	if !strings.Contains(user.tmux("show-hooks", "-g", "alert-silence"), "mine") {
		t.Fatal("the user's hook at tower's index was replaced")
	}
	free.d.Stop("test")
	free.d.Wait()
	if strings.Contains(free.tmux("list-keys", "-T", "root"), "TOWER_CLIENT") {
		t.Fatal("M-o still bound after stop")
	}
	if _, cmd := binding(strings.Split(free.tmux("list-keys", "-T", "prefix"), "\n"), "prefix", "L"); cmd != "switch-client -l" {
		t.Fatalf("prefix L after stop: %q", cmd)
	}
	if strings.Contains(free.tmux("show-hooks", "-g", "alert-bell"), "tower-alert") {
		t.Fatal("alert hook left after stop")
	}
	if strings.Contains(free.tmux("list-sessions", "-F", "#{session_name}"), "_tower") {
		t.Fatal("_tower left after stop")
	}
}

func TestNoServerNeverStarted(t *testing.T) {
	w := newWorld(t)
	n := w.node("N", "")
	n.start(true)
	w.eventually(2*time.Second, "no server seen", func() bool { return n.d.snapshotNow().NoServer })
	time.Sleep(500 * time.Millisecond)
	if _, err := n.srv.Run(context.Background(), "list-sessions"); err == nil {
		t.Fatal("towerd started a server")
	}
	ack := n.d.act(context.Background(), &proto.Request{Op: proto.OpNew, Name: "first"})
	if !ack.OK || ack.Note != "started tmux on N" {
		t.Fatalf("new with no server: %+v", ack)
	}
	if s := n.d.snapshotNow(); s.NoServer || len(s.Sessions) != 1 || s.Sessions[0].Name != "first" {
		t.Fatalf("the answer came before the watch took the server: %+v", s)
	}
	// The last session goes: _tower must not keep the server.
	n.tmux("kill-session", "-t", "first")
	w.eventually(3*time.Second, "the server exits", func() bool {
		_, err := n.srv.Run(context.Background(), "list-sessions")
		return err != nil
	})
	w.eventually(3*time.Second, "polling again", func() bool { return n.d.snapshotNow().NoServer })
}
