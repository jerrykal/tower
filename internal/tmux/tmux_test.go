package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testServer starts a private tmux server for one test and stops it after.
func testServer(t *testing.T) Server {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ttx")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	s := Server{Bin: Resolve("tmux"), Args: []string{"-L", "tt-unit", "-f", "/dev/null"}}
	if _, err := s.Run(context.Background(), "new-session", "-d", "-s", "base", "-x", "80", "-y", "24"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Ending every session ends the server (exit-empty is on by default).
		out, _ := s.Run(context.Background(), "list-sessions", "-F", "#{session_id}")
		for _, id := range strings.Fields(out) {
			s.Run(context.Background(), "kill-session", "-t", id)
		}
		os.RemoveAll(dir)
	})
	return s
}

func TestQuoteRoundTrip(t *testing.T) {
	s := testServer(t)
	// tmux stores names vis-encoded (a backslash becomes two), so the
	// expectation for each name is what tmux makes of it given directly as
	// an argument.
	names := []string{"plain", "it's", `back\slash`, "semi;colon", "#{pid}", "dollar $HOME", `"dq"`, "ünï 中文", "a ; kill-server", "x'y'z"}
	for _, n := range names {
		script := filepath.Join(t.TempDir(), "cmd")
		os.WriteFile(script, []byte("rename-session -t base "+Arg(n)+"\n"), 0o600)
		if _, err := s.Run(context.Background(), "source-file", script); err != nil {
			t.Fatalf("%q: %v", n, err)
		}
		out, _ := s.Run(context.Background(), "list-sessions", "-F", "#{session_id}\t#{session_name}")
		id, got, _ := strings.Cut(strings.TrimSuffix(out, "\n"), "\t")
		s.Run(context.Background(), "rename-session", "-t", id, "base")
		s.Run(context.Background(), "rename-session", "-t", id, Literal(n))
		out, _ = s.Run(context.Background(), "list-sessions", "-F", "#{session_name}")
		if want := strings.TrimSuffix(out, "\n"); got != want {
			t.Fatalf("rename to %q gave %q, tmux itself makes %q", n, got, want)
		}
		s.Run(context.Background(), "rename-session", "-t", id, "base")
	}
}

func TestControlFIFOAndNotes(t *testing.T) {
	s := testServer(t)
	c, err := Attach(s, "base")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Name() == "" {
		t.Fatal("no client name")
	}
	// Many concurrent commands, each must get its own answer.
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.Do(fmt.Sprintf("display-message -p 'n=%d'", i))
			if err != nil {
				errs <- err
				return
			}
			if r.Text() != fmt.Sprintf("n=%d", i) {
				errs <- fmt.Errorf("command %d got %q", i, r.Text())
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	r, err := c.Do("kill-window -t nosuch")
	if err != nil || !r.Err {
		t.Fatalf("a failing command must be marked: %+v %v", r, err)
	}
	// A notification for a new session.
	if _, err := s.Run(context.Background(), "new-session", "-d", "-s", "other"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case n := <-c.Notes():
			if n.Name == "sessions-changed" {
				return
			}
		case <-deadline:
			t.Fatal("no sessions-changed notification")
		}
	}
}

func TestControlCloseDetachesOnlyItself(t *testing.T) {
	s := testServer(t)
	c, err := Attach(s, "base")
	if err != nil {
		t.Fatal(err)
	}
	other, err := Attach(s, "base")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	c.Close()
	out, _ := s.Run(context.Background(), "list-clients", "-F", "#{client_name}")
	if !strings.Contains(out, other.Name()) || strings.Contains(out, c.Name()+"\n") && c.Name() != other.Name() {
		t.Fatalf("clients after close: %q (closed %s, kept %s)", out, c.Name(), other.Name())
	}
	if _, err := c.Do("display-message -p x"); err != ErrClosed {
		t.Fatalf("Do after Close: %v", err)
	}
}

func TestControlEndsWithServer(t *testing.T) {
	s := testServer(t)
	c, err := Attach(s, "base")
	if err != nil {
		t.Fatal(err)
	}
	s.Run(context.Background(), "kill-session", "-t", "base")
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("control client outlived its server")
	}
}

func TestResolvePassesShims(t *testing.T) {
	dir := t.TempDir()
	shims := filepath.Join(dir, "mise", "shims")
	real := filepath.Join(dir, "real")
	os.MkdirAll(shims, 0o755)
	os.MkdirAll(real, 0o755)
	// A shim whose manager cannot say: passed over for the next match.
	os.WriteFile(filepath.Join(shims, "faketool"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	os.WriteFile(filepath.Join(real, "faketool"), []byte("#!/bin/sh\n"), 0o755)
	// A stand-in manager that knows nothing.
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "mise"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	t.Setenv("PATH", shims+":"+real+":"+bin)
	if got := Resolve("faketool"); got != filepath.Join(real, "faketool") {
		t.Fatalf("Resolve = %q", got)
	}
	// A manager that names the binary.
	os.WriteFile(filepath.Join(bin, "mise"), []byte("#!/bin/sh\necho "+filepath.Join(real, "faketool")+"\n"), 0o755)
	t.Setenv("PATH", shims+":"+bin)
	if got := Resolve("faketool"); got != filepath.Join(real, "faketool") {
		t.Fatalf("Resolve through the manager = %q", got)
	}
	if _, err := exec.LookPath("tmux"); err == nil && !filepath.IsAbs(Resolve("tmux")) {
		t.Fatal("tmux resolves to an absolute path")
	}
}

func TestControlDoMany(t *testing.T) {
	s := testServer(t)
	c, err := Attach(s, "base")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	before := c.Bytes()
	reps, err := c.DoMany([]string{"display-message -p one", "show -gv @missing", "display-message -p three"}, 5*time.Second)
	if err != nil || len(reps) != 3 {
		t.Fatalf("%v %d replies", err, len(reps))
	}
	if reps[0].Text() != "one" || !reps[1].Err || reps[2].Text() != "three" {
		t.Fatalf("replies out of order: %+v", reps)
	}
	if c.Bytes() <= before {
		t.Fatal("bytes read not counted")
	}
}
