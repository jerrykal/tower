package relay

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// printf is a shell printf of s, every byte octal-escaped, and a pause
// after each byte when slow.
func printf(s string, slow bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, `printf '\%03o'; `, s[i])
		if slow {
			b.WriteString("sleep 0.002; ")
		}
	}
	return b.String()
}

func TestRelayRelease(t *testing.T) {
	// Release ends an armed relay at once and leaves the session running:
	// a second relay on it shows what comes next; one released before it
	// began ends at once. Relayed counts each relay on its own.
	s := startSession(t, sh(`stty raw -echo; printf one; read x; printf two; read y; printf three; sleep 5`), nil, cookedModes(t))
	tt := newTestTerminal(t, 24, 80)
	c := collect(tt)
	s.Reuse()
	ch := relay(s, tt.Terminal)
	c.until(t, "one", 5*time.Second)
	if s.Relayed() != 3 {
		t.Fatalf("relayed %d", s.Relayed())
	}
	start := time.Now()
	s.Release()
	if res := <-ch; res.err != ErrReleased {
		t.Fatalf("relay: %+v", res)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("released after %v", d)
	}
	s.Release() // after the relay: nothing
	if err := s.Send(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadUntil([]byte("two"), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	s.Reuse()
	s.Release()
	if res := <-relay(s, tt.Terminal); res.err != ErrReleased {
		t.Fatalf("released before it began: %+v", res)
	}
	s.Reuse()
	ch = relay(s, tt.Terminal)
	s.Send(nil)
	c.until(t, "three", 2*time.Second)
	if s.Relayed() != 5 {
		t.Fatalf("relayed %d in the third relay", s.Relayed())
	}
	s.Release()
	<-ch
}

func TestDrain(t *testing.T) {
	// Drain discards up to its own nonce's marker, however it comes, and
	// keeps what follows; another nonce's marker is just output.
	for _, slow := range []bool{false, true} {
		s := startSession(t, sh(`stty raw -echo; printf 'goodbye'; `+printf(Ended("n0"), false)+printf(Ended("n9"), slow)+printf(MarkerAgain+MarkerReady, false)+`sleep 5`), nil, cookedModes(t))
		if err := s.Drain("n9", 2*time.Second); err != nil {
			t.Fatalf("slow %v: drain: %v", slow, err)
		}
		before, err := s.ReadUntil([]byte(MarkerReady), time.Second)
		if err != nil || string(before) != MarkerAgain {
			t.Fatalf("slow %v: after the drain %q %v", slow, before, err)
		}
	}
	s := startSession(t, sh(`stty raw -echo; printf 'x'; sleep 5`), nil, cookedModes(t))
	if err := s.Drain("n9", 100*time.Millisecond); err != ErrTimeout {
		t.Fatalf("no marker: %v", err)
	}
	s = startSession(t, sh(`stty raw -echo; printf 'x'`), nil, cookedModes(t))
	if err := s.Drain("n9", 2*time.Second); err != ErrExited {
		t.Fatalf("exited: %v", err)
	}
}
