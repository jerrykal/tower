package scenario

import (
	"encoding/json"
	"fmt"
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

// The data the Atlas dashboard shows besides tmux's listing: the git
// state of sessions, zoxide directories, panes; and the requests it
// makes besides kill, rename and new.

// git runs git in dir as a host would, without the user's configuration.
func (w *World) git(dir string, args ...string) {
	w.T.Helper()
	bin := dirs.FindGit()
	if bin == "" {
		w.T.Skip("no git")
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+w.UserHome, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		w.T.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// repo makes a repo with one commit on main under the world's home
// directory (rel is relative to it) and returns its path.
func (w *World) repo(rel string) string {
	w.T.Helper()
	dir := filepath.Join(w.UserHome, rel)
	os.MkdirAll(dir, 0o755)
	w.git(dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644)
	w.git(dir, "add", "README")
	w.git(dir, "commit", "-q", "-m", "first")
	return dir
}

// sessionOf is session name on host in v.
func sessionOf(v *proto.View, host, name string) *proto.Session {
	if hs := HostIn(v, host); hs != nil {
		for i := range hs.Sessions {
			if hs.Sessions[i].Name == name {
				return &hs.Sessions[i]
			}
		}
	}
	return nil
}

func gitOf(v *proto.View, host, name string) *proto.Git {
	if s := sessionOf(v, host, name); s != nil {
		return s.Git
	}
	return nil
}

// A01: the git state of a session on a remote reaches the home's view and
// another remote's: the branch, a linked worktree's repo, a detached
// commit; a tree made dirty shows once a dashboard opens, on any host.
func TestA01(t *testing.T) {
	w := NewWorld(t, "a01")
	w.Timing(map[string]string{"TOWER_LOOK_EVERY": "1000"})
	proj := w.repo("src/proj")
	wt := filepath.Join(w.UserHome, "src", "proj.fix")
	w.git(proj, "worktree", "add", "-q", "-b", "fix", wt)
	det := filepath.Join(w.UserHome, "src", "proj.det")
	w.git(proj, "worktree", "add", "-q", "--detach", det)
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"})
	c := w.Host("C", []string{"charlie"})
	b.MustTmux("new-session", "-d", "-s", "proj", "-c", proj)
	b.MustTmux("new-session", "-d", "-s", "fix", "-c", wt+"/")
	b.MustTmux("new-session", "-d", "-s", "det", "-c", det)
	w.Home(a, b.Remote(), c.Remote())
	w.WaitUp(a, "B", "C")

	start := time.Now()
	w.Eventually(5*time.Second, "B's git state in the home's view", func() bool {
		v := &a.View("").View
		return gitOf(v, "B", "proj") != nil && gitOf(v, "B", "fix") != nil && gitOf(v, "B", "det") != nil
	})
	t.Logf("B's git state in the home's view %v after B was up", time.Since(start).Round(time.Millisecond))
	v := &a.View("").View
	if g := *gitOf(v, "B", "proj"); g != (proto.Git{Branch: "main"}) {
		t.Fatalf("B:proj %+v", g)
	}
	if g := *gitOf(v, "B", "fix"); g != (proto.Git{Branch: "fix", Repo: "proj"}) {
		t.Fatalf("B:fix %+v", g)
	}
	if g := *gitOf(v, "B", "det"); len(g.Branch) != 7 || g.Repo != "proj" || g.Dirty {
		t.Fatalf("B:det %+v", g)
	}
	w.Eventually(3*time.Second, "B's git state on C", func() bool { return gitOf(&c.View("").View, "B", "fix") != nil })

	// Edits since the last refresh: a dashboard opening on C refreshes
	// B through the home.
	time.Sleep(2500 * time.Millisecond) // past the look's pacing and a repo's freshness
	os.WriteFile(filepath.Join(wt, "edit.txt"), []byte("x"), 0o644)
	w.git(proj, "checkout", "-q", "-b", "next")
	start = time.Now()
	var seen time.Duration
	for time.Since(start) < 5*time.Second {
		v := &c.View("").View // a dashboard's reads, as it opens and follows the view
		if g, p := gitOf(v, "B", "fix"), gitOf(v, "B", "proj"); g != nil && g.Dirty && p != nil && p.Branch == "next" {
			seen = time.Since(start)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if seen == 0 {
		t.Fatalf("B's changes not on C: %+v", HostIn(&c.View("").View, "B").Sessions)
	}
	t.Logf("a dirty tree and a new branch on B in C's dashboard %v after it opened", seen.Round(time.Millisecond))
	if seen > 2*time.Second {
		t.Fatalf("the refresh a dashboard asks for took %v", seen)
	}
}

// A02: zoxide directories reach every dashboard: most frecent first, ~
// for home, git roots with their branch and dirt, directories that are a
// session's left out, gone ones left out, a directory on a network mount
// listed unchecked; a new session in a dir takes it off the list in the
// rows read right after the answer.
func TestA02(t *testing.T) {
	w := NewWorld(t, "a02")
	proj := w.repo("src/proj")
	w.repo("src/lib")
	os.WriteFile(filepath.Join(w.UserHome, "src", "lib", "wip"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(w.UserHome, "notes"), 0o755)
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"},
		Zoxide("~/src/lib", "~/notes", "~/nfs/data/repo", "~/src/proj", "~/gone", "~"),
		// ~/nfs/data/repo does not exist: listed all the same, which only
		// a directory never stat'ed can be.
		MountTable("/dev/disk1 / apfs rw 0 0\nserver:/export ~/nfs nfs4 rw 0 0\n"))
	c := w.Host("C", []string{"charlie"})
	b.MustTmux("new-session", "-d", "-s", "proj", "-c", proj)
	w.Home(a, b.Remote(), c.Remote())
	w.WaitUp(a, "B", "C")

	want := []proto.Dir{
		{Path: "~/src/lib", Root: true, Git: &proto.Git{Branch: "main", Dirty: true}},
		{Path: "~/notes"},
		{Path: "~/nfs/data/repo", Net: true},
		{Path: "~"},
	}
	same := func(got []proto.Dir) bool {
		return slices.EqualFunc(got, want, func(x, y proto.Dir) bool {
			return x.Path == y.Path && x.Root == y.Root && x.Net == y.Net && (x.Git == nil) == (y.Git == nil) && (x.Git == nil || *x.Git == *y.Git)
		})
	}
	dirsOn := func(h *Host) []proto.Dir {
		if hs := HostIn(&h.View("").View, "B"); hs != nil {
			return hs.Dirs
		}
		return nil
	}
	for _, h := range []*Host{b, a, c} {
		w.Eventually(5*time.Second, "B's dirs on "+h.Name, func() bool { return same(dirsOn(h)) })
	}
	// A host whose zoxide lists nothing has no dirs.
	if hs := HostIn(&a.View("").View, "C"); hs == nil || len(hs.Dirs) != 0 {
		t.Fatalf("C's dirs: %+v", hs)
	}
	st := b.Status().Detail.Dirs
	t.Logf("B: %d dirs, %d repos, zoxide %s", st.Dirs, st.Repos, st.Zoxide)

	// A new session in a dir, from C's dashboard: B's dirs on C lack it
	// as soon as the answer is in.
	ack, el := c.Act("", proto.Request{Op: proto.OpNew, Target: w.Ref(a, "B", "bravo"), Name: "notes", Dir: "~/notes"}, 0)
	if !ack.OK {
		t.Fatalf("new in ~/notes: %+v", ack)
	}
	if got := dirsOn(c); slices.ContainsFunc(got, func(d proto.Dir) bool { return d.Path == "~/notes" }) {
		t.Fatalf("~/notes still among B's dirs on C right after the answer (%v): %+v", el, got)
	}
	if p := sessionOf(&c.View("").View, "B", "notes"); p == nil || p.Path != filepath.Join(w.UserHome, "notes") {
		t.Fatalf("B:notes on C: %+v", p)
	}
	t.Logf("new in ~/notes on B from C: %v", el.Round(time.Millisecond))
}

// A03: the panes of a session on one remote, and a capture with its
// window's layout, for a dashboard on another remote.
func TestA03(t *testing.T) {
	w := NewWorld(t, "a03")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"})
	c := w.Host("C", []string{"charlie"})
	b.MustTmux("split-window", "-h", "-t", "=bravo:", "-c", w.UserHome)
	b.MustTmux("split-window", "-v", "-t", "=bravo:")
	b.MustTmux("new-window", "-d", "-t", "=bravo:")
	w.Home(a, b.Remote(), c.Remote())
	w.WaitUp(a, "B", "C")
	w.Eventually(3*time.Second, "B on C", func() bool { return HasSession(&c.View("").View, "B", "bravo") })
	ref := w.Ref(a, "B", "bravo")

	ack, el := c.Act("", proto.Request{Op: proto.OpPanes, Target: ref, Kind: proto.KindSession}, 0)
	if !ack.OK || len(ack.Panes) != 4 {
		t.Fatalf("B:bravo's panes from C: %+v", ack)
	}
	t.Logf("panes of B:bravo from C: %v", el.Round(time.Microsecond))
	win := ack.Panes[0].Window
	var layout []proto.Pane
	for _, p := range ack.Panes {
		if p.Window == win {
			layout = append(layout, p)
		}
	}
	if len(layout) != 3 {
		t.Fatalf("the first window's panes: %+v", ack.Panes)
	}
	// Left of the split, the full height; right, two halves.
	l, r1, r2 := layout[0], layout[1], layout[2]
	if l.Left != 0 || l.Top != 0 || r1.Left <= l.Width || r1.Left != r2.Left || r2.Top <= r1.Top || l.Height < r1.Height+r2.Height ||
		r1.Path != "~" || l.Command == "" || !slices.ContainsFunc(layout, func(p proto.Pane) bool { return p.Active }) {
		t.Fatalf("the layout: %+v", layout)
	}

	b.MustTmux("send-keys", "-t", l.ID, "echo LAYOUT-$((6*7))", "Enter")
	var text string
	w.Eventually(3*time.Second, "the capture", func() bool {
		ack, el = c.Act("", proto.Request{Op: proto.OpCapture, Target: proto.Ref{Host: ref.Host, Inst: ref.Inst, Session: ref.Session, Window: win, Pane: l.ID}}, 0)
		text = ack.Text
		return ack.OK && strings.Contains(text, "LAYOUT-42")
	})
	if len(ack.Panes) != 3 || ack.Panes[0].ID != l.ID || ack.Panes[2].ID != r2.ID {
		t.Fatalf("capture's layout: %+v", ack.Panes)
	}
	t.Logf("capture with its layout from C: %v", el.Round(time.Microsecond))
	// A window's panes alone.
	wref := ref
	wref.Window = win
	if ack, _ := c.Act("", proto.Request{Op: proto.OpPanes, Target: wref, Kind: proto.KindWindow}, 0); !ack.OK || len(ack.Panes) != 3 {
		t.Fatalf("a window's panes: %+v", ack)
	}
}

// A04: through act from a dashboard on another remote: a grouped
// duplicate, a new session in a directory (~ taken on the target host, a
// directory that is gone refused), a new window in its session's
// directory, a window renamed and killed; each in the rows read right
// after its answer.
func TestA04(t *testing.T) {
	w := NewWorld(t, "a04")
	proj := w.repo("src/proj")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"})
	c := w.Host("C", []string{"charlie"})
	w.Home(a, b.Remote(), c.Remote())
	w.WaitUp(a, "B", "C")
	w.Eventually(3*time.Second, "B on C", func() bool { return HasSession(&c.View("").View, "B", "bravo") })
	onB := proto.Ref{Host: w.Ref(a, "B", "bravo").Host}
	act := func(req proto.Request) *proto.Ack {
		t.Helper()
		ack, el := c.Act("", req, 0)
		if !ack.OK {
			t.Fatalf("%s %s from C: %+v", req.Op, req.Kind, ack)
		}
		t.Logf("%s %s on B from C: %v", req.Op, req.Kind, el.Round(time.Millisecond))
		return ack
	}

	act(proto.Request{Op: proto.OpNew, Target: onB, Name: "proj", Dir: "~/src/proj"})
	s := sessionOf(&c.View("").View, "B", "proj")
	if s == nil || s.Path != proj {
		t.Fatalf("B:proj on C: %+v", s)
	}
	if ack, _ := c.Act("", proto.Request{Op: proto.OpNew, Target: onB, Name: "x", Dir: "~/no/such/dir"}, 0); ack.OK || !strings.Contains(ack.Err, "no directory ~/no/such/dir on B") {
		t.Fatalf("new in a directory that is gone: %+v", ack)
	}

	ack := act(proto.Request{Op: proto.OpDup, Target: w.Ref(a, "B", "proj"), Name: "proj 2"})
	v := &c.View("").View
	d, p := sessionOf(v, "B", "proj 2"), sessionOf(v, "B", "proj")
	if d == nil || d.ID != ack.Ref.Session || d.Group == "" || d.Group != p.Group || len(d.Windows) != len(p.Windows) {
		t.Fatalf("the duplicate on C: %+v (of %+v)", d, p)
	}

	ack = act(proto.Request{Op: proto.OpNew, Kind: proto.KindWindow, Target: w.Ref(a, "B", "proj"), Name: "logs"})
	win := ack.Ref.Window
	if !slices.ContainsFunc(sessionOf(&c.View("").View, "B", "proj").Windows, func(x proto.Window) bool { return x.ID == win && x.Name == "logs" }) {
		t.Fatal("the new window is not on C after the answer")
	}
	w.Eventually(3*time.Second, "the window's shell in the session's directory", func() bool {
		out, _ := b.Tmux("display-message", "-p", "-t", win, "#{pane_current_path}")
		real, _ := filepath.EvalSymlinks(proj)
		return strings.TrimSpace(out) == proj || strings.TrimSpace(out) == real
	})
	wref := w.Ref(a, "B", "proj")
	wref.Window = win
	act(proto.Request{Op: proto.OpRename, Kind: proto.KindWindow, Target: wref, Name: "logs #1"})
	if !slices.ContainsFunc(sessionOf(&c.View("").View, "B", "proj").Windows, func(x proto.Window) bool { return x.Name == "logs #1" }) {
		t.Fatal("the renamed window is not on C after the answer")
	}
	act(proto.Request{Op: proto.OpKill, Kind: proto.KindWindow, Target: wref})
	if slices.ContainsFunc(sessionOf(&c.View("").View, "B", "proj").Windows, func(x proto.Window) bool { return x.ID == win }) {
		t.Fatal("the killed window is still on C after the answer")
	}
}

// A05: what the git state and zoxide directories cost on a host with 40
// sessions, each in a repo of its own, and a 500-entry zoxide database
// (one in five a repo): the size of its state, and the CPU a periodic
// refresh and a dashboard's look take there, git's runs included.
func TestA05(t *testing.T) {
	w := NewWorld(t, "a05")
	var list []string
	for i := range 500 {
		rel := fmt.Sprintf("src/group-%d/some-directory-%03d", i%10, i)
		if i%5 == 0 {
			w.repo(rel)
		} else {
			os.MkdirAll(filepath.Join(w.UserHome, rel), 0o755)
		}
		list = append(list, "~/"+rel)
	}
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, Zoxide(list...), Env("TOWER_DIRS_EVERY", "3000"), Env("TOWER_LOOK_EVERY", "100"))
	var script strings.Builder
	for i := range 40 {
		dir := w.repo(fmt.Sprintf("work/project-%02d", i))
		fmt.Fprintf(&script, "new-session -d -s project-%02d -c '%s'\n", i, dir)
		fmt.Fprintf(&script, "new-window -d -t project-%02d: -c '%s'\n", i, dir)
	}
	src := filepath.Join(w.Dir, "a05.tmux")
	os.WriteFile(src, []byte(script.String()), 0o600)
	b.MustTmux("source-file", src)
	w.Home(a, b.Remote())
	w.WaitUp(a, "B")
	w.Eventually(10*time.Second, "B's dirs and git state at the home", func() bool {
		hs := HostIn(&a.View("").View, "B")
		return hs != nil && len(hs.Dirs) == 200 && hs.Dirs[0].Git != nil && gitOf(&a.View("").View, "B", "project-39") != nil
	})
	hs := HostIn(&a.View("").View, "B")
	full, _ := json.Marshal(proto.State{Sessions: hs.Sessions, Dirs: hs.Dirs})
	bare := slices.Clone(hs.Sessions)
	for i := range bare {
		bare[i].Git = nil
	}
	plain, _ := json.Marshal(proto.State{Sessions: bare})
	t.Logf("B's state: %d bytes; %d without git state and dirs (%d sessions, %d dirs)", len(full), len(plain), len(hs.Sessions), len(hs.Dirs))

	cost := func(what string, trigger func(), done func(proto.DirsStatus, proto.DirsStatus) bool) {
		before := b.Status().Detail
		start := time.Now()
		trigger()
		var after *proto.Detail
		w.Eventually(10*time.Second, what, func() bool {
			after = b.Status().Detail
			return done(before.Dirs, after.Dirs)
		})
		t.Logf("%s: %v CPU on B (towerd, git and zoxide), %v wall", what, time.Duration(after.CPUMs-before.CPUMs)*time.Millisecond, time.Since(start).Round(time.Millisecond))
	}
	// The first periodic refresh asks about every repo zoxide listed (new
	// ones); the next ones only about the sessions'.
	steady := func(d proto.DirsStatus) bool { return d.TimerRun > 0 && d.TimerRun < 100 }
	w.Eventually(10*time.Second, "a periodic refresh of the sessions' repos", func() bool { return steady(b.Status().Detail.Dirs) })
	cost("a periodic refresh (one period of 3s)", func() {}, func(x, y proto.DirsStatus) bool { return y.Rounds > x.Rounds && steady(y) })
	time.Sleep(2500 * time.Millisecond) // past a repo's freshness
	cost("a look", func() { b.View("") }, func(x, y proto.DirsStatus) bool { return y.Rounds > x.Rounds && y.LookRun > 0 })
	st := b.Status().Detail.Dirs
	t.Logf("B: %d dirs, %d repos; periodic refresh %dms (%d git runs), look %dms (%d git runs)", st.Dirs, st.Repos, st.TimerMs, st.TimerRun, st.LookMs, st.LookRun)
	if st.TimerRun < 40 || st.LookRun < 140 {
		t.Fatalf("git runs: %+v", st)
	}
}
