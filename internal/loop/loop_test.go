package loop

import (
	"encoding/json"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/relay"
)

func TestBackoff(t *testing.T) {
	var got []time.Duration
	d := time.Duration(0)
	for range 8 {
		d = nextBackoff(d)
		got = append(got, d)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestNotes(t *testing.T) {
	local := []string{"/bin/tower", "attach", "--loop", "L", "$1"}
	if got := withNoteArgv(local, "", false); !slices.Equal(got, local) {
		t.Fatalf("no note: %q", got)
	}
	if got := withNoteArgv(local, "B is not responding", false); !slices.Equal(got, append(slices.Clone(local), "--note", "B is not responding")) {
		t.Fatalf("local: %q", got)
	}
	remote := []string{"ssh", "-t", "B", "--", "tower attach --loop L '$1'"}
	got := withNoteArgv(remote, "it's ended", true)
	if got[len(got)-1] != `tower attach --loop L '$1' --note 'it'\''s ended'` || remote[len(remote)-1] != "tower attach --loop L '$1'" {
		t.Fatalf("remote: %q (argv was %q)", got, remote)
	}
	line, _ := json.Marshal(proto.GoLine{Loop: "L", Gen: 3, Session: "$1"})
	var g proto.GoLine
	if err := json.Unmarshal([]byte(withNoteGo(string(line), "now on B")), &g); err != nil || g.Note != "now on B" || g.Gen != 3 || g.Session != "$1" {
		t.Fatalf("go line: %+v %v", g, err)
	}
}

// TestWatchCtrlC: the watch sees ctrl-c among other keys, and once
// stopped takes nothing more from the terminal.
func TestWatchCtrlC(t *testing.T) {
	p, err := relay.OpenPty()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer p.Close()
	sfd := int(p.Slave.Fd())
	m, _ := relay.GetModes(sfd)
	relay.SetModes(sfd, m.Raw())
	hit, stop := watchCtrlC(sfd)
	p.Master.Write([]byte("ab\x03"))
	select {
	case <-hit:
	case <-time.After(2 * time.Second):
		t.Fatal("ctrl-c not seen")
	}
	stop()
	hit, stop = watchCtrlC(sfd)
	stop()
	p.Master.Write([]byte("xyz"))
	buf := make([]byte, 3)
	p.Slave.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _ := io.ReadFull(p.Slave, buf); n != 3 || string(buf) != "xyz" {
		t.Fatalf("after the watch stopped the terminal holds %q", buf[:n])
	}
	select {
	case <-hit:
		t.Fatal("a stopped watch reported ctrl-c")
	default:
	}
}

// TestStandbyStartOutsideLock: a hand-off taking a standby does not wait
// for the refresh starting another (a pty, ssh's fork and exec).
func TestStandbyStartOutsideLock(t *testing.T) {
	s := newStandbys(&attachLoop{tty: -1})
	started := make(chan struct{})
	s.spawn = func(argv, env []string, m relay.Modes, rows, cols int) (*relay.Session, error) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		return nil, errors.New("no session in this test")
	}
	v := &proto.View{Hosts: []proto.Host{{ID: "h1", Name: "B"}}}
	done := make(chan struct{})
	go func() {
		s.apply([]proto.Offer{{Host: "h1", Key: "k", Argv: []string{"ssh"}}}, v)
		close(done)
	}()
	<-started
	start := time.Now()
	s.take("h2", "k")
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("take waited %v for a standby starting", d)
	}
	<-done
	if s.backoff["h1"] == nil {
		t.Fatal("a failed start did not back its host off")
	}
}

func TestTTYName(t *testing.T) {
	p, err := relay.OpenPty()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer p.Close()
	got, err := ttyName(int(p.Slave.Fd()))
	if err != nil || got != p.Name {
		t.Fatalf("ttyName %q %v, want %q", got, err, p.Name)
	}
}

func TestTowerless(t *testing.T) {
	got := towerless([]string{"TOWER_TMUX=-L x", "PATH=/bin", "HOME=/h", "TOWER_TEST_TIMING=/t"})
	if !slices.Equal(got, []string{"HOME=/h", "PATH=/bin"}) {
		t.Fatalf("%q", got)
	}
}
