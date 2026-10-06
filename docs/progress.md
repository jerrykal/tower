# Progress

Where the build of v0.0.1 stands. A session resuming the work starts here.

**Final report:** https://claude.ai/artifact/CWeFXFcBWUMBDUyXMwE9az (the same content as this page, laid out
to scan).

## Status

| Step | Status |
| --- | --- |
| 1. Skeleton, packaging, v0.0.0 | done |
| 2. Core: proto, stream, towerd, transport, loop, relay, harness | done; `/code-review xhigh`, all 15 findings fixed |
| 3. Install on connect | done; `/code-review high`, all 10 findings fixed; R03 passes on `alpha` and `bravo` |
| 4. Dashboard (Atlas) | done; `/code-review medium`, all 5 findings fixed |
| 5. `prefix o`, daily use, tag v0.0.1 | waits for the user |

## Waiting for the user

- [ ] The first real install into `~/.local/share/tower` on `alpha` and `bravo`:
      tower installs itself on connect; until now it was only installed
      into `~/.cache/tower-test/harness/install` (R03). Running `tower`
      from a home whose `hosts.toml` lists them does it.
- [ ] Step 5: bind `prefix o` in the dotfiles (`~/.dotfiles/tmux/tmux.conf`,
      falling back to `session-picker.sh` without tower), and use it daily.
- [ ] `/code-review ultra` before the tag (you start it; it is billed).
- [ ] Tag `v0.0.1` (CI's release workflow builds and publishes it).
- [ ] After v0.0.1: delete the prototype branch and worktree
      (`feat/tower-prototype`, `~/.dotfiles.feat-tower-prototype`) and
      `session-picker.sh`.

## Next: the scenario suite over real ssh

The fake ssh (`test/scenario/fakessh`, `fakenet`) is a simulator, and it
has drifted from ssh where it mattered (decisions 41 and 112). It is to
be replaced: on Linux each simulated host is a container running sshd
and tmux, reached over real ssh, its link shaped with `tc` and
`iptables`; on macOS, where CI runners have no Docker, hosts are reached
over localhost sshd. Each phase ends with both shards run and reported.

**Phase 0, development on Linux: done.** The repo is developed on a Linux
machine at `~/projects/tower`. The suite's first Linux runs found five
harness bugs, fixed (a2e8739 to 35fdbfd): the fake ssh's `TERM`, XDG dirs
leaking the user's zoxide database, U02's anchored target, a terminal
that could not answer colour queries (tmux then holds ESC 500ms), U01's
same-second recency tie. LC08 on Linux is the new latency baseline.

- [ ] **Phase 1, spike (go or no-go).** A compose file with two host
      containers (Ubuntu, sshd, tmux, `iproute2`, `iptables`), key auth,
      the home and terminals on this machine. Port two scenarios to it:
      LC08 (hand-off latency by RTT) and one hand-off-heavy LS scenario.
      Measure against their fake-ssh runs: wall time, failures over 10
      runs, hand-off latency per RTT; on Linux (native Docker) and on
      macOS (Docker Desktop). Go when wall time is within about 1.5× of
      the fake's, failures are no more frequent, and latency tracks the
      shaped RTT as the fake's does; else write down why and stop here.
- [ ] **Phase 2, a host backend behind `World`.** Start a host, its ssh
      name, set a fault, reset; scenarios stop writing knobs. Each knob
      gets a real mechanism: delay, jitter and bandwidth `tc netem`;
      `Freeze` and `HalfOpenAt` `iptables` DROP on established
      connections (or `docker pause`); `Drop` killing the connections;
      `Down` stopping sshd (refused), a new host key (hostkey), no
      authorized key (auth), key auth off (password), an unknown name
      (resolve), DROP on SYN (timeout); `Mux` ssh's own ControlMaster.
      No direct equivalent, decided per scenario (rewrite, a unit test
      of how tower reads the error, or drop): `Exit`, `ODelayMs`,
      `Stall`, `Pty`, `tscheck`.
- [ ] **Phase 3, the container backend.** Long-lived containers reset
      between scenarios (test tmux servers killed, `TOWER_HOME` wiped),
      an ssh config and key per world with short ControlPaths, a small
      agent in each container applying faults without a `docker exec`
      per change.
- [ ] **Phase 4, port the families**, one at a time, each passing on
      the container backend before its knob code goes. Worlds that are
      not timing-sensitive run with `t.Parallel`; the timing checks
      become a serial latency tier. Sleeps give way to event waits
      (`WaitMark`, the tracker) where a scenario waits on tower.
- [ ] **Phase 5, the macOS backend.** Localhost sshd, each simulated
      host its own `TOWER_HOME` through `hosts.toml`'s `home`
      (decision 65), faults with `dnctl`/`pf` on `lo0`. Locally it needs
      Remote Login and a test user, which the user sets up.
- [ ] **Phase 6, CI.** Linux container shards on every pull request,
      across tmux 3.2a to 3.7; the macOS shards nightly on `macos-latest`.
- [ ] **Phase 7, remove the fake ssh.** Delete `fakessh` and `fakenet`;
      update [design/harness.md](design/harness.md),
      [scenarios.md](scenarios.md), CLAUDE.md's scenario-suite section,
      and a decision superseding 3, 32 and the fake ssh's parts of 41
      and 112. The real-host scenarios (R) stay as they are.

Two quirks of the current harness on Linux are left for the
replacement:

- [ ] LS01 and LS10 wait for `LOOP-EXIT=137` after a `kill -9`, which the
      terminal prints after its launch command; whether it wraps at the
      110-column edge depends on the length of the run's temp path
      (`/tmp/tsc-<pid>`), and `Term.Screen` does not join wrapped lines.
      They pass on macOS and fail on Linux with 6-digit pids.
- [ ] tmux holds a lone ESC for 500ms while a client waits on a terminal
      query; the scenario terminals now answer colour queries
      (`window-style`), so U01 passes, but any other query a tmux pane
      cannot answer would delay ESC the same way.

## What was built

Fresh code, from the design notes in [design/](design/README.md); the
prototypes were read only for narrow facts (a glyph codepoint, a tmux
quirk, a timing constant). About 22,000 lines of Go and 17,000 of tests,
123 commits from v0.0.0.

| Package | What |
| --- | --- |
| `proto` | wire types: stream messages, local calls, the model |
| `config` | paths per machine and tmux server, identities, `hosts.toml` |
| `tmux` | binaries past version-manager shims, quoting, the control client |
| `stream` | one JSON-lines peer: queued writer, requests, keepalive, stall mark, clock offset |
| `transport` | ssh options, failure classes, probe, control-socket sweep, network watch, `sh -c` for any login shell |
| `client` | calls to towerd; ensure (start, upgrade, replace a wedged one) |
| `towerd` | the daemon: watch, registrations, keys and alert hooks, keeper, remote and home roles, links, loops, hand-off, standby offers, git and zoxide data |
| `relay` | ptys, the byte-exact relay, frame holds between escape sequences |
| `loop` | the attach loop, the attach shim, standby sessions |
| `install` | install on connect: platform, build source, verified upload |
| `dirs` | git state and zoxide dirs, refreshed in the background |
| `hosts` | the host list's edits, naming rules and checks |
| `ui` | the Atlas dashboard (Bubble Tea, no Lip Gloss) |
| `test/scenario` | the acceptance harness, the fake ssh, 92 scenarios |

Docs: [overview.md](overview.md) (product), [protocol.md](protocol.md),
[scenarios.md](scenarios.md), [decisions.md](decisions.md) (101
decisions), a design note per package.

## Suite results

At 0a4213c: every scenario passes. The simulated suite runs in two
shards: `-run '^TestLC'` 8/8 in 330s, `-skip '^TestLC'` 82 passed in 508s
(R01–R03 skip without `TOWER_REAL`). In that run U01 failed once at a
3s screen wait under the full suite's load ("local server" not yet in the
breadcrumb); alone it passed 5 of 5. Recorded, not loosened.

| Family | Scenarios | Result |
| --- | --- | --- |
| Core edge cases | S00–S27, D01 | pass |
| Extras | E01–E04 | pass |
| towerd | V01–V09 | pass |
| Install on connect | I01–I05 | pass |
| Slow and stalled hosts | LH01–LH06 | pass |
| Connections and switches | LC01–LC08 | pass |
| Freshness and dashboards | LV01, LV02, LV04, LD01–LD03 | pass |
| Dashboard data | A01–A05 | pass |
| Dashboard (Atlas) | U01–U08 | pass (U01: one failure under load, see above) |
| Standbys and the relay | LS01–LS09 | pass |
| Real hosts | R01–R03 | pass on `alpha` and `bravo` |

## Latency measured

| What | Measured | Reference (prototype) |
| --- | --- | --- |
| Switch stored → new client, `alpha` → laptop (real ssh) | 11ms | 11ms |
| Switch stored → new client, laptop → `alpha` through its standby | 17–23ms | 17ms |
| ⏎ → target's shim, standby, laptop → remote at RTT 0/50/150/400ms (LC08) | 21/45/96/221ms | 4/30/80/206ms |
| same, remote → remote | 7/57/159/408ms | 2/54/156/410ms |
| `prefix d` gives the terminal back | 4–11ms | about 100ms |
| A change on one remote in another's view, RTT 150ms (LV01) | 133–185ms | 167ms |
| An open dashboard follows the view, RTT 50ms (LV04) | 84ms | about 0.1s |
| ^x row gone from the dashboard (LD01) | 17–28ms | 4–7ms |
| Dashboard popup, first frame (U08) | 23–26ms | – |
| Keystroke through the relay (LS07) | +4–9µs | +7µs |
| 200 MiB `cat` through the relay (LS08) | 205–223 MB/s | 118–136 MB/s |
| Remote towerd up over real ssh (R01) | 0.49s | 0.5s |
| Kill relayed bravo → home → alpha (R01) | 131ms (115ms ssh) | 150ms (120ms ssh) |
| Client back after a wake (R01) | 0.27s | 1.2s |

## Code reviews

| Review | Findings | Resolved |
| --- | --- | --- |
| Step 2, `/code-review xhigh` | 15 | all fixed, each with a test that fails before: a fake `%end` in a capture mismatching every later reply; registrations saved without the lock; reloads rebuilding links; unchanged re-reads pushing states; ssh's reason lost; the last target from the wrong loop; cached ages counted twice; a broken hosts.toml re-read on every view; `XDG_RUNTIME_DIR` shared across homes; ctrl-c killing the loop; a loop stuck without towerd; ctrl-c mid-⏎; standbys spawned under a lock; Linux process lookup; a stray 8.8 MB binary committed (removed, ignored) |
| Step 3, `/code-review high` | 10 | all fixed: fish/csh login shells (`sh -c`, quoting with no backslash); an upload cut short or of another build installed; a platform directory; `current` downgraded; dist checksums overwritten; the retry counter; dirty dev builds sharing a version; macOS `sh` exiting 126 for a missing build |
| Step 4, `/code-review medium` | 5 | all fixed: a picked pane lost on a hand-off; a new window not selected; an add-host spinner left running; dir sessions and duplicates sharing `<name> 2`; every view starting a git/zoxide look |

Also fixed from the real-host runs: standby shims left waiting behind
Tailscale SSH for 12h (standbys now take a heartbeat and exit 30s after
it stops); and from a note of yours: a rebuilt binary of the same version
now replaces the running towerd (V09).

**Rebuild rule.** No file is a near-copy of a prototype file. The step-2
review counted 288 of 23,087 non-trivial lines (1.25%) matching any 80+
character line or 5-line run of a prototype file: import blocks, Go
idioms, tmux format strings, the ssh option list, and in the docs a
diagram and a hosts.toml example taken from the design. The step-4 review
found about 25 key-help strings in `internal/ui/help.go` matching the UI
prototype's help table (the documented key texts); every other file
shares 0–6 lines.

## Blocked

Nothing.

## Open

- `?` and `-`/`.` type into the finder when its query is not empty.
- Views and states are sent whole; dirs are more than half of each (a
  22 KB state with 200 dirs). Deltas would pay off past about 100 KB.
- A dashboard opening costs up to about 1s of CPU on a host with many
  zoxide repos (a git status each). Reading only `HEAD` for zoxide repos
  would make it nearly free, without their dirty marker.
- Git state follows a session's starting directory, not where its panes
  have moved since.
- `~^Z` does nothing in a relayed session (its ssh leads a session of its
  own); `suspend-client` and ctrl-z behave as with plain ssh.
- A local attach's frame hold can still land inside an escape sequence
  while tmux floods the terminal (only relayed sessions order the writes).
- The new pty and Linux code paths of the relay ran on Linux only in CI
  and on `alpha`/`bravo` through the real-host scenarios.

## How the work was done

Main session: the docs, the design notes, `proto`, `config`, `tmux`,
`stream`, `transport`, `client`, the harness and fake ssh, the real-host
scenarios, the review fixes after step 2's towerd findings and steps 3
and 4, and the merges. Agents (Opus, each in a worktree, merged
fast-forward after review): `towerd`; `relay` then the loop and standbys;
the step-2 picker then the dashboard scenarios; install on connect; step
4's data sources; the Atlas UI. One agent stalled for about 3 hours
waiting on a background run that had ended; `CLAUDE.md` now has the suite
run in foreground shards and agents never end a turn with a run going.
