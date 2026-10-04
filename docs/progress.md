# Progress

Where the build of v0.0.1 stands. A session resuming the work starts here.

## Steps

| Step | Status |
| --- | --- |
| 1. Skeleton, packaging, v0.0.0 | done |
| 2. Core: proto, stream, towerd, transport, loop, relay, harness | done; reviewed (`/code-review xhigh`) and every finding fixed |
| 3. Install on connect | done: reviewed (`/code-review high`), its 10 findings fixed, R03 passes on `pc` and `831` |
| 4. Dashboard (Atlas) | in progress: two agents (towerd data sources; the Atlas UI) |
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
4. Step 4: the Atlas dashboard (`docs/design/ui.md`, stage 2), then
   `/code-review medium`.
5. The final report (here and as an artifact), then step 5 with the user.
