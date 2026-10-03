package towerd

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/tmux"
)

func TestPacerBurstThenPaced(t *testing.T) {
	var mu sync.Mutex
	var runs []time.Time
	p := newPacer(100*time.Millisecond, 2, func() {
		mu.Lock()
		runs = append(runs, time.Now())
		mu.Unlock()
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
	p := newPacer(time.Millisecond, 2, func() {
		if n.Add(1) == 1 {
			<-release
		}
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
	r.save()
	r2 := loadRegistry(r.path)
	if r2.count() != 1 || r2.list[0].Pid != 13 {
		t.Fatalf("restored: %+v", r2.list)
	}
}

func TestParseClient(t *testing.T) {
	pid, created, name, ok := parseClient("42:1700000000:/dev/ttys003")
	if !ok || pid != 42 || created != "1700000000" || name != "/dev/ttys003" {
		t.Fatal(pid, created, name, ok)
	}
	if _, _, name, _ := parseClient("1:2:client:with:colons"); name != "client:with:colons" {
		t.Fatal(name)
	}
	for _, bad := range []string{"", "x:1:n", "1:2", "-1:2:n"} {
		if _, _, _, ok := parseClient(bad); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
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
