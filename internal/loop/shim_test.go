package loop

import (
	"bytes"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/relay"

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
		g, err := waitGo(s, s, "", true)
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
	go waitGo(s, s, "", true)
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

// A standby ignores a line that is not a go line (input typed for an
// earlier client), and says it can go again before it says it is ready.
func TestStandbyIgnoresStrayLines(t *testing.T) {
	m, s, err := testPty()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer m.Close()
	defer s.Close()
	done := make(chan string, 1)
	go func() {
		g, err := waitGo(s, s, "", true)
		if err != nil {
			done <- err.Error()
			return
		}
		done <- g.Session + " " + g.Again
	}()
	buf := make([]byte, 0, 128)
	tmp := make([]byte, 128)
	for deadline := time.Now().Add(3 * time.Second); !strings.Contains(string(buf), relay.MarkerReady) && time.Now().Before(deadline); {
		m.SetReadDeadline(time.Now().Add(time.Second))
		n, _ := m.Read(tmp)
		buf = append(buf, tmp[:n]...)
	}
	if !strings.HasSuffix(string(buf), relay.MarkerAgain+relay.MarkerReady) {
		t.Fatalf("markers %q", buf)
	}
	io.WriteString(m, "ls -l")
	io.WriteString(m, "\n"+`{"loop":"L1","gen":3,"home":"h","s":"$4","again":"n7"}`+"\n")
	select {
	case got := <-done:
		if got != "$4 n7" {
			t.Fatalf("took %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the go line after a stray one was not taken")
	}
}

// A standby a client was detached into: input typed before it waits is
// thrown away (a go line among it too), it says the attach ended, that it
// can go again and that it is ready in one write, and a key that would
// signal it is only a byte while it waits.
func TestStandbyAfterDetach(t *testing.T) {
	m, s, err := testPty()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer m.Close()
	defer s.Close()
	io.WriteString(m, `{"loop":"L0","gen":1,"home":"h","s":"$9"}`+"\n")
	time.Sleep(50 * time.Millisecond)
	done := make(chan string, 1)
	go func() {
		g, err := waitGo(s, s, "n7", true)
		if err != nil {
			done <- err.Error()
			return
		}
		done <- g.Session
	}()
	want := relay.Ended("n7") + relay.MarkerAgain + relay.MarkerReady
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for deadline := time.Now().Add(3 * time.Second); !strings.Contains(string(buf), relay.MarkerReady) && time.Now().Before(deadline); {
		m.SetReadDeadline(time.Now().Add(time.Second))
		n, _ := m.Read(tmp)
		buf = append(buf, tmp[:n]...)
	}
	if !strings.HasSuffix(string(buf), want) {
		t.Fatalf("markers %q", buf)
	}
	if mode, _ := getTermios(int(s.Fd())); mode.Lflag&unix.ISIG != 0 {
		t.Fatal("the signal keys are on while it waits")
	}
	io.WriteString(m, `{"loop":"L1","gen":2,"home":"h","s":"$4"}`+"\n")
	select {
	case got := <-done:
		if got != "$4" {
			t.Fatalf("took %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no go line taken")
	}
}

// The command a detach runs in the client's place: every word quoted for
// any shell, or none when a word cannot be.
func TestResumeCommand(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	if strings.ContainsAny(exe, `'\`) {
		t.Skip("test binary path needs quoting")
	}
	got := resumeCommand(Shim{MKey: "k1", Tmux: []string{"-L", "tt-x"}}, "n7")
	want := "exec '" + exe + "' 'attach' '--standby' '--ended' 'n7' '--mkey' 'k1' '--tmux' '-L tt-x'"
	if got != want {
		t.Fatalf("%s", got)
	}
	if got := resumeCommand(Shim{}, "n7"); got != "exec '"+exe+"' 'attach' '--standby' '--ended' 'n7'" {
		t.Fatalf("no key, no tmux: %s", got)
	}
	if got := resumeCommand(Shim{Tmux: []string{"-S", `/tmp/a'b`}}, "n7"); got != "" {
		t.Fatalf("a quote: %s", got)
	}
	for n, want := range map[string]bool{"0a1b2c3d4e5f6789": true, "": false, "0A1B": false, "ab;cd": false, "ab\x07": false} {
		if validNonce(n) != want {
			t.Errorf("validNonce(%q) = %v", n, !want)
		}
	}
}

func sh(script string) []string { return []string{"/bin/sh", "-c", script} }
