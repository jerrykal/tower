# Progress

Where the build of v0.0.1 stands. A session resuming the work starts here.

## Steps

| Step | Status |
| --- | --- |
| 1. Skeleton, packaging, v0.0.0 | done |
| 2. Core: proto, stream, towerd, transport, loop, relay, harness | built; 7 towerd review findings to fix |
| 3. Install on connect | built (I01–I05 pass); `/code-review high` and R03 on real hosts to do |
| 4. Dashboard (Atlas) | not started |
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

**Still to fix, all in `internal/towerd`** (held back while step 3 changed
the same files):

1. `clients.go:52` — `registry.save` encodes entries without `Daemon.mu`
   while `bind` filters the list in place: `null` entries in clients.json,
   then a nil dereference on every start.
2. `home.go:151` — reload compares `config.Host` values with pointer fields
   (`Enabled`, `Standby`): any host setting them is rebuilt on every reload.
3. `watch.go:46` — the snapshot dedup key includes `Ago`, so an unchanged
   tmux still bumps watchers and pushes states and views (also
   `remote.go:169`, `link.go:647`).
4. `link.go:443` — `Wait` before the stderr reader finishes loses ssh's
   reason ("ssh exited 255" instead of the fix).
5. `home.go:299` — `changed` not reset per client in `applyClients`: the
   wrong session becomes the last target.
6. `link.go:157` — `fromCache` shifts `Ago` in place in the shared cache:
   offline hosts' sessions age twice.
7. `home.go:135` — a hosts.toml that fails to parse is re-parsed and logged
   on every view call.

Also: towerd's own `parseClient` should switch to `proto.ParseClient`
(decision 54); the merged view is encoded once per link under the lock
(once would do); the popup size is 80% in `ui.PopupSize` but 90% in
towerd's `M-o` binding.

## Blocked

Nothing.

## Next

Paused here at the user's request, at a clean point: everything on
`main` is built, tested and pushed. In order:

1. Fix the seven towerd findings above (each with a regression test), run
   the suite in its two shards (CLAUDE.md), merge.
2. `/code-review high` for step 3 (install on connect), then fix.
3. The real hosts from the main session only: R01, R02, and R03 (install
   into `~/.cache/tower-test/harness/` with `TOWER_INSTALL_DIR`), under
   CLAUDE.md's safety rules.
4. Step 4: the Atlas dashboard (`docs/design/ui.md`, stage 2), then
   `/code-review medium`.
5. The final report (here and as an artifact), then step 5 with the user.
