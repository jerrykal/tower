# dirs

The directories a dashboard shows besides tmux's own listing: the git
state of each session's directory (branch, dirt, the repo of a linked
worktree) and the zoxide directories with no session yet. towerd learns
them in the background with a `Refresher`, never on the watch's re-read
path, and never stats a path on a network mount.

## Files

| File | Holds |
| --- | --- |
| `refresher.go` | `Refresher`: what it follows, its rounds, the answers `Git` and `Dirs` |
| `git.go` | `FindRepo` and `RepoAt` (a repo's root and linked worktree from the file system), `Status` (one `git status`) |
| `zoxide.go` | `Query` (`zoxide query --list --all`), finding the binaries, `Short` and `Expand` (`~`) |
| `mounts.go`, `mounts_linux.go`, `mounts_darwin.go` | the mount table and which filesystems are network ones |

## API

```go
type Options struct {
    Git, Zoxide func() string          // find the binaries (in the refresher's goroutine); nil or "": none
    Home        string                 // for ~
    Mounts      func() (Mounts, error) // nil: SystemMounts
    Every       time.Duration          // periodic refresh (60s)
    Fresh       time.Duration          // a look leaves a repo asked this recently alone (2s)
    Workers     int                    // git processes at once (4)
    Timeout     time.Duration          // one git or zoxide run (10s)
    MaxRoots, MaxPlain int             // zoxide directories kept (100 git roots, 100 others)
    SlowFor     time.Duration          // a repo whose status timed out is left alone (10m)
    OnChange    func()                 // Git or Dirs would answer differently
    Log         func(string, ...any)
}

func New(o Options) *Refresher
func (r *Refresher) Start()
func (r *Refresher) Stop()
func (r *Refresher) Sessions(dirs []string)        // the session directories to follow
func (r *Refresher) Look()                         // a dashboard opened: refresh everything now
func (r *Refresher) Git(dir string) *proto.Git     // a session directory's state, or nil
func (r *Refresher) Dirs() []proto.Dir             // zoxide directories with no session, most frecent first
func (r *Refresher) Net(path string) bool          // on a network mount?
func (r *Refresher) Stats() Stats
```

`Sessions` only compares and records (a map of about 40 entries), so the
watch calls it on every re-read that changed something; a directory not
seen before is looked at at once in the refresher's goroutine. `Git` and
`Dirs` read under the refresher's lock and return values never changed
once handed out (a change makes new ones), so states and views marshal
them without copying.

## Rounds

One goroutine runs the rounds, one at a time:

| Round | When | Does |
| --- | --- | --- |
| new | `Sessions` saw a directory it never looked at | find that directory's repo; ask git about repos never asked |
| periodic | at start, then every `Every` (60s) | read the mount table and zoxide; find every session directory's repo again; ask git about every session's repo (and repos never asked) |
| look | `Look` (towerd: a dashboard read, at most every 10s) | as periodic, and ask git about every repo, the zoxide ones too, not asked within `Fresh` |

A round's git runs go out `Workers` at a time; each has `Timeout`, after
which the process is killed and `Wait` gives up on it a second later
(`WaitDelay`), so a stuck process holds no round. A repo whose status
timed out is left alone for `SlowFor`; a failing one (not a repo any
more, another owner's) has no state and is logged once. `OnChange` is
called after a change at most every 200ms during a round and once at its
end.

**Why these periods.** A git status costs about 5ms of CPU (mostly the
process start), so the cost is the number of repos. Only an open
dashboard shows the zoxide directories, and a dashboard opening looks, so
their repos are refreshed only then; the sessions' repos are also kept
fresh in the background (60s), so a dashboard's first frame is at most a
minute old and the look corrects it within a few hundred milliseconds. On
a host with 40 sessions in 40 repos and a 500-entry zoxide database (one
in five a repo; 140 repos followed): a periodic refresh costs about 0.2–0.3s
of CPU (0.4–0.6% of a core at 60s), a look about 0.75–1s, spread over 4
processes in 0.2–0.3s wall (`TestRefreshCost`, A05).

## Git state

- **The repo** comes from the file system, not from git: walk up from the
  directory to the first one holding a `.git` entry (`FindRepo`). A `.git`
  file is a linked worktree when the `gitdir` it names has a `commondir`;
  the main worktree's directory name is the parent of that common
  directory (`<repo>/.git`), or a bare repo's name without `.git`. A
  submodule's gitdir has no `commondir`. No process runs for a directory
  outside any repo, which most zoxide entries are.
- **The state** is one `git --no-optional-locks -C <root> status
  --porcelain=v2 --branch --ignore-submodules=dirty`: the branch from
  `# branch.head`, the first 7 characters of `# branch.oid` when
  detached, dirty when any entry follows. No optional locks, so it never
  takes `index.lock` from under the user's own git; submodules count when
  their commit moved, not for changes inside them (a status inside each
  would multiply the cost); untracked files count as the repo's own
  `status.showUntrackedFiles` says (a huge repo can turn them off).
  `GIT_DIR` and its kin are dropped from git's environment, and
  `GIT_TERMINAL_PROMPT=0` set.
- **Sessions in one repo** share its state: the cache is keyed by root.

## Zoxide directories

`zoxide query --list --all`: most frecent first, `--all` so zoxide does
not stat each entry itself. Each entry, in order: on a network mount, kept
as `Net` and never stat'ed; else left out if it is not a directory any
more; a git root (a `.git` file or directory) is `Root` and gets its
repo's state. At most `MaxRoots` roots and `MaxPlain` others are kept
(network entries count as others): the dashboard lists roots by default
and every entry after `^g`, so the long tail of plain directories is cut
first. 200 entries are about 12 KB of a state (A05). A zoxide that is
missing, or fails, gives no directories; an empty database is no
directories.

**Which side filters.** `Dirs` leaves out the directories that are a
session's directory (compared after `~` and cleaning), since the dashboard
shows "dirs with no session yet": towerd knows its own home directory, the
dashboard does not know a remote's. The list follows sessions at once: a
session made in a dir takes it off the list in the state sent with the
request's answer (read your writes, A02).

## Network mounts

A stat on a hard NFS mount whose server is gone blocks until the server
answers. Network filesystems come from the mount table:
`/proc/self/mounts` on Linux (the kernel writes it from memory) and
`getfsstat(MNT_NOWAIT)` on macOS (what `mount` prints, from the kernel's
cached statistics, with no process start). Network types: `nfs`, `nfs4`,
`cifs`, `smb3`, `smbfs`, `ncpfs`, `afs`, `9p`, `ceph`, `glusterfs`,
`lustre`, `gpfs`, `beegfs`, `davfs`, `autofs` (a stat triggers the
mount), `fuse`, every `fuse.<name>` but known local ones (`lxcfs`,
`snapfuse`, `squashfuse`, `portal`, `mergerfs`, `bindfs`, `encfs`,
`gocryptfs`, `ntfs-3g`, `appimaged`), and on macOS `afpfs`, `webdav`,
`macfuse`, `osxfuse`, `osxfusefs`, `fusefs`. A path is on the deepest
mount that contains it; the walk up for a repo stops at a network mount.
Paths are compared as written: a symlink into a network mount is not seen
through, since resolving it is the stat that blocks. A home directory on
NFS (common on clusters) therefore has no git state at all; its zoxide
entries are listed unchecked.

`TOWER_TEST_MOUNTS` (a file in `/proc/self/mounts` format) replaces the
table, for the scenario suite.

## Finding the binaries

`tmux.Resolve` past version-manager shims, in the refresher's goroutine
(a shim may cost a `mise which`), never on towerd's start. On macOS
`/usr/bin/git` is a stub that opens an installer dialog when the command
line tools are missing, so it is used only when `xcode-select -p`
succeeds.

## Tests

`dirs_test.go`: the mount table (escapes, deepest mount, FUSE types), the
system's table, `FindRepo` (a repo, a subdirectory, a linked worktree, a
submodule, a network mount), `Status` (clean, untracked, modified,
detached, unborn, not a repo), `Query` with a fake zoxide, the refresher
end to end (sessions, worktree, dirs with a network one that does not
exist, the session filter, a look after an edit), the caps, no binaries,
and `TestRefreshCost` (not in `-short`), which prints the costs above.
