package loop

import (
	"bytes"
	"github.com/jerrykal/tower/internal/relay"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestAttachCommand(t *testing.T) {
	got := AttachCommand("$3", "@7", "", "123:456", "")
	want := []string{"attach-session", "-t", "$3", ";", "if-shell", "-F", "#{!=:#{pid}:#{start_time},123:456}", "detach-client -E 'exit 43'", ";", "select-window", "-t", "@7"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	if got := AttachCommand("$3", "", "", "", ""); !slices.Equal(got, []string{"attach-session", "-t", "$3"}) {
		t.Fatalf("%q", got)
	}
	got = AttachCommand("$3", "", "", "", "tower: C:#1 ended")
	want = []string{"attach-session", "-t", "$3", ";", "display-message", "-d", "4000", "tower: C:##1 ended"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
}

func TestReadLineTakesOneLine(t *testing.T) {
	r := bytes.NewBufferString("\r\n{\"s\":\"$1\"}\nkeys typed after")
	line, err := readLine(r, nil)
	if err != nil || string(line) != `{"s":"$1"}` {
		t.Fatalf("%q %v", line, err)
	}
	if r.String() != "keys typed after" {
		t.Fatalf("read past the line: %q left", r.String())
	}
}

func TestStandbyWaitsForItsGoLine(t *testing.T) {
	m, s, err := testPty()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer m.Close()
	defer s.Close()
	before, _ := getTermios(int(s.Fd()))
	type res struct {
		g   string
		err error
	}
	done := make(chan res, 1)
	go func() {
		g, err := waitGo(s, s)
		if err != nil {
			done <- res{err: err}
			return
		}
		done <- res{g: g.Loop + " " + g.Session + " " + g.Window}
	}()
	// The ready marker comes first, with the pty's echo off by then.
	buf := make([]byte, 0, 64)
	tmp := make([]byte, 64)
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(string(buf), relay.MarkerReady) && time.Now().Before(deadline) {
		m.SetReadDeadline(time.Now().Add(time.Second))
		n, _ := m.Read(tmp)
		buf = append(buf, tmp[:n]...)
	}
	if !strings.Contains(string(buf), relay.MarkerReady) {
		t.Fatalf("no ready marker: %q", buf)
	}
	io.WriteString(m, `{"loop":"L1","gen":2,"home":"h","inst":"1:2","s":"$3","w":"@7"}`+"\nleft for tmux")
	r := <-done
	if r.err != nil || r.g != "L1 $3 @7" {
		t.Fatalf("%+v", r)
	}
	after, _ := getTermios(int(s.Fd()))
	if before.Lflag&unix.ECHO != after.Lflag&unix.ECHO || before.Lflag&unix.ICANON != after.Lflag&unix.ICANON {
		t.Fatal("the pty's modes were not restored")
	}
	m.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	n, _ := m.Read(tmp)
	if got := string(tmp[:n]); got != relay.MarkerGo {
		t.Fatalf("after the go line the pty shows %q", got)
	}
	// Nothing after the line was taken: tmux, taking the pty raw, reads it.
	raw := *after
	rawMode(&raw)
	setTermios(int(s.Fd()), &raw)
	if got := readWithin(s, time.Second); got != "left for tmux" {
		t.Fatalf("the shim took %q from what followed the go line", got)
	}
}

// readWithin reads what f has within d (a pty slave ignores deadlines).
func readWithin(f io.Reader, d time.Duration) string {
	ch := make(chan string, 1)
	go func() {
		b := make([]byte, 64)
		n, _ := f.Read(b)
		ch <- string(b[:n])
	}()
	select {
	case s := <-ch:
		return s
	case <-time.After(d):
		return "(nothing)"
	}
}

// A standby that stops hearing its loop's heartbeats exits: a session's
// hang-up does not always reach it.
func TestStandbyExitsWithoutHeartbeats(t *testing.T) {
	m, s, err := testPty()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer m.Close()
	defer s.Close()
	t.Setenv("TOWER_STANDBY_SILENCE", "300ms")
	exited := make(chan time.Time, 1)
	old := standbyExit
	standbyExit = func() { exited <- time.Now() }
	defer func() { standbyExit = old }()
	go waitGo(s, s)
	// Heartbeats keep it waiting.
	stop := time.Now().Add(time.Second)
	for time.Now().Before(stop) {
		io.WriteString(m, "\n")
		time.Sleep(100 * time.Millisecond)
		select {
		case <-exited:
			t.Fatal("exited while it heard heartbeats")
		default:
		}
	}
	quiet := time.Now()
	select {
	case at := <-exited:
		if d := at.Sub(quiet); d > 700*time.Millisecond {
			t.Fatalf("exited %v after the last heartbeat", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a standby without heartbeats kept waiting")
	}
}

func TestAttachCommandSelectsThePane(t *testing.T) {
	got := AttachCommand("$3", "@7", "%12", "", "")
	want := []string{"attach-session", "-t", "$3", ";", "select-window", "-t", "@7", ";", "select-pane", "-t", "%12"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
}
