# Progress

Where the build of v0.0.1 stands. A session resuming the work starts here.

## Steps

| Step | Status |
| --- | --- |
| 1. Skeleton, packaging, v0.0.0 | done |
| 2. Core: proto, stream, towerd, transport, loop, relay, harness | in progress |
| 3. Install on connect | not started |
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
- [x] scenarios (see scenarios.md): every simulated one passes (69 tests,
  13 minutes); R01 and R02 are run by hand on real hosts

## Step 2 scenarios

The dashboard's scenarios (S01, S09, S11, S19, LV04, LD01, LD03, LC07,
and the dashboard parts of S17, V04, V06, LV01) run through the real loop,
the popup and `tower _ui`; LH06 too runs the real loop. The picker ranks
its matches (exact name first), so the LC hosts' sessions are s0 … s400
again.

## Blocked

Nothing.

## Next

Step 3, install on connect (in progress in its own branch), then the real
hosts (R01, R02, by hand).
