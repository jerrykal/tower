package dirs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

// gitBin is the git the tests make repos with and the refresher runs.
func gitBin(t testing.TB) string {
	t.Helper()
	p := FindGit()
	if p == "" {
		t.Skip("no git")
	}
	return p
}

// isolateGit keeps the user's own git configuration out of the tests.
func isolateGit(t testing.TB) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func run(t testing.TB, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo makes a repo with one commit on branch main.
func newRepo(t testing.TB, dir string) {
	t.Helper()
	git := gitBin(t)
	os.MkdirAll(dir, 0o755)
	run(t, dir, git, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	run(t, dir, git, "add", "a.txt")
	run(t, dir, git, "commit", "-q", "-m", "one")
}

// tempDir is a temporary directory with symlinks resolved (macOS's /var
// is /private/var), so paths compare as git and the mount table spell them.
func tempDir(t testing.TB) string {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseMounts(t *testing.T) {
	m, err := ParseMounts(strings.NewReader(`/dev/sda1 / ext4 rw 0 0
server:/export /mnt/nfs nfs4 rw 0 0
//nas/share /mnt/my\040share cifs rw 0 0
user@host:/ /home/u/remote fuse.sshfs rw 0 0
lxcfs /var/lib/lxcfs fuse.lxcfs rw 0 0
/dev/sdb1 /mnt/nfs/local ext4 rw 0 0
/dev/sdc1 /media/usb fuseblk rw 0 0
`))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"/":                     false,
		"/home/u":               false,
		"/mnt/nfs":              true,
		"/mnt/nfs/x/y":          true,
		"/mnt/nfsother":         false,
		"/mnt/nfs/local/repo":   false, // a local mount inside a network one
		"/mnt/my share/a":       true,
		"/home/u/remote/src":    true,
		"/var/lib/lxcfs/proc":   false,
		"/media/usb/photos":     false,
		"/home/u/remote-thing":  false,
		"/mnt/my share":         true,
		"/mnt/my share2/nested": false,
	} {
		if got := m.Net(path); got != want {
			t.Errorf("Net(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestSystemMounts(t *testing.T) {
	m, err := SystemMounts()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.On("/"); !ok {
		t.Fatalf("no mount holds /: %v", m)
	}
}

func TestFindRepo(t *testing.T) {
	isolateGit(t)
	git := gitBin(t)
	root := tempDir(t)
	main := filepath.Join(root, "proj")
	newRepo(t, main)
	os.MkdirAll(filepath.Join(main, "src", "deep"), 0o755)
	wt := filepath.Join(root, "proj.feature")
	run(t, main, git, "worktree", "add", "-q", "-b", "feature", wt)
	plain := filepath.Join(root, "plain")
	os.MkdirAll(plain, 0o755)
	// A submodule's .git file names a gitdir with no commondir.
	sub := filepath.Join(root, "sub")
	os.MkdirAll(filepath.Join(root, "modules", "sub"), 0o755)
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../modules/sub\n"), 0o644)

	cases := []struct {
		dir  string
		want Repo
		ok   bool
	}{
		{main, Repo{Root: main}, true},
		{filepath.Join(main, "src", "deep"), Repo{Root: main}, true},
		{wt, Repo{Root: wt, Main: "proj"}, true},
		{sub, Repo{Root: sub}, true},
		{plain, Repo{}, false},
	}
	for _, c := range cases {
		got, ok := FindRepo(c.dir, nil)
		if ok != c.ok || got != c.want {
			t.Errorf("FindRepo(%s) = %+v %v, want %+v %v", c.dir, got, ok, c.want, c.ok)
		}
	}
	// The walk never looks at a directory on a network mount.
	net := Mounts{{Dir: root, Type: "nfs"}, {Dir: "/", Type: "apfs"}}
	if r, ok := FindRepo(filepath.Join(main, "src"), net); ok {
		t.Errorf("found %+v on a network mount", r)
	}
}

func TestStatus(t *testing.T) {
	isolateGit(t)
	git := gitBin(t)
	dir := filepath.Join(tempDir(t), "r")
	newRepo(t, dir)
	ctx := context.Background()
	st := func() proto.Git {
		t.Helper()
		g, err := Status(ctx, git, dir, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return *g
	}
	if g := st(); g != (proto.Git{Branch: "main"}) {
		t.Fatalf("clean: %+v", g)
	}
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644)
	if g := st(); !g.Dirty {
		t.Fatalf("untracked file: %+v", g)
	}
	os.Remove(filepath.Join(dir, "new.txt"))
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644)
	if g := st(); !g.Dirty {
		t.Fatalf("modified file: %+v", g)
	}
	run(t, dir, git, "checkout", "-q", "--", "a.txt")
	commit := run(t, dir, git, "rev-parse", "HEAD")
	run(t, dir, git, "checkout", "-q", "--detach")
	if g := st(); g.Branch != commit[:7] || g.Dirty {
		t.Fatalf("detached: %+v, want %s", g, commit[:7])
	}
	// An unborn branch has a name and no commit.
	fresh := filepath.Join(tempDir(t), "fresh")
	os.MkdirAll(fresh, 0o755)
	run(t, fresh, git, "init", "-q", "-b", "trunk")
	if g, err := Status(ctx, git, fresh, 5*time.Second); err != nil || g.Branch != "trunk" {
		t.Fatalf("unborn: %+v %v", g, err)
	}
	if _, err := Status(ctx, git, filepath.Join(tempDir(t)), 5*time.Second); err == nil {
		t.Fatal("status outside a repo did not fail")
	}
}

func TestShortAndExpand(t *testing.T) {
	home := "/home/u"
	for in, want := range map[string]string{"/home/u": "~", "/home/u/src": "~/src", "/home/user": "/home/user", "/tmp": "/tmp"} {
		if got := Short(in, home); got != want {
			t.Errorf("Short(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"~": "/home/u", "~/src/": "/home/u/src", "/a/../b": "/b", "rel": "/home/u/rel", "~other/x": "/home/u/~other/x"} {
		if got := Expand(in, home); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeZoxide writes a zoxide that lists dirs and counts its runs.
func fakeZoxide(t testing.TB, dir string, list []string) string {
	t.Helper()
	data := filepath.Join(dir, "zoxide.list")
	os.WriteFile(data, []byte(strings.Join(list, "\n")+"\n"), 0o644)
	p := filepath.Join(dir, "zoxide")
	script := fmt.Sprintf("#!/bin/sh\necho run >> %s.runs\n[ \"$*\" = 'query --list --all' ] || exit 2\nexec cat %s\n", data, data)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQuery(t *testing.T) {
	dir := tempDir(t)
	z := fakeZoxide(t, dir, []string{"/a/b", "relative/skipped", "/c/"})
	got, err := Query(context.Background(), z, time.Second)
	if err != nil || !slices.Equal(got, []string{"/a/b", "/c"}) {
		t.Fatalf("Query: %v %v", got, err)
	}
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte("#!/bin/sh\necho 'zoxide: no match found' >&2\nexit 1\n"), 0o755)
	if got, err := Query(context.Background(), empty, time.Second); err != nil || got != nil {
		t.Fatalf("an empty database: %v %v", got, err)
	}
}

// world is a home directory with repos, a fake zoxide and a mount table.
type world struct {
	t      *testing.T
	home   string
	zox    string
	mounts Mounts
	r      *Refresher
	calls  atomic.Int32
}

func newDirsWorld(t *testing.T) *world {
	isolateGit(t)
	gitBin(t)
	w := &world{t: t, home: tempDir(t)}
	w.mounts = Mounts{{Dir: filepath.Join(w.home, "nfs"), Type: "nfs"}, {Dir: "/", Type: "local"}}
	return w
}

func (w *world) start(list []string, o Options) {
	w.zox = fakeZoxide(w.t, w.t.TempDir(), list)
	o.Git = FindGit
	o.Zoxide = func() string { return w.zox }
	o.Home = w.home
	o.Mounts = func() (Mounts, error) { return w.mounts, nil }
	o.OnChange = func() { w.calls.Add(1) }
	o.Log = w.t.Logf
	w.r = New(o)
	w.r.Start()
	w.t.Cleanup(w.r.Stop)
}

func (w *world) eventually(what string, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.t.Fatalf("%s: not within 5s", what)
}

func TestRefresherSessionsAndDirs(t *testing.T) {
	w := newDirsWorld(t)
	git := gitBin(t)
	proj := filepath.Join(w.home, "src", "proj")
	newRepo(t, proj)
	wt := filepath.Join(w.home, "src", "proj.fix")
	run(t, proj, git, "worktree", "add", "-q", "-b", "fix", wt)
	other := filepath.Join(w.home, "src", "other")
	newRepo(t, other)
	plain := filepath.Join(w.home, "notes")
	os.MkdirAll(plain, 0o755)
	// On the network mount: listed, never looked at (it does not even
	// exist, which a stat would have found).
	remote := filepath.Join(w.home, "nfs", "share", "repo")
	gone := filepath.Join(w.home, "deleted")

	w.start([]string{other, plain, remote, gone, proj, wt, w.home}, Options{Fresh: time.Nanosecond})
	w.r.Sessions([]string{"~/src/proj", wt + "/", filepath.Join(proj, "src")})

	w.eventually("the sessions' git state", func() bool {
		g := w.r.Git(wt)
		return w.r.Git("~/src/proj") != nil && g != nil && g.Repo == "proj"
	})
	if g := *w.r.Git(proj); g != (proto.Git{Branch: "main"}) {
		t.Fatalf("proj: %+v", g)
	}
	if g := *w.r.Git(wt); g != (proto.Git{Branch: "fix", Repo: "proj"}) {
		t.Fatalf("the linked worktree: %+v", g)
	}
	// A directory inside a repo has the repo's state.
	if g := w.r.Git(filepath.Join(proj, "src")); g == nil || g.Branch != "main" {
		t.Fatalf("a subdirectory: %+v", g)
	}
	if g := w.r.Git(plain); g != nil {
		t.Fatalf("a directory with no session: %+v", g)
	}

	w.eventually("the dirs' git state", func() bool {
		d := w.r.Dirs()
		return len(d) > 0 && d[0].Git != nil
	})
	got := w.r.Dirs()
	want := []proto.Dir{
		{Path: "~/src/other", Root: true, Git: &proto.Git{Branch: "main"}},
		{Path: "~/notes"},
		{Path: "~/nfs/share/repo", Net: true},
		{Path: "~"},
	}
	if !sameDirs(got, want) {
		t.Fatalf("dirs:\n got %s\nwant %s", dirsString(got), dirsString(want))
	}

	// A session closes: its directory is a dir with no session again.
	w.r.Sessions([]string{wt})
	got = w.r.Dirs()
	if len(got) != 5 || got[3].Path != "~/src/proj" || !got[3].Root {
		t.Fatalf("after proj's session went: %s", dirsString(got))
	}

	// A change in a repo shows after a look.
	calls := w.calls.Load()
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("b"), 0o644)
	w.r.Look()
	w.eventually("the worktree dirty", func() bool { g := w.r.Git(wt); return g != nil && g.Dirty })
	if w.calls.Load() == calls {
		t.Fatal("no OnChange for the change")
	}
	st := w.r.Stats()
	if st.Roots != 3 || st.Dirs != 6 || st.Zoxide != w.zox {
		t.Fatalf("stats %+v", st)
	}
}

func TestRefresherCapsDirs(t *testing.T) {
	w := newDirsWorld(t)
	var list []string
	for i := range 30 {
		d := filepath.Join(w.home, fmt.Sprintf("d%02d", i))
		if i%3 == 0 {
			newRepo(t, d)
		} else {
			os.MkdirAll(d, 0o755)
		}
		list = append(list, d)
	}
	w.start(list, Options{MaxRoots: 4, MaxPlain: 5})
	w.eventually("the dirs", func() bool { return len(w.r.Dirs()) == 9 })
	var roots, plain []string
	for _, d := range w.r.Dirs() {
		if d.Root {
			roots = append(roots, d.Path)
		} else {
			plain = append(plain, d.Path)
		}
	}
	if !slices.Equal(roots, []string{"~/d00", "~/d03", "~/d06", "~/d09"}) || !slices.Equal(plain, []string{"~/d01", "~/d02", "~/d04", "~/d05", "~/d07"}) {
		t.Fatalf("roots %v, plain %v", roots, plain)
	}
}

func TestRefresherWithoutBinaries(t *testing.T) {
	w := newDirsWorld(t)
	proj := filepath.Join(w.home, "proj")
	newRepo(t, proj)
	r := New(Options{Home: w.home, Mounts: func() (Mounts, error) { return w.mounts, nil }})
	r.Start()
	defer r.Stop()
	r.Sessions([]string{proj})
	time.Sleep(100 * time.Millisecond)
	if g := r.Git(proj); g != nil || len(r.Dirs()) != 0 {
		t.Fatalf("no git, no zoxide: %+v %v", g, r.Dirs())
	}
}

// TestRefreshCost measures a full refresh on a host with 40 sessions in
// 40 repos and a 500-entry zoxide database (one in five a repo), and the
// size of what a state carries for them.
func TestRefreshCost(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	w := newDirsWorld(t)
	var sessions, list []string
	for i := range 40 {
		d := filepath.Join(w.home, "work", fmt.Sprintf("project-%02d", i))
		newRepo(t, d)
		for j := range 20 {
			os.WriteFile(filepath.Join(d, fmt.Sprintf("f%02d.go", j)), []byte("package x\n"), 0o644)
		}
		sessions = append(sessions, d)
	}
	for i := range 500 {
		d := filepath.Join(w.home, "src", fmt.Sprintf("group-%d", i%10), fmt.Sprintf("some-directory-%03d", i))
		if i%5 == 0 {
			newRepo(t, d)
		} else {
			os.MkdirAll(d, 0o755)
		}
		list = append(list, d)
	}
	zox := fakeZoxide(t, t.TempDir(), list)
	// Rounds run here, one at a time, as the refresher's goroutine runs them.
	r := New(Options{Home: w.home, Fresh: time.Nanosecond, Mounts: func() (Mounts, error) { return w.mounts, nil }})
	r.git, r.zox = FindGit(), zox
	r.Sessions(sessions)
	measure := func(kind roundKind) (time.Duration, time.Duration) {
		before, start := cpu(), time.Now()
		r.round(kind)
		return time.Since(start), cpu() - before
	}
	measure(onNew)
	wallNew, cpuNew := measure(onTimer) // the first: zoxide's repos are new
	wallTimer, cpuTimer := measure(onTimer)
	wallLook, cpuLook := measure(onLook)
	st := r.Stats()
	dirs, _ := json.Marshal(r.Dirs())
	var withGit []proto.Session
	for i, s := range sessions {
		withGit = append(withGit, proto.Session{ID: fmt.Sprintf("$%d", i), Name: filepath.Base(s), Path: s, Git: r.Git(s)})
	}
	ss, _ := json.Marshal(withGit)
	for i := range withGit {
		withGit[i].Git = nil
	}
	bare, _ := json.Marshal(withGit)
	t.Logf("first periodic refresh (every repo new): %v wall, %v CPU", wallNew.Round(time.Millisecond), cpuNew.Round(time.Millisecond))
	t.Logf("periodic refresh: %v wall, %v CPU, %d git status runs", wallTimer.Round(time.Millisecond), cpuTimer.Round(time.Millisecond), st.Timer.Statuses)
	t.Logf("look: %v wall, %v CPU, %d git status runs", wallLook.Round(time.Millisecond), cpuLook.Round(time.Millisecond), st.Look.Statuses)
	t.Logf("%d dirs kept: %d bytes; git state adds %d bytes to 40 sessions; %d repos followed", st.Dirs, len(dirs), len(ss)-len(bare), st.Roots)
}

// cpu is the user and system time of this process and its finished
// children.
func cpu() time.Duration {
	var self, kids syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &self)
	syscall.Getrusage(syscall.RUSAGE_CHILDREN, &kids)
	tv := func(t syscall.Timeval) time.Duration { return time.Duration(t.Nano()) }
	return tv(self.Utime) + tv(self.Stime) + tv(kids.Utime) + tv(kids.Stime)
}

func sameDirs(a, b []proto.Dir) bool {
	return slices.EqualFunc(a, b, func(x, y proto.Dir) bool {
		return x.Path == y.Path && x.Root == y.Root && x.Net == y.Net && sameGit(x.Git, y.Git)
	})
}

func dirsString(ds []proto.Dir) string {
	var b strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&b, "[%s root=%v net=%v git=%+v] ", d.Path, d.Root, d.Net, d.Git)
	}
	return b.String()
}
