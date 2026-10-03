# tower

tower makes the tmux servers on several machines feel like one local tmux
server. Read [docs/overview.md](docs/overview.md) for the product,
[docs/protocol.md](docs/protocol.md) for how the parts talk,
[docs/design/](docs/design/README.md) before changing a package, and
[docs/progress.md](docs/progress.md) to resume unfinished work.

## Working here

- Build and test through mise: `mise run build`, `mise run test`. The
  scenario suite is `mise run scenarios` (see below).
- Commits are unscoped conventional commits (`feat: …`, `fix: …`,
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
  aliases `pc`, `831`, `gb200` and `pp`; no IPs, no ssh config, nothing
  under other users' homes.
- **Real hosts** (`TOWER_REAL=pc,831`) are for the main session only, never
  a subagent. They use only `tmux -L tower-test-…` sockets and only
  `~/.cache/tower-test/harness/`, never the rest of `~/.cache/tower-test`
  (the user's demo lives there). Teardown stops the home before cleaning
  the remote, or the home reconnects and restarts towerd there; afterwards,
  check the host is clean.

## The scenario suite

`test/scenario` is the acceptance suite: simulated hosts are tmux servers
with their own `TOWER_HOME` and machine id, reached through the fake ssh.

- **Isolate every run**: a short `TMUX_TMPDIR` (socket paths are limited to
  104 bytes), a private `TOWER_TEST_DIR` and a private `TMPDIR`, and `unset
  TMUX TMUX_PANE`. Parallel runs need their own three.
- **Run the full suite in the background** and read its log; it takes about
  20 minutes. Run a single scenario with `-run`.
- **Timing checks are load-sensitive.** Rerun a failure alone before
  believing it, and never loosen a threshold to make a run pass.
- `tmux` and `fzf` may be mise shims costing 60–80ms a call; tower resolves
  the real binaries (`internal/tmux.Resolve`). Keep every hot path on the
  resolved binary.
- Leave no processes, tmux servers or temp directories behind; the harness
  teardown checks.
