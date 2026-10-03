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
- [ ] towerd (an agent is building it in a worktree)
- [x] relay (LS06, LS07 and the relay part of LS08 as package tests)
- [ ] loop, standbys
- [ ] picker (Bubble Tea; `docs/design/ui.md`; an agent is building it)
- [ ] scenarios (see scenarios.md for the per-scenario status)

## Blocked

Nothing yet.

## Next

Merge the towerd and relay agents' branches (review the diffs, run their
scenarios), then the loop, standbys and the picker.
