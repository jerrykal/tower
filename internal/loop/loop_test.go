package loop

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/relay"
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

// TestWatchCtrlC: the watch sees ctrl-c among other keys, and once
// stopped takes nothing more from the terminal.
func TestWatchCtrlC(t *testing.T) {
	p, err := relay.OpenPty()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer p.Close()
	sfd := int(p.Slave.Fd())
	m, _ := relay.GetModes(sfd)
	relay.SetModes(sfd, m.Raw())
	hit, stop := watchCtrlC(sfd)
	p.Master.Write([]byte("ab\x03"))
	select {
	case <-hit:
	case <-time.After(2 * time.Second):
		t.Fatal("ctrl-c not seen")
	}
	stop()
	hit, stop = watchCtrlC(sfd)
	stop()
	p.Master.Write([]byte("xyz"))
	buf := make([]byte, 3)
	p.Slave.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _ := io.ReadFull(p.Slave, buf); n != 3 || string(buf) != "xyz" {
		t.Fatalf("after the watch stopped the terminal holds %q", buf[:n])
	}
	select {
	case <-hit:
		t.Fatal("a stopped watch reported ctrl-c")
	default:
	}
}

// TestStandbyStartOutsideLock: a hand-off taking a standby does not wait
// for the refresh starting another (a pty, ssh's fork and exec).
func TestStandbyStartOutsideLock(t *testing.T) {
	s := newStandbys(&attachLoop{tty: -1})
	started := make(chan struct{})
	s.spawn = func(argv, env []string, m relay.Modes, rows, cols int) (*relay.Session, error) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		return nil, errors.New("no session in this test")
	}
	v := &proto.View{Hosts: []proto.Host{{ID: "h1", Name: "B"}}}
	done := make(chan struct{})
	go func() {
		s.apply([]proto.Offer{{Host: "h1", Key: "k", Argv: []string{"ssh"}}}, v)
		close(done)
	}()
	<-started
	start := time.Now()
	s.take("h2", "k")
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("take waited %v for a standby starting", d)
	}
	<-done
	if s.backoff["h1"] == nil {
		t.Fatal("a failed start did not back its host off")
	}
}

func TestTTYName(t *testing.T) {
	p, err := relay.OpenPty()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer p.Close()
	got, err := ttyName(int(p.Slave.Fd()))
	if err != nil || got != p.Name {
		t.Fatalf("ttyName %q %v, want %q", got, err, p.Name)
	}
}

func TestTowerless(t *testing.T) {
	got := towerless([]string{"TOWER_TMUX=-L x", "PATH=/bin", "HOME=/h", "TOWER_TEST_TIMING=/t"})
	if !slices.Equal(got, []string{"HOME=/h", "PATH=/bin"}) {
		t.Fatalf("%q", got)
	}
}

// TestWatchHostLink: an attach prepared on a link is given up for that
// link, or a later one, going; a view still showing the earlier link
// (sent before the link came up and the prepare answered) is no reason.
func TestWatchHostLink(t *testing.T) {
	l := &attachLoop{id: "L"}
	l.views = newViewer(l)
	view := func(status string, link int) *proto.Dash {
		return &proto.Dash{View: proto.View{Hosts: []proto.Host{{ID: "h", Name: "R", Status: status, Link: link}}}}
	}
	l.views.set(view(proto.StatusConnecting, 1))
	lost := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.watchHost(ctx, &proto.Prepared{Gen: 2, Target: proto.Ref{Host: "h"}, Link: 2}, lost)
	select {
	case why := <-lost:
		t.Fatalf("given up on a view of the earlier link: %s", why)
	case <-time.After(50 * time.Millisecond):
	}
	l.views.set(view(proto.StatusUp, 2))
	l.views.set(view(proto.StatusStalled, 2))
	select {
	case why := <-lost:
		if why != "R is not responding" {
			t.Fatalf("given up: %q", why)
		}
	case <-time.After(time.Second):
		t.Fatal("not given up when the link it was prepared on stalled")
	}
}
