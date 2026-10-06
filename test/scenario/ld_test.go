package scenario

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

// LD02: deadlines at 400ms round trips: requests from B's dashboard to C
// through the home, with deadlines from 500ms to 2s; one that was
// reported failed never ran.
func TestLD02(t *testing.T) {
	w := NewWorld(t, "ld02")
	a := w.Host("A", []string{"alpha", "apple"})
	b := w.Host("B", []string{"bravo"})
	c := w.Host("C", []string{"charlie"})
	for _, n := range []string{"B", "C"} {
		w.Shape(n, func(l *Link) { l.DelayMs = 200 })
	}
	w.Home(a, b.Remote(), c.Remote())
	w.WaitLink(a, "B", "up", 15*time.Second)
	w.WaitLink(a, "C", "up", 15*time.Second)
	l := w.FakeLoop(a)
	l.Attach(w.Ref(a, "B", "bravo"), b)
	time.Sleep(1500 * time.Millisecond)
	cl := b.ClientIDs("bravo")[0]
	cid := w.Link(a, "C").ID
	var lines []string
	failedButRan := 0
	for _, to := range []int{500, 700, 800, 900, 1000, 1100, 1300, 2000} {
		for i := range 2 {
			name := fmt.Sprintf("dl%d_%d", to, i)
			ack, el := b.Act(cl, proto.Request{Op: proto.OpNew, Target: proto.Ref{Host: cid}, Name: name}, to)
			time.Sleep(1200 * time.Millisecond)
			made := strings.Contains(","+strings.Join(c.Sessions(), ",")+",", ","+name+",")
			lines = append(lines, fmt.Sprintf("%5dms: ok %-5v made %-5v in %v %s", to, ack.OK, made, el.Round(time.Millisecond), ack.Err))
			if !ack.OK && made {
				failedButRan++
			}
		}
	}
	t.Logf("requests B → home → C at 400ms round trips:\n%s", strings.Join(lines, "\n"))
	if failedButRan > 0 {
		t.Fatalf("%d requests reported failed ran anyway", failedButRan)
	}
}
