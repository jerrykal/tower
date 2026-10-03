package relay

import (
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// roundTrips types n single keys at the terminal emulator's side of a pty
// (master fd), each after the previous one's echo, and returns the times
// from writing a key to reading its echo, the first warm left out.
func roundTrips(t *testing.T, fd, n, warm int) durations {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var d durations
	key, echo := []byte{0}, []byte{0}
	for i := range n {
		key[0] = 'a' + byte(i%26)
		start := time.Now()
		if _, err := unix.Write(fd, key); err != nil {
			t.Fatal(err)
		}
		for {
			m, err := unix.Read(fd, echo)
			if err == unix.EINTR {
				continue
			}
			if err != nil || m != 1 {
				t.Fatalf("echo %d: %v", i, err)
			}
			break
		}
		took := time.Since(start)
		if echo[0] != key[0] {
			t.Fatalf("key %q echoed as %q", key, echo)
		}
		if i >= warm {
			d = append(d, took)
		}
	}
	return d
}

// LS07: the relay adds microseconds, not milliseconds, to a keystroke's
// echo.
func TestLS07KeystrokeLatency(t *testing.T) {
	const keys, warm = 3200, 200

	// Direct: cat on a raw pty as its session leader, the pty its
	// controlling terminal.
	p := newPty(t)
	sfd := fdOf(t, p.Slave)
	m, err := GetModes(sfd)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetModes(sfd, m.Raw()); err != nil {
		t.Fatal(err)
	}
	startDirect(t, p, []string{"cat"}, nil)
	direct := roundTrips(t, fdOf(t, p.Master), keys, warm)

	// Relayed: cat behind a standby's session, the terminal a pty.
	s, tt := startStandby(t)
	ch := relay(s, tt.Terminal)
	relayed := roundTrips(t, tt.mfd, keys, warm)
	s.Terminate()
	if res := <-ch; res.err != nil {
		t.Fatal(res.err)
	}

	dm, rm := direct.pct(0.5), relayed.pct(0.5)
	t.Logf("direct  median %v p99 %v", dm, direct.pct(0.99))
	t.Logf("relayed median %v p99 %v (+%v median, +%v p99)", rm, relayed.pct(0.99), rm-dm, relayed.pct(0.99)-direct.pct(0.99))
	if rm-dm >= time.Millisecond {
		t.Fatalf("the relay adds %v to the median echo", rm-dm)
	}
}
