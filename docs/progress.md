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
- [ ] the attach loop (`tower`, `tower dash`), standbys
- [ ] scenarios (see scenarios.md for the per-scenario status)

## Scenarios waiting for the loop and the dashboard

Every towerd scenario passes on its own side. Their loop and UI parts are
marked `// loop part:` in the tests and listed as `pass (towerd part)` in
scenarios.md: S05, S13, S17, V02, V03, V04, V07, V08, LH01, LH02, LV01,
LC02, LC03. The towerd ops the loop and dashboard need are built and
unit-tested: `loop`, `loop-bye`, `prepare`, `after`, `wait-switch`,
`held`, `standby`, `last`, `view`, `watch`, `act` (with `switch`).

## Blocked

Nothing.

## Next

The attach loop (`internal/loop`, docs/design/loop.md) on the towerd ops
and the relay, with standbys and `ui.Pick`; then the loop and UI parts of
the scenarios listed above and the loop scenarios (S00, S04, S06–S12,
S19, S24–S27, D01, LH03, LV04, LC01, LC04, LC06, LC08, LD01, LD03, LS01–
LS05, LS09).
