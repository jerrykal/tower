package ui

import (
	"slices"
	"strconv"
	"testing"

	"github.com/jerrykal/tower/internal/proto"
)

// TestRanking: the best match first, as with fzf; ties in recency order;
// the cursor on the top row.
func TestRanking(t *testing.T) {
	host := func(id, name string, sessions ...string) proto.Host {
		h := proto.Host{ID: id, Name: name, Status: proto.StatusUp, Inst: "1:1"}
		for i, s := range sessions {
			h.Sessions = append(h.Sessions, proto.Session{ID: "$" + strconv.Itoa(i), Name: s, Ago: int64(i) * 1000})
		}
		return h
	}
	d := proto.Dash{Gen: 1, Self: "aaaa", View: proto.View{Hosts: []proto.Host{
		host("aaaa", "R150", "s150", "cobra", "xs50y", "s50-logs"),
		host("bbbb", "R50", "s50", "bravo", "s5"),
	}}}
	d.View.Hosts[1].Sessions[0].Ago = 99_000 // s50 the least recent of all
	m, _, _ := newTestModel(t, d, false)
	for _, c := range []struct {
		q    string
		want []string
	}{
		// The name itself, then a prefix, then inside a word, then
		// scattered.
		{"s50", []string{"R50 s50", "R150 s50-logs", "R150 xs50y", "R150 s150"}},
		// A prefix of two names: both, in recency order.
		{"s5", []string{"R50 s5", "R150 s50-logs", "R50 s50", "R150 xs50y", "R150 s150"}},
		// At a word start before inside a word.
		{"bra", []string{"R50 bravo", "R150 cobra"}},
		{"logs", []string{"R150 s50-logs"}},
		// Host and name, spaces aside, before scattered characters.
		{"r50 s", []string{"R50 s5", "R50 s50", "R150 s150", "R150 s50-logs"}},
		{"S150", []string{"R150 s150"}},
	} {
		press(t, m, "ctrl+u")
		for _, r := range c.q {
			press(t, m, string(r))
		}
		got := names(m)
		if len(got) < len(c.want) || !slices.Equal(got[:len(c.want)], c.want) {
			t.Errorf("%q: %q, want %q first", c.q, got, c.want)
		}
		if cursorText(m) != c.want[0] {
			t.Errorf("%q: the cursor is on %q, want the top row", c.q, cursorText(m))
		}
	}
	// ⏎ takes the top row.
	press(t, m, "ctrl+u", "s", "5", "0")
	m.c.Pick = true
	press(t, m, "enter")
	if m.choice == nil || m.choice.Label != "s50" {
		t.Fatalf("⏎ took %+v, want s50", m.choice)
	}
}
