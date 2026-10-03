package loop

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
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

func TestTowerless(t *testing.T) {
	got := towerless([]string{"TOWER_TMUX=-L x", "PATH=/bin", "HOME=/h", "TOWER_TEST_TIMING=/t"})
	if !slices.Equal(got, []string{"HOME=/h", "PATH=/bin"}) {
		t.Fatalf("%q", got)
	}
}
