package towerd

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/dirs"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/stream"
	"github.com/jerrykal/tower/internal/tmux"
)

func TestPacerBurstThenPaced(t *testing.T) {
	var mu sync.Mutex
	var runs []time.Time
	p := newPacer(100*time.Millisecond, 2, func() bool {
		mu.Lock()
		runs = append(runs, time.Now())
		mu.Unlock()
		return true
	})
	defer p.Stop()
	start := time.Now()
	// A kick after a quiet spell runs at once.
	p.Kick()
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	if len(runs) != 1 || runs[0].Sub(start) > 20*time.Millisecond {
		t.Fatalf("first kick: %v", runs)
	}
	mu.Unlock()
	// A burst: kicks fold, and runs come no faster than one per 100ms
	// once the bucket is empty.
	for range 50 {
		p.Kick()
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n := len(runs); n < 3 || n > 6 {
		t.Fatalf("%d runs for a 250ms burst", n)
	}
	for i := 3; i < len(runs); i++ {
		if gap := runs[i].Sub(runs[i-1]); gap < 80*time.Millisecond {
			t.Fatalf("runs %d and %d %v apart", i-1, i, gap)
		}
	}
}

func TestPacerFoldsKicksDuringARun(t *testing.T) {
	var n atomic.Int32
	release := make(chan struct{})
	p := newPacer(time.Millisecond, 2, func() bool {
		if n.Add(1) == 1 {
			<-release
		}
		return true
	})
	defer p.Stop()
	p.Kick()
	time.Sleep(10 * time.Millisecond)
	for range 10 {
		p.Kick()
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	if got := n.Load(); got != 2 {
		t.Fatalf("%d runs: kicks during a run fold into one more", got)
	}
}

func TestPacerRunsThatSentNothingAreFree(t *testing.T) {
	var mu sync.Mutex
	var sent []time.Time
	send := false
	p := newPacer(time.Second, 2, func() bool {
		mu.Lock()
		defer mu.Unlock()
		if send {
			sent = append(sent, time.Now())
		}
		return send
	})
	defer p.Stop()
	for range 5 {
		p.Kick()
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	send = true
	mu.Unlock()
	start := time.Now()
	p.Kick()
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || sent[0].Sub(start) > 20*time.Millisecond {
		t.Fatalf("the first real send waited behind runs that sent nothing: %v", sent)
	}
}

func TestParseRead(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	reps := []tmux.Reply{
		{Lines: []string{"123:999"}},
		{Lines: []string{
			"$0\t999990\t999000\t1\t\t/home/u\twork",
			"$1\t0\t999995\t0\tg\t/tmp\tname\twith tab",
			"$2\t999999\t999000\t1\t\t/\t_tower",
		}},
		{Lines: []string{
			"$0\t@0\t1\t2\t1\t0\t1\t0\tvim",
			"$1\t@1\t0\t1\t1\t1\t0\t0\tsh",
			"$2\t@2\t0\t1\t1\t0\t0\t0\tkeep",
		}},
		{Lines: []string{
			"500\t999990\t0\t$0\t@0\twork\t/dev/ttys001",
			"501\t999991\t1\t$2\t@2\t_tower\tclient-501",
			"502\t999992\t0\t$2\t@2\t_tower\t/dev/ttys002",
		}},
	}
	s, towerID, onTower := parseRead(reps, now)
	if s.Inst != "123:999" || towerID != "$2" {
		t.Fatalf("inst %q, _tower %q", s.Inst, towerID)
	}
	if len(s.Sessions) != 2 {
		t.Fatalf("sessions: %+v", s.Sessions)
	}
	w := s.Sessions[0]
	if w.Name != "work" || w.Ago != 10_000 || w.Attached != 1 || len(w.Windows) != 1 || !w.Windows[0].Activity || w.Windows[0].Panes != 2 {
		t.Fatalf("work: %+v", w)
	}
	n := s.Sessions[1]
	if n.Name != "name\twith tab" || n.Ago != 5_000 || n.Group != "g" || !n.Windows[0].Bell {
		t.Fatalf("a session never attached ages from its creation, and a name keeps its tab: %+v", n)
	}
	if len(s.Clients) != 2 || s.Clients[0].Pid != 500 || s.Clients[1].Name != "/dev/ttys002" {
		t.Fatalf("clients (control clients left out): %+v", s.Clients)
	}
	if len(onTower) != 1 || onTower[0] != "/dev/ttys002" {
		t.Fatalf("clients on _tower: %v", onTower)
	}
}

func TestRegistryBind(t *testing.T) {
	r := &registry{path: t.TempDir() + "/clients.json"}
	now := time.Now().UnixMilli()
	r.add(&reg{Pid: 10, Loop: "L", Gen: 1, Home: "H", Inst: "1:1", At: now})
	r.add(&reg{Pid: 11, Loop: "L", Gen: 2, Home: "H", Inst: "1:1", At: now - 60_000})
	s := &snapshot{Inst: "1:1", Clients: []tclient{{Pid: 10, Created: "77", Name: "/dev/tty1", Session: "$1", Window: "@1"}}}
	if !r.bind(s) {
		t.Fatal("binding is a change")
	}
	if r.count() != 1 {
		t.Fatalf("a registration that never found its client is dropped: %d left", r.count())
	}
	if g := r.byClient("10:77:/dev/tty1"); g == nil || g.Session != "$1" {
		t.Fatalf("by client: %+v", g)
	}
	if r.byClient("10:78:/dev/tty1") != nil {
		t.Fatal("a reused pid with another client_created matched")
	}
	// The client moves; then it goes.
	s.Clients[0].Session = "$2"
	r.bind(s)
	if hc := r.forHome("H"); len(hc) != 1 || hc[0].Session != "$2" || hc[0].Loop != "L" {
		t.Fatalf("for home: %+v", hc)
	}
	if len(r.forHome("other")) != 0 {
		t.Fatal("another home's clients")
	}
	s.Clients = nil
	r.bind(s)
	if r.count() != 0 {
		t.Fatal("a registration whose client went is dropped")
	}
	// A restarted server drops them all.
	r.add(&reg{Pid: 12, Inst: "1:1", At: now})
	r.bind(&snapshot{Inst: "2:2"})
	if r.count() != 0 {
		t.Fatal("a registration of an earlier server instance is dropped")
	}
	r.add(&reg{Pid: 13, Inst: "1:1", At: now})
	r.save(r.copyLocked())
	r2 := loadRegistry(r.path)
	if r2.count() != 1 || r2.list[0].Pid != 13 {
		t.Fatalf("restored: %+v", r2.list)
	}
}

// Saves run without the daemon's lock while bind edits the list in place:
// they must write the copy taken under the lock, never torn or nil
// entries, and an older copy never overwrites a newer one.
func TestRegistrySaveRacesBind(t *testing.T) {
	var mu sync.Mutex
	r := &registry{path: filepath.Join(t.TempDir(), "clients.json")}
	now := time.Now().UnixMilli()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the watch: binds and drops under the lock
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			mu.Lock()
			r.add(&reg{Pid: 100 + i%50, Inst: "1:1", At: now})
			s := &snapshot{Inst: "1:1"}
			for p := 100; p < 150; p += 2 {
				s.Clients = append(s.Clients, tclient{Name: "/dev/t" + strconv.Itoa(p), Pid: p, Session: "$1"})
			}
			r.bind(s)
			mu.Unlock()
		}
	}()
	for range 300 { // registrations saving as they come
		mu.Lock()
		c := r.copyLocked()
		mu.Unlock()
		r.save(c)
	}
	close(stop)
	wg.Wait()
	var raw []json.RawMessage
	b, _ := os.ReadFile(r.path)
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, m := range raw {
		if string(m) == "null" {
			t.Fatalf("a null entry was saved: %s", b)
		}
	}
	older := r.copyLocked()
	newer := r.copyLocked()
	r.list = nil
	r.save(r.copyLocked())
	r.save(newer)
	r.save(older)
	if got := loadRegistry(r.path); len(got.list) != 0 {
		t.Fatalf("an older copy overwrote a newer one: %+v", got.list)
	}
	// A file holding null entries (from before this fix) loads without them.
	os.WriteFile(r.path, []byte(`[null,{"pid":7,"loop":"L","gen":1,"home":"H","at":1},null]`), 0o600)
	got := loadRegistry(r.path)
	if len(got.list) != 1 || got.list[0].Pid != 7 {
		t.Fatalf("loaded %+v", got.list)
	}
	got.bind(&snapshot{Inst: "1:1"}) // must not dereference a nil entry
}

func TestAnswersRunOnce(t *testing.T) {
	a := newAnswers()
	var runs atomic.Int32
	release := make(chan struct{})
	fn := func() *proto.Ack {
		runs.Add(1)
		<-release
		return &proto.Ack{OK: true, Note: "made"}
	}
	var wg sync.WaitGroup
	acks := make([]*proto.Ack, 5)
	for i := range acks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acks[i] = a.run("r1", fn)
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("ran %d times", runs.Load())
	}
	for _, ack := range acks {
		if !ack.OK || ack.Note != "made" || ack.ID != "r1" {
			t.Fatalf("%+v", ack)
		}
	}
	if ack := a.run("r1", fn); runs.Load() != 1 || !ack.OK {
		t.Fatal("a later repeat is answered from memory")
	}
}

func TestKeyBindingParse(t *testing.T) {
	lines := []string{
		"bind-key    -T prefix o       select-pane -t :.+",
		"bind-key -r -T prefix L       switch-client -l",
		"bind-key    -T root   M-o     run-shell -C 'display-popup -E x'",
	}
	if line, cmd := binding(lines, "prefix", "L"); cmd != "switch-client -l" || line != lines[1] {
		t.Fatalf("%q %q", line, cmd)
	}
	if _, cmd := binding(lines, "root", "M-o"); cmd != "run-shell -C 'display-popup -E x'" {
		t.Fatalf("%q", cmd)
	}
	if line, _ := binding(lines, "root", "M-p"); line != "" {
		t.Fatal("an unbound key found")
	}
	if got := hookAt([]string{"alert-bell[0] display-message x", "alert-bell[7193] display-message -c c tower-alert"}, "alert-bell"); got != "display-message -c c tower-alert" {
		t.Fatalf("%q", got)
	}
}

func TestBindingShellLineSetsTheEnvironment(t *testing.T) {
	d := &Daemon{
		towerEnv: []string{"TOWER_HOME=/tmp/with space", "TOWER_TMUX=-L ignored", "TOWER_SSH=it's"},
		env:      &config.Env{Tmux: []string{"-L", "the server"}, MKey: "0123456789ab"},
		tm:       tmux.Server{Bin: "/usr/bin/tmux"},
		self:     "/usr/bin/env",
	}
	k := &keys{w: &watcher{d: d}}
	line := strings.Replace(k.shellLine(0), "#{client_pid}:#{client_created}:#{client_name}", "1:2:/dev/tty", 1)
	out, err := exec.Command("/bin/sh", "-c", line).Output()
	if err != nil {
		t.Fatal(err, line)
	}
	for _, want := range []string{"TOWER_CLIENT=1:2:/dev/tty", "TOWER_HOME=/tmp/with space", "TOWER_TMUX=-L the server", "TOWER_SSH=it's", "TOWER_MKEY=0123456789ab", "TOWER_TMUX_BIN=/usr/bin/tmux"} {
		if !strings.Contains(string(out), want+"\n") {
			t.Fatalf("%q missing from the binding's environment:\n%s", want, out)
		}
	}
}

// A link made from the cache leaves the cache alone, and two links made
// from it show the same ages: an offline host's sessions age once.
func TestCacheAgesOnce(t *testing.T) {
	heard := time.Now().Add(-time.Hour)
	c := cachedHost{ID: "abcd1234", Sessions: []proto.Session{{ID: "$1", Name: "s", Ago: 1000}}, Heard: heard.UnixMilli()}
	h := &homeRole{}
	l1 := newLink(h, config.Host{Name: "X"})
	l1.fromCache(c)
	l2 := newLink(h, config.Host{Name: "X"})
	l2.fromCache(c)
	if c.Sessions[0].Ago != 1000 {
		t.Fatalf("the cache changed: %d", c.Sessions[0].Ago)
	}
	a1, a2 := l1.sessions[0].Ago, l2.sessions[0].Ago
	want := int64(1000 + time.Hour/time.Millisecond)
	if a1-want > 1000 || a2-want > 1000 || a1 < want || a2 < want {
		t.Fatalf("ages %d and %d, want about %d", a1, a2, want)
	}
	// Saved again, the ages are as of the same moment.
	back := l2.toCache()
	if back.Heard != c.Heard || back.Sessions[0].Ago < 1000 || back.Sessions[0].Ago > 1100 {
		t.Fatalf("cached again: %+v", back)
	}
}

// Of two loops on one host, the one whose client moved becomes the last
// target, whichever order the clients come in.
func TestLastFollowsTheClientThatMoved(t *testing.T) {
	d := &Daemon{id: "home0001", name: "A", changed: make(chan struct{}), env: &config.Env{StateDir: t.TempDir()}}
	// savedAt now: the save applyClients starts writes nothing (it would
	// race the test's temporary directory going away).
	h := &homeRole{d: d, loops: map[string]*loopRec{}, clientsB: map[string][]proto.Client{}, savedAt: time.Now()}
	d.home = h
	s1 := proto.Ref{Host: "home0001", Session: "$1", Label: "s1"}
	h.loops["LA"] = &loopRec{id: "LA", gen: 1, cur: s1, seen: true}
	h.loops["LB"] = &loopRec{id: "LB", gen: 1, cur: s1, seen: true}
	sessions := []proto.Session{{ID: "$1", Name: "s1"}, {ID: "$2", Name: "s2"}}
	d.mu.Lock()
	h.applyClients("home0001", "A", "", sessions, []proto.Client{
		{Loop: "LA", Gen: 1, Home: "home0001", Session: "$2"}, // moved
		{Loop: "LB", Gen: 1, Home: "home0001", Session: "$1"}, // stayed, listed after
	})
	d.mu.Unlock()
	if h.last.Session != "$2" {
		t.Fatalf("last is %+v, want the client that moved, on $2", h.last)
	}
}

// A session ended, the loop goes back to its previous session even while
// the loop's own older client is still attached there, not yet reaped; a
// client of anyone else's keeps it taken.
func TestMoveOnPassesOverTheLoopsOwnOldClient(t *testing.T) {
	d := &Daemon{id: "home0001", name: "A", changed: make(chan struct{}), env: &config.Env{StateDir: t.TempDir()}}
	d.dirs = dirs.New(dirs.Options{Home: t.TempDir()})
	h := &homeRole{d: d, loops: map[string]*loopRec{}, clientsB: map[string][]proto.Client{}, savedAt: time.Now(), reaping: map[string]*reapState{}}
	d.home = h
	d.snap = &snapshot{At: time.Now(), Sessions: []proto.Session{
		{ID: "$1", Name: "prev", Attached: 1, Ago: 5000},
		{ID: "$2", Name: "other", Ago: 9000},
	}}
	ended := proto.Ref{Host: "home0001", Session: "$3"}
	h.loops["L"] = &loopRec{id: "L", gen: 3, cur: ended, prev: proto.Ref{Host: "home0001", Session: "$1"}, seen: true}
	for _, tc := range []struct {
		name   string
		client proto.Client
		want   string
	}{
		{"its own older attach", proto.Client{Loop: "L", Gen: 2, Home: "home0001", Session: "$1"}, "$1"},
		{"another home's client", proto.Client{Loop: "L", Gen: 2, Home: "home0002", Session: "$1"}, "$2"},
		{"another loop's live client", proto.Client{Loop: "M", Gen: 1, Home: "home0001", Session: "$1"}, "$2"},
	} {
		h.clientsB["home0001"] = []proto.Client{tc.client}
		if tc.client.Loop == "M" {
			h.loops["M"] = &loopRec{id: "M", gen: 1, seen: true}
		}
		next, ok := h.moveOn("L", ended)
		if !ok || next.Session != tc.want {
			t.Fatalf("%s: moved on to %+v (%v), want %s", tc.name, next, ok, tc.want)
		}
	}
	// Its own older attach on a host whose towerd cannot detach it (the
	// reap was told "unknown request"): it stays, so the session is taken.
	old := proto.Client{Loop: "L", Gen: 2, Home: "home0001", Session: "$1", Pid: 7, Name: "/dev/pts/7"}
	h.clientsB["home0001"] = []proto.Client{old}
	h.reaping[reapKey("home0001", old)] = &reapState{next: time.Now().Add(reapOldPeer)}
	if next, ok := h.moveOn("L", ended); !ok || next.Session != "$2" {
		t.Fatalf("a client the reap cannot detach: moved on to %+v (%v), want $2", next, ok)
	}
}

// Views call this machine local; messages name it by its host name.
func TestLocalName(t *testing.T) {
	t.Setenv("TOWER_TEST_NAME", "")
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	if name, label := localName(); name != h || label != proto.LocalName {
		t.Fatalf("localName = %q, %q; want %q, %q", name, label, h, proto.LocalName)
	}
	t.Setenv("TOWER_TEST_NAME", "A")
	if name, label := localName(); name != "A" || label != "A" {
		t.Fatalf("with TOWER_TEST_NAME: %q, %q", name, label)
	}
}

// A link that drops while a request waits for its first pong: the
// request hears at once that the connection was lost, not, near its
// deadline, that too little time was left.
func TestRouteHearsALinkDropBeforeItsFirstPong(t *testing.T) {
	d := &Daemon{id: "home0001", name: "A", changed: make(chan struct{}), env: &config.Env{StateDir: t.TempDir()}}
	h := &homeRole{d: d, loops: map[string]*loopRec{}, clientsB: map[string][]proto.Client{}, savedAt: time.Now(), links: map[string]*link{}}
	d.home = h
	r, w := io.Pipe() // a peer that never answers
	defer w.Close()
	conn := stream.New(r, io.Discard, stream.Options{})
	conn.SetHelloRTT(5 * time.Second) // the connect's: a 3s request waits for a pong
	h.links["B"] = &link{h: h, cfg: config.Host{Name: "B"}, id: "host0002", status: proto.StatusUp, conn: conn}
	h.order = []string{"B"}
	time.AfterFunc(50*time.Millisecond, func() { conn.Close(nil) })
	start := time.Now()
	ack := h.route(context.Background(), &proto.Request{ID: "r", Op: proto.OpKill, Target: proto.Ref{Host: "host0002", Session: "$1"}, Deadline: stream.Now() + 3000})
	if el := time.Since(start); el > time.Second || ack.Err != "B: connection lost" {
		t.Fatalf("after %v: %+v, want B: connection lost at once", el.Round(time.Millisecond), ack)
	}
}

// A link's generation grows with each connect, and a new link's (the
// host's entry changed, or the home restarted) is past an old one's.
func TestLinkGenGrows(t *testing.T) {
	a := nextLinkGen(0)
	if b := nextLinkGen(a); b <= a {
		t.Fatalf("next %d after %d", b, a)
	}
	time.Sleep(2 * time.Millisecond)
	if c := nextLinkGen(0); c <= a {
		t.Fatalf("a new link's %d is not past an earlier one's %d", c, a)
	}
}

// tmuxBefore reads #{version}: a letter after the minor, a next- build,
// and builds with no version number (master, OpenBSD's) are not before.
func TestTmuxBefore(t *testing.T) {
	for v, want := range map[string]bool{
		"3.2": true, "3.2a": true, "2.9a": true, "3.3": false, "3.3a": false, "3.7c": false,
		"4.0": false, "next-3.3": false, "next-3.2": true, "master": false, "openbsd-7.6": false, "": false,
	} {
		if got := tmuxBefore(v, 3, 3); got != want {
			t.Errorf("tmuxBefore(%q, 3, 3) = %v, want %v", v, got, want)
		}
	}
}
