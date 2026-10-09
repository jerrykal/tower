package loop

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/relay"
)

// shPrintf is a shell printf of s, every byte octal-escaped.
func shPrintf(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, `\%03o`, s[i])
	}
	return "printf '" + b.String() + "'; "
}

// recycleSet is a standby set on a test terminal, and a session running
// script, in use by an attach that will hand it back.
func recycleSet(t *testing.T, script string) (*standbys, *standby) {
	t.Helper()
	m, tty, err := testPty()
	if err != nil {
		t.Skip("no pty:", err)
	}
	t.Cleanup(func() { m.Close(); tty.Close() })
	modes, err := relay.GetModes(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	s := newStandbys(&attachLoop{tty: int(tty.Fd()), orig: modes})
	sess, err := relay.Start(sh(`stty raw -echo; `+script), nil, modes, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeAll)
	t.Cleanup(sess.Close)
	sb := newStandby("h", "B", "k", sess, towerless(os.Environ()), modes, inUse)
	s.byHost["h"] = sb
	return s, sb
}

// A session whose client is detached back into a standby is the host's
// standby again: drained to the ended marker (a ready marker its client
// printed before that is not taken for the shim's), then ready; an attach
// switching back meanwhile waits for it.
func TestRecycle(t *testing.T) {
	s, sb := recycleSet(t, `printf 'goodbye'; `+shPrintf(relay.MarkerReady)+`sleep 0.2; `+shPrintf(relay.Ended("n1")+relay.MarkerAgain+relay.MarkerReady)+`sleep 10`)
	start := time.Now()
	s.recycle(sb, "n1", time.Second)
	got := s.take("h", "k", 2*time.Second, true)
	if got == nil || !got.again || got.sess != sb.sess {
		t.Fatalf("took %+v", got)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("taken after %v: before its client ended", d)
	}
	if got.state != inUse || s.byHost["h"] != got {
		t.Fatal("a reusable standby taken is not in the set, in use")
	}
	got.sess.Kill()
	got.sess.Close()
}

// A session whose client is not detached in time, or under another
// nonce, or whose ssh exits meanwhile, is given up: killed and out of the
// set, its host not backed off.
func TestRecycleGivesUp(t *testing.T) {
	for _, script := range []string{`sleep 10`, shPrintf(relay.Ended("n2")+relay.MarkerAgain+relay.MarkerReady) + `sleep 10`, `printf goodbye`} {
		s, sb := recycleSet(t, script)
		s.recycle(sb, "n1", 200*time.Millisecond)
		select {
		case <-sb.sess.Done():
		case <-time.After(2 * time.Second):
			t.Fatalf("%q: the session was not ended", script)
		}
		time.Sleep(20 * time.Millisecond)
		if s.take("h", "k", 0, true) != nil {
			t.Fatalf("%q: a given-up session was taken", script)
		}
		if s.backoff["h"] != nil {
			t.Fatalf("%q: a given-up recycle backed the host off", script)
		}
	}
}

// A host whose standby is no longer the attach's keeps it; the session
// handed back is ended.
func TestRecycleSlotTaken(t *testing.T) {
	s, sb := recycleSet(t, shPrintf(relay.Ended("n1")+relay.MarkerAgain+relay.MarkerReady)+`sleep 10`)
	_, other := recycleSet(t, `sleep 10`)
	s.byHost["h"] = other
	s.recycle(sb, "n1", time.Second)
	select {
	case <-sb.sess.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the session handed back was kept")
	}
	delete(s.byHost, "h")
	other.sess.Kill()
	other.sess.Close()
}

// No standby starts for the host whose session the running attach will
// hand back, nor is that session dropped, offered or not; once the
// attach lets it go, one starts.
func TestApplySkipsTheAttachedHost(t *testing.T) {
	s := newStandbys(&attachLoop{tty: -1})
	started := make(chan string, 3)
	s.spawn = func(argv, env []string, m relay.Modes, rows, cols int) (*relay.Session, error) {
		started <- argv[0]
		return nil, errors.New("no session in this test")
	}
	sb := newStandby("h1", "B", "old", nil, nil, relay.Modes{}, inUse)
	s.byHost["h1"] = sb
	v := &proto.View{Hosts: []proto.Host{{ID: "h1", Name: "B"}, {ID: "h2", Name: "C"}}}
	s.apply([]proto.Offer{{Host: "h1", Key: "k", Argv: []string{"ssh-b"}}, {Host: "h2", Key: "k", Argv: []string{"ssh-c"}}}, v)
	if s.byHost["h1"] != sb {
		t.Fatal("the attach's session was dropped")
	}
	s.release(sb)
	s.backoff = map[string]*hostBackoff{}
	s.apply([]proto.Offer{{Host: "h1", Key: "k", Argv: []string{"ssh-b"}}}, v)
	close(started)
	var got []string
	for a := range started {
		got = append(got, a)
	}
	if len(got) != 2 || got[0] != "ssh-c" || got[1] != "ssh-b" {
		t.Fatalf("started %q", got)
	}
}
