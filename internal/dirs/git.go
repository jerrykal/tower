package dirs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/proto"
)

// Repo is what the file system says of a git repo's root, before git is
// asked anything.
type Repo struct {
	Root string // the worktree's top directory
	// Main is the main worktree's directory name when Root is a linked
	// worktree (`git worktree add`), else "".
	Main string
}

// FindRepo walks up from dir (absolute, clean) to the first directory
// holding a .git entry. A directory on a network mount is never looked
// at, and the walk stops there. ok is false outside any repo.
func FindRepo(dir string, m Mounts) (Repo, bool) {
	for p := dir; ; {
		if m.Net(p) {
			return Repo{}, false
		}
		if r, ok := RepoAt(p); ok {
			return r, true
		}
		up := filepath.Dir(p)
		if up == p {
			return Repo{}, false
		}
		p = up
	}
}

// RepoAt reports whether dir is a repo's root (a .git directory, or the
// .git file of a linked worktree or a submodule) and which.
func RepoAt(dir string) (Repo, bool) {
	dotgit := filepath.Join(dir, ".git")
	st, err := os.Lstat(dotgit)
	if err != nil {
		return Repo{}, false
	}
	r := Repo{Root: dir}
	if st.Mode().IsRegular() {
		r.Main = mainOf(dir, dotgit)
	}
	return r, true
}

// mainOf names the main worktree of the linked worktree whose .git file
// is dotgit: the file says `gitdir: <repo>/.git/worktrees/<name>`, and that
// directory's commondir file leads to the repo's own .git. A submodule's
// gitdir has no commondir: not a linked worktree.
func mainOf(dir, dotgit string) string {
	b, err := os.ReadFile(dotgit)
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	c, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(c))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) == ".git" {
		return filepath.Base(filepath.Dir(common))
	}
	// A bare repo with worktrees: its own name, without .git.
	return strings.TrimSuffix(filepath.Base(common), ".git")
}

// ErrSlow is a git status that did not finish in its time.
var ErrSlow = errors.New("git status took too long")

// Status asks git for the branch (or the short commit when detached) and
// whether the worktree at root has changes. It never takes git's optional
// locks, so it cannot get in the way of the user's own git; submodules
// count only when their commit changed; untracked files count as git's
// own status.showUntrackedFiles says.
func Status(ctx context.Context, git, root string, timeout time.Duration) (*proto.Git, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "--no-optional-locks", "-C", root,
		"status", "--porcelain=v2", "--branch", "--ignore-submodules=dirty")
	cmd.Env = gitEnv()
	// A process stuck past its kill (a disk that hangs) must not hold the
	// refresher: Wait gives up on it after this.
	cmd.WaitDelay = time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ErrSlow
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return ParseStatus(out.Bytes()), nil
}

// ParseStatus reads `git status --porcelain=v2 --branch`: the branch from
// its header, a short commit for a detached HEAD, dirty if any entry
// follows.
func ParseStatus(out []byte) *proto.Git {
	g := &proto.Git{}
	var oid, head string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "# branch.oid "):
			oid = strings.TrimPrefix(l, "# branch.oid ")
		case strings.HasPrefix(l, "# branch.head "):
			head = strings.TrimPrefix(l, "# branch.head ")
		case strings.HasPrefix(l, "#"), l == "":
		default:
			g.Dirty = true
		}
	}
	g.Branch = head
	if head == "(detached)" {
		g.Branch = oid
		if len(g.Branch) > 7 {
			g.Branch = g.Branch[:7]
		}
	}
	return g
}

// gitEnv is this process's environment without what would point git at
// another repo or prompt.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
			"GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES", "GIT_TERMINAL_PROMPT", "GIT_OPTIONAL_LOCKS":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
}
