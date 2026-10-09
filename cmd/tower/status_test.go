package main

import (
	"strings"
	"testing"

	"github.com/jerrykal/tower/internal/proto"
)

// The full status shows each loop's standbys by host name, the last
// detach into a standby, and which clients a detach makes standbys.
func TestFormatStatusReuse(t *testing.T) {
	st := &proto.Status{ID: "h0", Version: "v", Detail: &proto.Detail{
		Links: []proto.LinkStatus{{Name: "bravo", ID: "h2", Status: proto.StatusUp}},
		Loops: []proto.LoopStatus{{ID: "L", Gen: 3,
			Standbys: []proto.StandbyStatus{{Host: "h2", State: "ready", Again: true, Ms: 12000, Opened: 1, Reused: 5}, {Host: "h3", Opened: 2, GivenUp: 1, Why: "relay: timed out"}},
			End:      &proto.EndStatus{Gen: 3, OK: true, Ms: 4, Ago: 2000}}},
		Clients: []proto.Client{{Name: "/dev/pts/3", Pid: 7, Loop: "L", Home: "h0", Reuse: true}},
	}}
	got := formatStatus(st)
	for _, want := range []string{
		"  standby bravo: ready 12s, reusable; sessions opened 1, reused 5, given up 0\n",
		"  standby h3: none; sessions opened 2, reused 0, given up 1 (last: relay: timed out)\n",
		"  last detach into a standby: gen 3 ok=true in 4ms, 2s ago\n",
		"client /dev/pts/3 (pid 7) of loop L at home h0, reusable\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
}
