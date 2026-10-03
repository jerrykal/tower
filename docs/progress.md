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
- [ ] scenarios (see scenarios.md for the per-scenario status): the
  loop's pass; the UI-centred ones remain

## Scenarios waiting for the dashboard

The loop's scenarios and the loop parts of towerd's pass (scenarios.md).
The UI-centred ones remain: S09, S11, S19, LV04, LD01, LD03, and the UI
parts of S17, V04, V06 and LV01 (still `pass (towerd part)` or `todo`).

## Blocked

Nothing.

## Next

The UI-centred scenarios above (S09, S11, S19, LV04, LD01, LD03, the UI
parts of S17, V04, V06, LV01), then step 3, install on connect.
