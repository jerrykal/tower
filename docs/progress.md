# Progress

Where the build of v0.0.1 stands. A session resuming the work starts here.

## Steps

| Step | Status |
| --- | --- |
| 1. Skeleton, packaging, v0.0.0 | done |
| 2. Core: proto, stream, towerd, transport, loop, relay, harness | done; reviewed (`/code-review xhigh`) and every finding fixed |
| 3. Install on connect | done: reviewed (`/code-review high`), its 10 findings fixed, R03 passes on `pc` and `831` |
| 4. Dashboard (Atlas) | built: towerd's data sources (A01–A05) and the Atlas UI (U01–U08); `/code-review medium` to do |
| 5. `prefix o`, daily use, tag v0.0.1 | waits for the user |

## Step 2

- [x] docs: overview, protocol, design notes per package (`docs/design/`)
- [x] proto, config, tmux, stream, transport, client, with unit tests
- [x] harness and fake ssh (`test/scenario`, `mise run scenarios`)
- [x] towerd: the watch, registrations, keys and alert hooks, the keeper,
  the remote and home roles, links (stall, probe, give-up, reconnect,
  backoff, wake, network change), the merged view, loops, switches,
  prepare, after, standby offers, `last`, routing, the bridge
- [x] CLI: `towerd [--bridged] [--stdio] [--tmux]`, `_keep`, `_ensure`,
  `status [--json]`, `stop`, `netchange`, `host add|rm|ls|on|off`, `last`,
  `attach` (the shim, `internal/loop/shim.go`, standby mode included)
- [x] relay (LS06, LS07 and LS08 as package tests; LS08 also through the
  fake ssh, which now uses the relay's pty and mode helpers)
- [x] picker (Bubble Tea; `docs/design/ui.md`): the popup, `ui.Pick` for the loop, `tower _ui`
- [x] the attach loop (`tower`, `tower dash`), standbys
  (`internal/loop`, docs/design/loop.md)
- [x] scenarios (see scenarios.md): every simulated one passes (77 tests
  with step 3's, 13 minutes, at 561327c); R01 and R02 are run by hand on
  real hosts

## Step 2 scenarios

The dashboard's scenarios (S01, S09, S11, S19, LV04, LD01, LD03, LC07,
and the dashboard parts of S17, V04, V06, LV01) run through the real loop,
the popup and `tower _ui`; LH06 too runs the real loop. The picker ranks
its matches (exact name first), so the LC hosts' sessions are s0 … s400
again.

## Code review, end of step 2 (`/code-review xhigh`)

15 findings. Fixed (31e3cd1 and before): the control client ending a
reply at a fake `%end` in a capture; `XDG_RUNTIME_DIR` shared across
`TOWER_HOME`s; ctrl-c killing the loop between attaches; a loop stuck when
towerd went away; ctrl-c quitting the dashboard mid-⏎; standbys started
under the set's lock; the harness's process lookup on Linux; a stray
binary committed at the root (removed, `/tower` ignored); duplicates
(TOWER_CLIENT parsing, quoting, sync sequences, the deadline clock, two
tmux runs per local detach).

The seven in `internal/towerd`, fixed after step 3 merged (de8b377 …
50864e3), each with a test that fails on the code before it:

1. registrations saved from a copy taken under the daemon's lock, null
   entries dropped on load;
2. a reload compares hosts by value (`config.Host.Same`);
3. re-reads, states and views compared with ages at a fixed reference, so
   an unchanged tmux sends nothing (5 idle re-reads: 6 states and 12
   watcher wake-ups before, none after);
4. ssh's stderr read to its end before the process is waited for;
5. only the loop whose client moved becomes the last target;
6. a link made from the cache leaves it alone (ages counted once);
7. a hosts.toml that does not parse is read once, not on every view.

Also: towerd parses TOWER_CLIENT with `proto.ParseClient`, and the popup
and towerd's `M-o` binding share one size (`proto.PopupSize`). Suite after
the fixes: both shards pass (LC 5.4 min, the rest 7.5 min).

## Step 4: towerd's data for the dashboard

- [x] `internal/dirs` ([design/dirs.md](design/dirs.md)): the git state of
  directories (branch or short commit, dirt, a linked worktree's repo),
  zoxide dirs (roots, network mounts listed unchecked, capped at 100 roots
  and 100 others), refreshed in the background
- [x] towerd: sessions' git state and the zoxide dirs with no session in
  states and views; a dashboard's read is a look (`look` message), at
  most every 10s per towerd; `status --full` shows the refresher and
  towerd's CPU
- [x] requests: `panes`, capture's panes, `new` in a dir (`~` on the
  target host, `#` literal, a gone dir refused), a new window in its
  session's dir; `dup` and window rename and kill were there (tested now)
- [x] scenarios A01–A05 pass; both shards pass after the rebase onto
  15e29ef (LC 5.4 min, the rest 7.8 min)
- Costs (A05, 40 sessions in 40 repos, a 500-entry zoxide database):
  a state of 22 KB (8.7 KB without dirs and git state; 200 dirs are
  12.4 KB); a periodic refresh about 0.3s of CPU a minute, a look 0.8–1s
  (141 git status runs, 0.2–0.3s wall)

## Step 4: the Atlas UI

- [x] `internal/ui` rewritten as the column dashboard
  ([design/ui.md](design/ui.md)): hosts › sessions and dirs › windows ›
  layout preview, the breadcrumb and footer, the finder (where the popup
  and the loop's picker open), column search, `-` `.`, `J` `K`, `D`, `n`,
  `r`, `x` with a confirm that says what is at stake, `^g`, the add-host
  picker and host edits, help, mouse, width and long-name fitting, Rosé
  Pine and Nerd Font glyphs, on a cell canvas of its own
- [x] `internal/hosts`: `tower host` and the dashboard share the host
  list's edits, naming rules, ssh aliases and checks
- [x] kept: live updates, cursor by identity, kill without waiting, fresh
  re-resolve, hand-off, `TOWER_LIVE=0`, `tower _ui` (now also `find`,
  `ask-kill`, `open`, `dup`), `ui.Pick`
- [x] the suite drives the finder (`Prompt` is its pill, `rowRe` its rows,
  the cursor read from the breadcrumb; `^x y`); U01–U08 added; both shards
  pass
- Start-up (U08, pty, 15 runs): first frame 23–26ms median, a key's echo
  8ms; the step-2 picker measured the same way 24ms and 8ms
- Open: a pane picked with `J` `K` is selected by a local switch only (the
  loop's attach selects the window); ⏎ on a down host retries every down
  host (`netchange`, no per-host call)

## Paused (step 4 review fixes)

Paused at the user's request. `/code-review medium` on step 4 found five
bugs; all five are fixed and committed with tests (e4a6414 … the look
fix): a pane picked with J/K now reaches the attach (`--pane`, the go
line); a new window's ref names its session so the dashboard selects it;
an add-host result is delivered after the checks' 15s; a dir's session
takes `<name>_<n>` and `D` reuses only a duplicate in the session's group;
only a dashboard's reads (`ViewArgs.Look`) start a git/zoxide look.

To resume:

1. Run both scenario shards (CLAUDE.md) on main: the last commits (the
   look fix especially) have passed unit tests only; the shards were
   stopped when the laptop closed.
2. A different build of the same base version must replace the running
   towerd (a user's note): `client.Ensure` and `stop` replace only a
   strictly older one, so after `mise run build` of uncommitted code the
   old towerd keeps running (and the same on remote hosts through the
   bridge). Replace when the versions differ and the running one is not
   newer; test it (a rebuilt dev binary replaces the towerd; an older
   release never does).
3. towerd gaps the UI agent named: no call to retry one host (⏎ on a
   down host uses netchange, which retries all).
4. The final report (here and as an artifact), then step 5 with the user.

## Blocked

Nothing.

## Next

Paused here at the user's request, at a clean point: everything on
`main` is built, tested and pushed. In order:

1. ~~Fix the seven towerd findings.~~ Done.
2. ~~`/code-review high` for step 3, then fix.~~ Done (8a3c49d): remote
   lines through `sh -c` (fish/csh login shells), a verified upload (size
   and version), a platform directory, `current` never downgraded,
   checksums per archive, only successful installs counted, a content
   hash in dirty dev versions, one ssh runner adapter.
3. ~~The real hosts.~~ Done: R01, R02, R03 pass on `pc` and `831` (see
   scenarios.md); hosts clean after. R02 found standby shims left waiting
   behind Tailscale SSH; standbys now exit 30s after their loop's
   heartbeats stop (37896c5). A host may set `home` (TOWER_HOME there).
4. ~~Step 4: the Atlas dashboard.~~ Built; `/code-review medium` next.
5. The final report (here and as an artifact), then step 5 with the user.
