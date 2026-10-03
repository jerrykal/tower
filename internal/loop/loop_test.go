package loop

import (
	"encoding/json"
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

func TestTowerless(t *testing.T) {
	got := towerless([]string{"TOWER_TMUX=-L x", "PATH=/bin", "HOME=/h", "TOWER_TEST_TIMING=/t"})
	if !slices.Equal(got, []string{"HOME=/h", "PATH=/bin"}) {
		t.Fatalf("%q", got)
	}
}
