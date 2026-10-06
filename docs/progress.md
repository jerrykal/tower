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

- [x] **Phase 1, spike (go or no-go).** A compose file with two host
      containers (Ubuntu, sshd, tmux, `iproute2`, `iptables`), key auth,
      the home and terminals on this machine. Port two scenarios to it:
      LC08 (hand-off latency by RTT) and one hand-off-heavy LS scenario.
      Measure against their fake-ssh runs: wall time, failures over 10
      runs, hand-off latency per RTT; on Linux (native Docker) and on
      macOS (Docker Desktop). Go when wall time is within about 1.5× of
      the fake's, failures are no more frequent, and latency tracks the
      shaped RTT as the fake's does; else write down why and stop here.

      Built (155cfe7): `test/scenario/hosts/` (image with tmux 3.7c from
      source, compose file, sshd's `ForceCommand` wrapper), the backend
      in `container_test.go` (decision 113, design in
      [design/harness.md](design/harness.md#host-containers)); LC08 and
      LS10 make B and C with `SSHHost()`. Measured on Linux, Docker 28,
      each scenario alone, 10 runs per backend:

      | | fake ssh | containers |
      | --- | --- | --- |
      | LC08 wall (test) | 139.4s (138.0s) | 147.4s (144.3s): 1.06× |
      | LS10 wall (test) | 32.8–33.9s (31.4–32.4s) | 8.8s (6.3s) |
      | failures | LC08 0/10, LS10 0/10 | LC08 0/10, LS10 0/10 |

      A container run adds about 3s per `go test` (compose up and down,
      the image cached; its first build takes 34s) and about 1.5s per
      world (a `docker exec` per tmux server and link change). LS10 is
      shorter because over real ssh the killed loop's client goes with
      its ssh at once, where the fake keeps the session and the home
      detaches it 28s later (below). LC08's medians match the fake's to
      the millisecond through a standby and for the hops that reuse a
      session, at every RTT; the hops that open a new session on the
      master (dashboard and eager modes, to a remote) take 3–5ms more,
      the real channel open, sshd's fork and pty (ms, fake / containers):

      | RTT | standby r→r, r→l, l→r | eager r→r, l→r | dashboard r→r, l→r |
      | --- | --- | --- | --- |
      | 0 | 2/2, 3/3, 4/4 | 6/10, 7/12 | 8/12, 8/11 |
      | 50 | 52/52, 29/28, 29/29 | 107/110, 83/87 | 160/163, 83/87 |
      | 150 | 152/152, 79/79, 79/79 | 307/310, 233/237 | 461/464, 233/237 |
      | 400 | 402/402, 204/204, 204/204 | 807/811, 608/612 | 1212/1214, 609/612 |

      Found on the way: real OpenSSH 9.6 at both ends ends a session when
      its mux client is killed (checked by hand with `ssh -tt … sleep`
      over a master, too), unlike the fake and decision 112's
      observation on `bravo` and `charlie`; LS10's "still there 2s later"
      now holds on the fake only. ssh's `[mux]` title overwrites its
      environment, so teardown ends real masters by control path.

      **Not measured: macOS (Docker Desktop).** This machine is Linux.
      The design leans on native Docker: sockets through bind mounts,
      shared pids and routable container addresses do not cross Docker
      Desktop's VM, so on macOS the backend would need a `docker exec`
      per host command and published ports. The recommendation is go on
      Linux, with macOS left to Phase 5's localhost sshd.

      **Decided: go** on Linux; macOS through Phase 5, not Docker
      Desktop.

      Shards at the end (fake ssh, as before): `^TestLC` 8/8 in 315s; the
      rest 83 passed, 4 skipped (R01–R04, without `TOWER_REAL`) in 498s, LS03 failing once under load (no loop on
      `^B:bravo` within 10s after B's towerd was killed); alone it
      passed 3 of 3. Recorded, not loosened.
- [x] **Phase 2, a host backend behind `World`.** Start a host, its ssh
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

      Built (c7c53cc to 78f251d, decision 114, the table in
      [design/harness.md](design/harness.md#links-and-faults)): scenarios
      call `w.Shape`, `Freeze`, `NetworkChange`, `Stall`, `Drop`, `Down`,
      `Heal` and `Reset`, and no longer touch the knobs; the container
      backend puts each in place with a real mechanism. `Stall` got one
      too: the session processes stopped while sshd answers. `Freeze`
      and `NetworkChange` are `iptables` chains; `Down` lives in the
      world's ssh config, per name, so each name has a port of its own.
      `TestHarnessLinks` passes on both backends (59s fake, 69s
      containers). Shards on the fake: `^TestLC` 8/8 in 315s, the rest
      85 passed, 4 skipped (R01–R04), 0 failed in 555s.

      Decided per scenario, done in Phase 4:
      - `Exit` 255 (LS09 step 2) becomes a `Drop`, which makes real ssh
        exit 255. 42 and 43 (S-handoff (c), LS09 steps 3–4) are tower's
        own codes, read by towerd's `after`, unit-tested in
        `towerd/daemon_test.go`; those steps stay on the fake only.
      - `ODelayMs` (LH05, a wedged master) becomes the home's real
        master for W stopped (`SIGSTOP`), found by its control path.
      - `Pty`, `Mux`: always on over real ssh. Scenarios that ran
        without them on the fake get both; whether any relied on their
        absence is checked as each family is ported.
      - `tscheck` (S15 `ts`, `ts2`): dropped from the container run;
        `transport` unit-tests the reading of Tailscale's check.
      - `WindowKB` (LH02–LH04, LS09): no mechanism, since real ssh's
        channel window is 2 MB. Either the LH family pads past it, or
        its stalls rely on `Stall` alone; measured when it is ported.

      Found on the way, for Phase 4:
      - Over real ssh, a session on a master that dies gets exit 255
        and an empty stderr, so tower shows "ssh exited 255" where the
        fake gave "connection lost" or "connection timed out".
      - Real ssh gives up a frozen link after 18–20s, not 15s
        (ServerAlive 5s × 3 counts from the last traffic); LC03's 8s
        and 12s limits are to be measured over real ssh.
      - The fake's freeze spares sessions on a master. Real ones die,
        so LC04 (freeze and network change together) behaves the same
        either way.
      - The containers share the machine's pids (decision 113), so
        faults signal exact, cgroup-checked pids, never by name.
        Whether Phase 3 gives each container its own pid namespace
        (the sweeps and the stall reading pids through the container
        agent) is to be decided there.
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

- [x] LS01 and LS10 wait for `LOOP-EXIT=137` after a `kill -9`, which the
      terminal prints after its launch command; whether it wraps at the
      110-column edge depends on the length of the run's temp path
      (`/tmp/tsc-<pid>`), and `Term.Screen` does not join wrapped lines.
      They pass on macOS and fail on Linux with 6-digit pids. Fixed
      (56f805d): `Term.Screen` joins wrapped lines (`capture-pane -J`);
      both pass on Linux.
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
