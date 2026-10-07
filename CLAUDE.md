# tower

tower makes the tmux servers on several machines feel like one local tmux
server. Read [docs/overview.md](docs/overview.md) for the product,
[docs/protocol.md](docs/protocol.md) for how the parts talk,
[docs/design/](docs/design/README.md) before changing a package, and
[docs/progress.md](docs/progress.md) to resume unfinished work.

## Working here

- Build and test through mise: `mise run build`, `mise run test`. The
  scenario suite is `mise run scenarios` (see below).
- Commits are unscoped conventional commits (`feat: …`, `fix: …`, `perf: …`,
  `docs: …`, `test: …`, `ci: …`), no attribution lines.
- Write docs, comments and commit messages in tower's own terms.
- A design change goes into the package's note in `docs/design/` and a
  line in [docs/decisions.md](docs/decisions.md); the behaviour in
  [docs/scenarios.md](docs/scenarios.md) must still hold.
- Latency is the product: measure before and after a change on any
  hand-off, stream or dashboard path.

## Safety

- **The user's tmux.** Every tmux command in tests and experiments names
  its own server (`-L tt-…`, `-L tower-test-…`). The default server, and the
  `-L tower-demo` server, belong to the user: leave them alone, and never
  run `kill-server`.
- **The repo is public.** Host names in code, docs and tests are only the
  placeholders `alpha`, `bravo`, `charlie` and `delta`, never a real host
  name; no IPs, no ssh config, nothing under other users' homes.
- **Real hosts** (`TOWER_REAL=<host>,<host>`) are for the main session only, never
  a subagent. They use only `tmux -L tower-test-…` sockets and only
  `~/.cache/tower-test/harness/`, never the rest of `~/.cache/tower-test`
  (the user's demo lives there). Teardown stops the home before cleaning
  the remote, or the home reconnects and restarts towerd there; afterwards,
  check the host is clean.
- **Docker.** Every container, network, volume and image a test or
  experiment makes carries the label `tower-test` and a `tt-` name
  prefix. Teardown and cleanup select by that label only: never
  `docker system prune`, an unfiltered `docker rm`, or anything that
  touches the machine's other containers. Afterwards, check none of
  yours is left.

## The scenario suite

`test/scenario` is the acceptance suite: simulated hosts are tmux servers
with their own `TOWER_HOME` and machine id, remotes reached over real ssh:
host containers on Linux, sshds of their own on macOS (`TOWER_HOSTS`).

- **Run it through `mise run scenarios`**, which isolates each run. A
  direct `go test ./test/scenario` needs the same: a short `TMUX_TMPDIR`
  (socket paths are limited to 104 bytes), a private `TOWER_TEST_DIR` and
  `TMPDIR`, and `unset TMUX TMUX_PANE`.
- **Run the suite in two foreground shards**, each one Bash call with a
  10-minute timeout, and read its result in the same turn:
  `mise run scenarios -run '^TestLC'` (about 6 minutes) and
  `mise run scenarios -skip '^TestLC'` (about 8). A family that outgrows
  its shard is split again. Run a single scenario with `-run`.
- **Wait for every run you start and report it in the same turn.** A run
  left in the background can finish without waking you, and the work then
  stalls with no report. If a run must go to the background, wait on it
  (Monitor with an until-loop on its log) before stopping.
- **Timing checks are load-sensitive.** Rerun a failure alone before
  believing it, and never loosen a threshold to make a run pass.
- `tmux` and `fzf` may be mise shims costing 60–80ms a call; tower resolves
  the real binaries (`internal/tmux.Resolve`). Keep every hot path on the
  resolved binary.
- Leave no processes, tmux servers or temp directories behind. Teardown
  catches a stray tmux server; check processes and temp directories
  yourself.

## Subagents

- Brief every agent with the shard commands, the wait-and-report rule, and
  the Safety rules above.
- An agent whose turn ends without its report has stalled: check its
  worktree (`git -C <worktree> log`, its processes) at once, and resume it
  with a direct instruction to finish and report, or take over its
  remaining checks yourself.

