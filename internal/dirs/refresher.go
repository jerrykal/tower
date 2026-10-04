// Package dirs knows the directories a dashboard shows besides tmux's own
// listing: the git state of each session's directory, and the zoxide
// directories with no session yet. It learns them in the background, off
// the watch's re-read path, and never stats a path on a network mount.
package dirs

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

// Options configure a Refresher. Zero values take the defaults.
type Options struct {
	// Git and Zoxide find the binaries; they are called once, in the
	// refresher's goroutine (a version manager's shim may take a process
	// start to see through). nil, or "" from them: no git state, or no
	// zoxide directories.
	Git, Zoxide func() string
	Home        string                 // the home directory, for ~
	Mounts      func() (Mounts, error) // nil: SystemMounts
	Every       time.Duration          // a periodic refresh this often (60s)
	Fresh       time.Duration          // a look leaves a repo asked within this alone (2s)
	Workers     int                    // git processes at once (4)
	Timeout     time.Duration          // one git or zoxide run (10s)
	MaxRoots    int                    // zoxide directories kept that are git roots (100)
	MaxPlain    int                    // and that are not, or are not checked (100)
	SlowFor     time.Duration          // a root whose status timed out is left alone this long (10m)
	// OnChange is called, from the refresher's goroutine and with no
	// lock held, after what Git or Dirs answer changed: at most every
	// 200ms during a refresh, and once at its end.
	OnChange func()
	Log      func(format string, args ...any)
}

func (o *Options) defaults() {
	if o.Mounts == nil {
		o.Mounts = SystemMounts
	}
	if o.Every <= 0 {
		o.Every = time.Minute
	}
	if o.Fresh <= 0 {
		o.Fresh = 2 * time.Second
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.MaxRoots <= 0 {
		o.MaxRoots = 100
	}
	if o.MaxPlain <= 0 {
		o.MaxPlain = 100
	}
	if o.SlowFor <= 0 {
		o.SlowFor = 10 * time.Minute
	}
	if o.OnChange == nil {
		o.OnChange = func() {}
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
}

// notifyEvery bounds OnChange during a long refresh.
const notifyEvery = 200 * time.Millisecond

// Refresher keeps the git state of the session directories it is given
// and the zoxide list, refreshing them in a goroutine of its own: a new
// session directory at once; zoxide and the sessions' repos every
// Options.Every; everything on Look.
type Refresher struct {
	o      Options
	ctx    context.Context
	cancel context.CancelFunc
	kickC  chan struct{}
	lookC  chan struct{}
	doneC  chan struct{}

	mu       sync.Mutex
	started  bool
	git, zox string // the binaries, once found
	sessions map[string]bool      // session directories wanted, expanded
	where    map[string]place     // each session directory's repo
	list     []entry              // the zoxide list, capped
	roots    map[string]*rootInfo // by root directory
	mounts   Mounts
	mounted  bool // mounts has been read
	dirs     []proto.Dir
	dirsOK   bool // dirs is up to date
	pending  bool // a change OnChange has not been told
	notified time.Time
	stats    Stats
	logged   map[string]string // the last error logged per source, logged once
}

type place struct {
	repo Repo
	in   bool // in a repo
	net  bool
}

type entry struct {
	path, short, main string
	root, net         bool
}

type rootInfo struct {
	main      string
	git       *proto.Git // nil until git answered (or when it fails)
	at        time.Time  // last asked
	slowUntil time.Time
}

// Stats say what the refresher holds and what its last refreshes cost.
type Stats struct {
	Git, Zoxide string // the binaries in use ("" none)
	Dirs, Roots int    // zoxide directories kept; repos followed
	Rounds      int    // refreshes so far
	Timer, Look Cost   // the last periodic refresh and the last look
}

// Cost is one refresh: its wall time and the git status runs it made.
type Cost struct {
	Took     time.Duration
	Statuses int
}

// New makes a refresher; Start runs it.
func New(o Options) *Refresher {
	o.defaults()
	r := &Refresher{o: o, kickC: make(chan struct{}, 1), lookC: make(chan struct{}, 1), doneC: make(chan struct{}),
		sessions: map[string]bool{}, where: map[string]place{}, roots: map[string]*rootInfo{}, logged: map[string]string{}}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r
}

// Start runs the refresher until Stop.
func (r *Refresher) Start() {
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	go r.run()
}

// Stop ends the refresher; git and zoxide runs under way are killed.
func (r *Refresher) Stop() {
	r.cancel()
	r.mu.Lock()
	started := r.started
	r.mu.Unlock()
	if started {
		select {
		case <-r.doneC:
		case <-time.After(3 * time.Second):
		}
	}
}

// Sessions sets the session directories to follow. A directory not seen
// before is looked at at once; the rest wait for the next refresh. It
// only compares and records, so it is cheap enough for every re-read.
func (r *Refresher) Sessions(dirs []string) {
	set := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		if p := Expand(d, r.o.Home); p != "" {
			set[p] = true
		}
	}
	r.mu.Lock()
	if maps.Equal(set, r.sessions) {
		r.mu.Unlock()
		return
	}
	r.sessions = set
	r.dirsOK = false
	fresh := false
	for p := range set {
		if _, ok := r.where[p]; !ok {
			fresh = true
			break
		}
	}
	r.mu.Unlock()
	if fresh {
		signal(r.kickC)
	}
}

// Look asks for a refresh of everything now (a dashboard opened): zoxide,
// and every repo not asked within Options.Fresh. It never waits.
func (r *Refresher) Look() { signal(r.lookC) }

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// Git is the git state of a session directory, or nil (not in a repo, on
// a network mount, not looked at yet, or git failed). The value is never
// changed once handed out.
func (r *Refresher) Git(dir string) *proto.Git {
	p := Expand(dir, r.o.Home)
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.where[p]
	if !ok || !w.in {
		return nil
	}
	if ri := r.roots[w.repo.Root]; ri != nil {
		return ri.git
	}
	return nil
}

// Dirs are the zoxide directories with no session yet, most frecent
// first. The slice is shared: callers must not change it.
func (r *Refresher) Dirs() []proto.Dir {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dirsOK {
		return r.dirs
	}
	var out []proto.Dir
	for _, e := range r.list {
		if r.sessions[e.path] {
			continue
		}
		d := proto.Dir{Path: e.short, Root: e.root, Net: e.net}
		if e.root {
			if ri := r.roots[e.path]; ri != nil {
				d.Git = ri.git
			}
		}
		out = append(out, d)
	}
	r.dirs, r.dirsOK = out, true
	return out
}

// Net reports whether path (~ allowed) is on a network mount, by the
// mount table as last read.
func (r *Refresher) Net(path string) bool {
	p := Expand(path, r.o.Home)
	r.mu.Lock()
	if !r.mounted {
		r.mu.Unlock()
		m := r.readMounts()
		r.mu.Lock()
		r.mounts, r.mounted = m, true
	}
	defer r.mu.Unlock()
	return r.mounts.Net(p)
}

// Stats is what the refresher holds now.
func (r *Refresher) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	s.Git, s.Zoxide, s.Dirs, s.Roots = r.git, r.zox, len(r.list), len(r.roots)
	return s
}

func (r *Refresher) run() {
	defer close(r.doneC)
	git, zox := "", ""
	if r.o.Git != nil {
		git = r.o.Git()
	}
	if r.o.Zoxide != nil {
		zox = r.o.Zoxide()
	}
	r.mu.Lock()
	r.git, r.zox = git, zox
	r.mu.Unlock()
	r.o.Log("dirs: git %q, zoxide %q, every %v", git, zox, r.o.Every)
	r.round(onTimer)
	t := time.NewTicker(r.o.Every)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			r.round(onTimer)
		case <-r.lookC:
			r.round(onLook)
		case <-r.kickC:
			r.round(onNew)
		}
	}
}

// What a round refreshes. Every round looks at session directories not
// seen before and asks git about repos never asked.
type roundKind int

const (
	onNew   roundKind = iota // only that
	onTimer                  // also zoxide, every session directory, and the sessions' repos
	onLook                   // also zoxide, every session directory, and every repo not asked within Fresh
)

func (r *Refresher) readMounts() Mounts {
	m, err := r.o.Mounts()
	if err != nil {
		r.logOnce("mounts", "dirs: reading the mount table: %v", err)
	}
	return m
}

// round refreshes what its kind says. The zoxide directories' repos are
// asked about on a look only (and once when they first show up): only an
// open dashboard shows them, and there are many more of them.
func (r *Refresher) round(kind roundKind) {
	full := kind != onNew
	start := time.Now()
	m := r.readMounts()
	r.mu.Lock()
	r.mounts, r.mounted = m, true
	zox := r.zox
	sessions := slices.Collect(maps.Keys(r.sessions))
	known := maps.Clone(r.where)
	r.mu.Unlock()

	var list []entry
	listed := false
	if full && zox != "" {
		l, err := Query(r.ctx, zox, r.o.Timeout)
		if err != nil {
			r.logOnce("zoxide", "dirs: zoxide: %v", err)
		} else {
			list, listed = r.pick(l, m), true
		}
	}
	where := make(map[string]place, len(sessions))
	for _, p := range sessions {
		if w, ok := known[p]; ok && !full {
			where[p] = w
			continue
		}
		where[p] = locate(p, m)
	}

	r.mu.Lock()
	changed := false
	// A session directory that went away meanwhile is not kept.
	for p := range where {
		if !r.sessions[p] {
			delete(where, p)
		}
	}
	for p, w := range r.where {
		if r.sessions[p] {
			if _, ok := where[p]; !ok {
				where[p] = w // arrived meanwhile: the next round
			}
		}
	}
	if !maps.Equal(where, r.where) {
		r.where = where
		changed = true
	}
	if listed && !slices.Equal(list, r.list) {
		r.list = list
		changed = true
	}
	want := map[string]string{} // root → main worktree's name
	ofSession := map[string]bool{}
	for _, w := range r.where {
		if w.in {
			want[w.repo.Root] = w.repo.Main
			ofSession[w.repo.Root] = true
		}
	}
	for _, e := range r.list {
		if e.root {
			want[e.path] = e.main
		}
	}
	for root := range r.roots {
		if _, ok := want[root]; !ok {
			delete(r.roots, root)
		}
	}
	var jobs []string
	now := time.Now()
	for root, main := range want {
		ri := r.roots[root]
		if ri == nil {
			ri = &rootInfo{main: main}
			r.roots[root] = ri
		}
		if ri.main != main {
			ri.main = main
			if ri.git != nil {
				g := *ri.git
				g.Repo = main
				ri.git = &g
				changed = true
			}
		}
		if r.git == "" || now.Before(ri.slowUntil) {
			continue
		}
		switch {
		case ri.at.IsZero(),
			kind == onTimer && ofSession[root],
			kind == onLook && now.Sub(ri.at) >= r.o.Fresh:
			jobs = append(jobs, root)
		}
	}
	git := r.git
	if changed {
		r.dirsOK, r.pending = false, true
	}
	r.mu.Unlock()
	r.notify(false)

	slices.Sort(jobs) // a stable order, for logs and tests
	sem := make(chan struct{}, r.o.Workers)
	var wg sync.WaitGroup
	for _, root := range jobs {
		select {
		case sem <- struct{}{}:
		case <-r.ctx.Done():
		}
		if r.ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.status(git, root)
		}()
	}
	wg.Wait()
	r.mu.Lock()
	r.stats.Rounds++
	switch kind {
	case onTimer:
		r.stats.Timer = Cost{time.Since(start), len(jobs)}
	case onLook:
		r.stats.Look = Cost{time.Since(start), len(jobs)}
	}
	r.mu.Unlock()
	r.notify(true)
}

// status asks git about one root and records the answer.
func (r *Refresher) status(git, root string) {
	g, err := Status(r.ctx, git, root, r.o.Timeout)
	if r.ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	ri := r.roots[root]
	if ri == nil {
		r.mu.Unlock()
		return
	}
	ri.at = time.Now()
	var problem string
	switch {
	case err == ErrSlow:
		ri.slowUntil = time.Now().Add(r.o.SlowFor)
		r.mu.Unlock()
		r.o.Log("dirs: git status in %s took over %v: left alone for %v", root, r.o.Timeout, r.o.SlowFor)
		return
	case err != nil:
		g, problem = nil, err.Error()
	default:
		g.Repo = ri.main
	}
	if !sameGit(g, ri.git) {
		ri.git = g
		r.dirsOK, r.pending = false, true
	}
	r.mu.Unlock()
	if problem != "" {
		r.logOnce("git "+root, "dirs: git status in %s: %s", root, problem)
	}
	r.notify(false)
}

func sameGit(a, b *proto.Git) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// notify tells OnChange about a pending change: at most every
// notifyEvery, unless final.
func (r *Refresher) notify(final bool) {
	r.mu.Lock()
	if !r.pending || !final && time.Since(r.notified) < notifyEvery {
		r.mu.Unlock()
		return
	}
	r.pending = false
	r.notified = time.Now()
	r.mu.Unlock()
	r.o.OnChange()
}

func (r *Refresher) logOnce(key, format string, args ...any) {
	r.mu.Lock()
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	same := r.logged[key] == msg
	r.logged[key] = msg
	r.mu.Unlock()
	if !same {
		r.o.Log("%s", msg)
	}
}

// locate finds the repo a session directory is in, unless it is on a
// network mount.
func locate(p string, m Mounts) place {
	if m.Net(p) {
		return place{net: true}
	}
	repo, ok := FindRepo(p, m)
	return place{repo: repo, in: ok}
}

// pick turns zoxide's list into the entries kept: most frecent first,
// directories that no longer exist left out, at most MaxRoots git roots
// and MaxPlain others (a directory on a network mount, never checked,
// counts as another).
func (r *Refresher) pick(list []string, m Mounts) []entry {
	var out []entry
	roots, plain := 0, 0
	for _, p := range list {
		if roots >= r.o.MaxRoots && plain >= r.o.MaxPlain {
			break
		}
		e := entry{path: p, short: Short(p, r.o.Home)}
		if m.Net(p) {
			e.net = true
		} else {
			st, err := os.Stat(p)
			if err != nil || !st.IsDir() {
				continue
			}
			if repo, ok := RepoAt(p); ok {
				e.root, e.main = true, repo.Main
			}
		}
		if e.root {
			if roots >= r.o.MaxRoots {
				continue
			}
			roots++
		} else {
			if plain >= r.o.MaxPlain {
				continue
			}
			plain++
		}
		out = append(out, e)
	}
	return out
}
