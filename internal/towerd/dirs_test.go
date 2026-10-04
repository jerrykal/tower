package towerd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/dirs"
	"github.com/jerrykal/tower/internal/proto"
)

// dirsHome gives the daemons of a test a home directory of their own, a
// zoxide that lists list, a mount table with <home>/nfs on NFS, and git
// without the user's configuration.
func dirsHome(t *testing.T, list ...string) string {
	t.Helper()
	if dirs.FindGit() == "" {
		t.Skip("no git")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	bin := filepath.Join(home, ".bin")
	os.MkdirAll(bin, 0o755)
	var b strings.Builder
	b.WriteString("#!/bin/sh\ncat <<'EOF'\n")
	for _, p := range list {
		b.WriteString(strings.ReplaceAll(p, "~", home) + "\n")
	}
	b.WriteString("EOF\n")
	os.WriteFile(filepath.Join(bin, "zoxide"), []byte(b.String()), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("TOWER_DIRS", "1")
	mounts := filepath.Join(home, ".mounts")
	os.WriteFile(mounts, []byte("/dev/disk1 / apfs rw 0 0\nserver:/x "+home+"/nfs nfs rw 0 0\n"), 0o644)
	t.Setenv("TOWER_TEST_MOUNTS", mounts)
	t.Setenv("TOWER_LOOK_EVERY", "1ms")
	return home
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(dirs.FindGit(), args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func repo(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	gitIn(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a"), []byte("a\n"), 0o644)
	gitIn(t, dir, "add", "a")
	gitIn(t, dir, "commit", "-q", "-m", "a")
}

func hostIn(v proto.View, name string) *proto.Host {
	for i := range v.Hosts {
		if v.Hosts[i].Name == name {
			return &v.Hosts[i]
		}
	}
	return nil
}

func sessionIn(v proto.View, host, name string) *proto.Session {
	if h := hostIn(v, host); h != nil {
		for i := range h.Sessions {
			if h.Sessions[i].Name == name {
				return &h.Sessions[i]
			}
		}
	}
	return nil
}

// A remote's sessions carry their git state and its zoxide directories
// reach the home's view, and from there the remote's own dashboards.
func TestGitAndDirsReachTheView(t *testing.T) {
	w := newWorld(t)
	home := dirsHome(t, "~/src/tool", "~/notes", "~/nfs/share", "~/src/proj", "~/gone")
	repo(t, filepath.Join(home, "src", "proj"))
	repo(t, filepath.Join(home, "src", "tool"))
	os.MkdirAll(filepath.Join(home, "notes"), 0o755)
	wt := filepath.Join(home, "src", "proj.fix")
	gitIn(t, filepath.Join(home, "src", "proj"), "worktree", "add", "-q", "-b", "fix", wt)

	a := w.node("A", "", "alpha")
	b := w.node("B", "", "bravo")
	b.tmux("new-session", "-d", "-s", "proj", "-c", filepath.Join(home, "src", "proj"))
	b.tmux("new-session", "-d", "-s", "fix", "-c", wt)
	a.hosts(b.remote())
	a.start(false)
	w.eventually(5*time.Second, "B up", func() bool { return a.link("B").Status == proto.StatusUp })

	w.eventually(5*time.Second, "B's git state in the home's view", func() bool {
		v := a.d.view(proto.ViewArgs{}).View
		p, f := sessionIn(v, "B", "proj"), sessionIn(v, "B", "fix")
		return p != nil && p.Git != nil && f != nil && f.Git != nil
	})
	v := a.d.view(proto.ViewArgs{}).View
	if g := *sessionIn(v, "B", "proj").Git; g != (proto.Git{Branch: "main"}) {
		t.Fatalf("B:proj %+v", g)
	}
	if g := *sessionIn(v, "B", "fix").Git; g != (proto.Git{Branch: "fix", Repo: "proj"}) {
		t.Fatalf("B:fix %+v", g)
	}

	// B's dirs: the session's directory left out, a gone one too, the
	// one on NFS listed unchecked.
	start := time.Now()
	for time.Since(start) < 5*time.Second {
		hb := hostIn(a.d.view(proto.ViewArgs{}).View, "B")
		if hb != nil && len(hb.Dirs) == 3 && hb.Dirs[0].Git != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := hostIn(a.d.view(proto.ViewArgs{}).View, "B").Dirs
	want := []proto.Dir{{Path: "~/src/tool", Root: true, Git: &proto.Git{Branch: "main"}}, {Path: "~/notes"}, {Path: "~/nfs/share", Net: true}}
	if len(got) != 3 || got[0].Path != want[0].Path || *got[0].Git != *want[0].Git || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("B's dirs %+v\n%s", got, b.log.String())
	}

	// A change in the repo shows once a dashboard looks, on any host: a
	// dashboard on B asks the home, which asks B.
	os.WriteFile(filepath.Join(wt, "b"), []byte("b"), 0o644)
	start = time.Now()
	w.eventually(5*time.Second, "B:fix dirty in the home's view", func() bool {
		b.d.view(proto.ViewArgs{Look: true}) // a dashboard on B reading
		s := sessionIn(a.d.view(proto.ViewArgs{}).View, "B", "fix")
		return s != nil && s.Git != nil && s.Git.Dirty
	})
	t.Logf("a dirty tree on B in the home's view %v after a look", time.Since(start).Round(time.Millisecond))
	// The loop's own reads (no Look) follow every tmux change and cost no
	// refresh.
	b.d.mu.Lock()
	b.d.lookAt = time.Time{}
	b.d.mu.Unlock()
	b.d.view(proto.ViewArgs{Loop: "L"})
	b.d.mu.Lock()
	looked := !b.d.lookAt.IsZero()
	b.d.mu.Unlock()
	if looked {
		t.Fatal("a view without Look started a look")
	}
	// And B's own dashboards read it from the home's view.
	w.eventually(2*time.Second, "B:fix dirty on B", func() bool {
		s := sessionIn(b.d.view(proto.ViewArgs{}).View, "B", "fix")
		return s != nil && s.Git != nil && s.Git.Dirty
	})

	// The session closes: its directory is a dir with no session again.
	b.tmux("kill-session", "-t", "proj")
	w.eventually(5*time.Second, "~/src/proj among B's dirs", func() bool {
		hb := hostIn(a.d.view(proto.ViewArgs{}).View, "B")
		return hb != nil && slices.ContainsFunc(hb.Dirs, func(d proto.Dir) bool { return d.Path == "~/src/proj" && d.Root })
	})
}

// panes lists a session's or a window's panes; capture answers with the
// window's panes too; both through the home from another host.
func TestPanesAndCapture(t *testing.T) {
	w := newWorld(t)
	home := dirsHome(t)
	a := w.node("A", "", "alpha")
	b := w.node("B", "", "bravo")
	b.tmux("split-window", "-h", "-t", "bravo:", "-c", home)
	b.tmux("new-window", "-d", "-t", "bravo:")
	a.hosts(b.remote())
	a.start(false)
	w.eventually(5*time.Second, "B up", func() bool { return a.link("B").Status == proto.StatusUp })
	ref := a.ref(b, "bravo")
	ctx := context.Background()

	ack := a.d.act(ctx, &proto.Request{Op: proto.OpPanes, Target: ref, Kind: proto.KindSession})
	if !ack.OK || len(ack.Panes) != 3 {
		t.Fatalf("a session's panes: %+v", ack)
	}
	wins := map[string]int{}
	for _, p := range ack.Panes {
		wins[p.Window]++
		if !strings.HasPrefix(p.ID, "%") || p.Width <= 0 || p.Height <= 0 || p.Command == "" || p.Path == "" {
			t.Fatalf("a pane %+v", p)
		}
	}
	if len(wins) != 2 {
		t.Fatalf("panes in %d windows: %+v", len(wins), ack.Panes)
	}
	first := ack.Panes[0].Window
	if ack.Panes[1].Window != first || ack.Panes[1].Left <= ack.Panes[0].Left || ack.Panes[1].Path != "~" {
		t.Fatalf("the split: %+v", ack.Panes[:2])
	}

	win := ref
	win.Window = first
	ack = a.d.act(ctx, &proto.Request{Op: proto.OpPanes, Target: win, Kind: proto.KindWindow})
	if !ack.OK || len(ack.Panes) != 2 || ack.Panes[0].Active == ack.Panes[1].Active {
		t.Fatalf("a window's panes: %+v", ack)
	}

	b.tmux("send-keys", "-t", ack.Panes[0].ID, "echo hello-capture", "Enter")
	w.eventually(3*time.Second, "the capture", func() bool {
		ack = a.d.act(ctx, &proto.Request{Op: proto.OpCapture, Target: proto.Ref{Host: ref.Host, Inst: ref.Inst, Session: ref.Session, Pane: ack.Panes[0].ID}})
		return ack.OK && strings.Contains(ack.Text, "hello-capture")
	})
	if len(ack.Panes) != 2 || ack.Panes[0].Window != first {
		t.Fatalf("capture's panes: %+v", ack.Panes)
	}
	// A session's capture: its active window's panes.
	ack = a.d.act(ctx, &proto.Request{Op: proto.OpCapture, Target: ref})
	if !ack.OK || len(ack.Panes) != 2 {
		t.Fatalf("a session's capture: %+v", ack)
	}
	ack = a.d.act(ctx, &proto.Request{Op: proto.OpPanes, Target: proto.Ref{Host: ref.Host, Session: "$999"}})
	if ack.OK {
		t.Fatalf("panes of a session that does not exist: %+v", ack)
	}
}

// The ops a dashboard needs besides kill, rename and new: a new session
// in a directory, ~ taken on the target host; a new window in its
// session's directory; a grouped duplicate; a window renamed and killed.
func TestNewInDirDupAndWindows(t *testing.T) {
	w := newWorld(t)
	home := dirsHome(t)
	a := w.node("A", "", "alpha")
	b := w.node("B", "", "bravo")
	a.hosts(b.remote())
	a.start(false)
	w.eventually(5*time.Second, "B up", func() bool { return a.link("B").Status == proto.StatusUp })
	ctx := context.Background()
	hash := filepath.Join(home, "a#{b}")
	os.MkdirAll(filepath.Join(home, "src", "proj"), 0o755)
	os.MkdirAll(hash, 0o755)
	path := func(name string) string {
		return b.tmux("display-message", "-p", "-t", "="+name+":", "#{session_path}")
	}

	ack := a.d.act(ctx, &proto.Request{Op: proto.OpNew, Target: proto.Ref{Host: b.d.id}, Name: "proj", Dir: "~/src/proj"})
	if !ack.OK || path("proj") != filepath.Join(home, "src", "proj") {
		t.Fatalf("new in ~/src/proj: %+v, path %q", ack, path("proj"))
	}
	ack = a.d.act(ctx, &proto.Request{Op: proto.OpNew, Target: proto.Ref{Host: b.d.id}, Name: "hash", Dir: hash})
	if !ack.OK || path("hash") != hash {
		t.Fatalf("new in a directory with a #: %+v, path %q", ack, path("hash"))
	}
	ack = a.d.act(ctx, &proto.Request{Op: proto.OpNew, Target: proto.Ref{Host: b.d.id}, Name: "nowhere", Dir: "~/not/there"})
	if ack.OK || !strings.Contains(ack.Err, "no directory ~/not/there on B") {
		t.Fatalf("new in a directory that is gone: %+v", ack)
	}
	ack = a.d.act(ctx, &proto.Request{Op: proto.OpNew, Target: proto.Ref{Host: b.d.id}, Name: "home"})
	if !ack.OK || path("home") != home {
		t.Fatalf("new with no directory: %+v, path %q", ack, path("home"))
	}

	// A new window starts in its session's directory.
	proj := a.ref(b, "proj")
	ack = a.d.act(ctx, &proto.Request{Op: proto.OpNew, Kind: proto.KindWindow, Target: proj, Name: "edit"})
	if !ack.OK || ack.Ref == nil || ack.Ref.Window == "" || ack.Ref.Session != proj.Session || ack.Ref.Label != "edit" {
		t.Fatalf("new window: %+v (its ref must name its session, for the dashboard to select it)", ack.Ref)
	}
	win := ack.Ref.Window
	w.eventually(3*time.Second, "the window's shell in the session's directory", func() bool {
		return b.tmux("display-message", "-p", "-t", win, "#{pane_current_path}") == filepath.Join(home, "src", "proj")
	})
	// Rename and kill it.
	wref := proj
	wref.Window = win
	if ack = a.d.act(ctx, &proto.Request{Op: proto.OpRename, Kind: proto.KindWindow, Target: wref, Name: "#{x} -n"}); !ack.OK {
		t.Fatalf("rename a window: %+v", ack)
	}
	if got := b.tmux("display-message", "-p", "-t", win, "#{window_name}"); got != "#{x} -n" {
		t.Fatalf("the window's name %q", got)
	}
	if ack = a.d.act(ctx, &proto.Request{Op: proto.OpKill, Kind: proto.KindWindow, Target: wref}); !ack.OK {
		t.Fatalf("kill a window: %+v", ack)
	}
	if out := b.tmux("list-windows", "-t", "=proj:", "-F", "#{window_id}"); strings.Contains(out, win) {
		t.Fatalf("the window outlived its kill: %s", out)
	}

	// A grouped duplicate shares the windows; the home's view shows it at once.
	ack = a.d.act(ctx, &proto.Request{Op: proto.OpDup, Target: proj, Name: "proj 2"})
	if !ack.OK || ack.Ref == nil || ack.Ref.Label != "proj 2" {
		t.Fatalf("dup: %+v", ack)
	}
	s := sessionIn(a.d.view(proto.ViewArgs{}).View, "B", "proj 2")
	if s == nil || s.Group == "" {
		t.Fatalf("the duplicate in the home's view: %+v", s)
	}
	if g := b.tmux("display-message", "-p", "-t", "=proj:", "#{session_group}"); g != s.Group {
		t.Fatalf("groups %q and %q", g, s.Group)
	}
	b.tmux("new-window", "-d", "-t", "=proj:")
	if n := b.tmux("display-message", "-p", "-t", "=proj 2:", "#{session_windows}"); n != "2" {
		t.Fatalf("the duplicate has %s windows", n)
	}
}
