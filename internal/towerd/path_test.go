package towerd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

func globalPath(n *node) string {
	return strings.TrimPrefix(n.tmux("show-environment", "-g", "PATH"), "PATH=")
}

// A directory an earlier towerd recorded goes, even when this towerd's
// own is already in PATH (the user's): nothing then is recorded.
func TestPathStaleRecordGoes(t *testing.T) {
	w := newWorld(t)
	n := w.node("S", "", "s1")
	own := filepath.Dir(w.keep)
	stale := t.TempDir()
	base := globalPath(n)
	n.tmux("set-environment", "-g", "PATH", base+":"+own+":"+stale)
	config.WriteJSON(n.env.State("keys.json"), keysFile{Path: stale})
	d := n.start(false)
	want := base + ":" + own
	w.eventually(3*time.Second, "the stale directory gone", func() bool { return globalPath(n) == want })
	d.Stop("test")
	d.Wait()
	if got := globalPath(n); got != want {
		t.Fatalf("the user's directory after stop: %q, want %q", got, want)
	}
	var kf keysFile
	config.ReadJSON(n.env.State("keys.json"), &kf)
	if kf.Path != "" {
		t.Fatalf("recorded %q", kf.Path)
	}
}

// A towerd run from a directory others can write to (a build unpacked in
// /tmp) puts nothing on PATH.
func TestPathRefusesAnOpenDirectory(t *testing.T) {
	w := newWorld(t)
	n := w.node("O", "", "o1")
	open := t.TempDir()
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(open, "tower")
	src, err := os.Open(w.keep)
	if err != nil {
		t.Fatal(err)
	}
	dst, _ := os.OpenFile(bin, os.O_CREATE|os.O_WRONLY, 0o755)
	io.Copy(dst, src)
	src.Close()
	dst.Close()
	before := globalPath(n)
	d, err := Start(Options{Env: n.env, Version: "0.0.1-unit", Transport: &inproc{w: w}, Self: bin, LogTo: n.log, Name: n.name})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Stop("test"); d.Wait() }()
	w.eventually(3*time.Second, "keys bound", func() bool { return d.status(true).Detail.Watch.Keys != "" })
	if got := globalPath(n); got != before {
		t.Fatalf("PATH took %q: %q", open, got)
	}
	if pathFit("relative") || pathFit("/a:b") {
		t.Fatal("a relative directory, or one with a list separator, is not fit")
	}
}

// Served is a connected home of this towerd's build; a remote-only towerd
// served refuses a replacement asked by anything but a bridge, and keeps
// tower stop.
func TestServedRefusesATypedReplacement(t *testing.T) {
	d := &Daemon{version: "1.0.0", homes: map[string]*homeRec{
		"a": {name: "A", version: "0.9.0", conn: nil},
	}}
	if _, ok := d.servedLocked(); ok {
		t.Fatal("served by a home with no stream")
	}

	w := newWorld(t)
	a := w.node("A", "", "alpha")
	b := w.node("B", "", "bravo")
	a.hosts(b.remote())
	a.start(false)
	w.eventually(5*time.Second, "B up", func() bool { return a.link("B").Status == proto.StatusUp })
	if !b.d.status(false).Served {
		t.Fatal("B, connected to by a home of its build, is not served")
	}
	c := client.New(b.env)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Call(ctx, proto.CallStop, proto.StopArgs{IfOlderThan: "0.0.1-other"}, nil)
	if err == nil || !strings.Contains(err.Error(), "runs the build of home") {
		t.Fatalf("a typed replacement of B's towerd: %v", err)
	}
	c.Version = "0.0.1-other"
	if _, err := c.Ensure(ctx); err == nil || !strings.Contains(err.Error(), "runs a home's build") {
		t.Fatalf("Ensure of another build on B: %v", err)
	}
	select {
	case <-b.d.done:
		t.Fatal("B's towerd stopped")
	default:
	}
}
