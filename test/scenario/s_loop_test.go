package scenario

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
)

// Scenarios of the attach loop: hand-offs, exits, the picker.

// stdSetup is the standard setup: a home on a listing remotes, all up.
func stdSetup(w *World, a *Host, remotes ...*Host) {
	w.T.Helper()
	var rs []config.Host
	for _, r := range remotes {
		rs = append(rs, r.Remote())
	}
	w.Home(a, rs...)
	names := []string{a.Name}
	for _, r := range remotes {
		names = append(names, r.Name)
	}
	w.WaitUp(a, names...)
}

// clientsAre waits until host's clients are exactly want.
func (w *World) clientsAre(h *Host, d time.Duration, want ...string) {
	w.T.Helper()
	slices.Sort(want)
	w.Eventually(d, h.Name+"'s clients are "+strings.Join(want, ","), func() bool {
		got := h.Clients()
		return slices.Equal(got, want) || len(got) == 0 && len(want) == 0
	})
}

// S00: the happy path: a loop picks a remote session, the popup there
// hands off to the laptop, and a local switch keeps the previous one.
func TestS00(t *testing.T) {
	w := NewWorld(t, "s00")
	a := w.Host("A", []string{"alpha", "apple"})
	b := w.Host("B", []string{"bravo", "banana"}, SSHHost())
	stdSetup(w, a, b)
	term := w.Loop("t", a, nil)
	term.Wait(Prompt, 5*time.Second)
	term.Pick("bravo")
	w.WaitLoop(a, "^B:bravo", 5*time.Second)
	w.clientsAre(b, 3*time.Second, "bravo")
	time.Sleep(300 * time.Millisecond)
	term.DashTo("apple")
	w.WaitLoop(a, "^A:apple", 8*time.Second)
	w.clientsAre(b, 3*time.Second)
	w.clientsAre(a, 3*time.Second, "apple")
	time.Sleep(300 * time.Millisecond)
	term.DashTo("alpha")
	l := w.WaitLoop(a, "^A:alpha", 5*time.Second)
	if !strings.HasPrefix(FormatRef(l.Prev), "A:apple") {
		t.Fatalf("previous is %s, want A:apple", FormatRef(l.Prev))
	}
}
