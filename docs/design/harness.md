# The scenario harness

`test/scenario` is the acceptance suite. It runs only on request
(`TOWER_SCENARIOS=1`, which `mise run scenarios` sets with an isolated
`TMUX_TMPDIR`, `TOWER_TEST_DIR` and `TMPDIR`).

## Pieces

| File | Holds |
| --- | --- |
| `main_test.go` | builds `tower` at `0.0.1-test`, `tower-v2` at `0.0.2-test` and the fake ssh into `$TOWER_TEST_DIR/bin` |
| `world_test.go` | `World` (one scenario), `Host` (one simulated machine and tmux server), homes, waits on links and loops, timing marks |
| `term_test.go` | `Term`: a terminal running the attach loop, driven with `send-keys`, read with `capture-pane` |
| `teardown_test.go` | ends everything and fails the test if a tmux server outlives its sessions |
| `fakenet/` | the knobs contract between the harness and the fake ssh |
| `fakessh/` | the fake ssh |
| `load_test.go` | load programs run on a host or beside a terminal (flood, build log, terminal reader): the suite's binary with `SCENARIO_HELPER` set |
| `s*_test.go` … | the scenarios, one file per family |

## Hosts

`w.Host(name, sessions, opts…)` makes a tmux server `-L tt-<id>-<name>`
with a generated config (`exit-empty on`, `/bin/sh`, `base-index`,
`detach-on-destroy no-detached`, `M-o` running the dashboard in a full-size
popup, `M-l` running `tower last`) and registers the fake ssh name `name`
for it. A host's processes run with:

- `HOME` and `XDG_CONFIG_HOME` in an empty directory, so tmux servers tower
  starts read no user config;
- `TOWER_HOME=<dir>/home-<machine>`, `TOWER_MACHINE_ID=<machine>`,
  `TOWER_TMUX=-L <sock>`;
- `TOWER_SSH=<the fake ssh>`, `TOWER_FAKE_DIR=<dir>/fake`;
- `TOWER_TEST_TIMING=<dir>/marks` (timing marks, `config.Mark`),
  `TOWER_TEST_NAME=<host>`;
- the test timings below, then the host's own extras.

Options: `Machine(m)` (several servers on one machine), `HomeName(n)` (a
shared home directory), `BaseIndex(n)`, `Env(k, v)`.

## Homes and links

`w.Home(h, remotes…)` writes `h`'s `hosts.toml` and starts `tower towerd`
there (or sends `reload` to a towerd a bridge already started, which
activates the home role). `h.Remote()` is the `hosts.toml` entry for a
simulated host: its fake ssh name, its tmux socket, the built binary.
`w.WaitLink`, `w.WaitUp` and `w.WaitLoop` poll the home's full status.

## Terminals

`w.Loop(name, home, extra)` starts the attach loop in a terminal of its
own (`-L tt-<id>-term-<name>`, 110×32). `t.Pick(query)` types a query into
the dashboard and presses Enter; `t.DashTo(query)` opens the dashboard with
`M-o` first. The dashboard's prompt is `sessions>`.

## Knobs for tower (test timings)

| Variable | Test value | Meaning |
| --- | --- | --- |
| `TOWER_PING` | 1000 | stream ping interval (ms) |
| `TOWER_SILENCE` | 2000 | the home gives a silent stream up; the remote waits 2× + 5s |
| `TOWER_BACKOFF_BASE`, `TOWER_BACKOFF_CAP` | 200, 3000 | link reconnect backoff |
| `TOWER_STABLE` | 3000 | up this long resets the backoff |
| `TOWER_NOSRV_POLL` | 400 | towerd's poll for a tmux server |
| `TOWER_IDLE` | 0 | towerd's idle exit; 0 never |
| `TOWER_TEST_PICKER` | 1 | the loop starts at the picker, not the last target |
| `TOWER_TEST_HOOKS` | 1 | allows the `plant` call |

Scenario-specific: `TOWER_HANDOFF_TTL`, `TOWER_ACK_TIMEOUT` (a dashboard
request's deadline), `TOWER_TEST_PAD` (pad states and views to n bytes),
`TOWER_TEST_SKEW` (ms added to the clock), `TOWER_TEST_NOTTY` (the
dashboard cannot write its client's tty), `TOWER_TEST_CRASH=after-switch`
(the dashboard dies once its switch is stored), `TOWER_TEST_GEN` (the
dashboard claims that attach generation), `TOWER_TEST_PROTO=lo-hi`,
`TOWER_TEST_FUTURE` (a remote also sends an unknown message type).

Timing marks the suite reads: `prepare`, `prepared <host>`, `attach`,
`attach: standby`, `attach: session`, `standby did not answer`, `exited
<code>`, `after <do>`, `switch stored: hold`, `end attach`, `home sees the
new client`, `shim: exec tmux`, `dash: enter`, `standby: start <host>`,
`standby: ready <host>`, `standby: go`, `standby: taken`, `standby: drop
<host>`.

## The fake ssh

Parses options like ssh (the first `-o` value wins), logs every call as a
JSON line in `fake/ssh.log`, and runs the remote command with the target
host's environment. Knobs per ssh name (`fakenet.Knobs`): `down`
(refused, hostkey, auth, password, resolve, timeout, tscheck, with ssh's
messages), `latency_ms`, `exit`, `freeze` (half-open; gives up after
ServerAliveInterval × ServerAliveCountMax, the remote side kept alive by a
holder process), `drop`, `o_delay_ms`, `delay_ms`, `jitter_ms`, `bw_kbps`,
`window_kb`, `stall` (no byte moves, never given up), `pty` (`ssh -t` gets
a pty on the host and the link's delays), `mux` (a shared control master:
6 round trips for a new one, 2 for a session on it; `-O exit` ends its
sessions) and `halfopen_at` (a network change: masters made before it are
dead and give up after the alive window).

## Teardown

Terminals first (a live loop would restart a towerd), then every towerd
(SIGCONT, SIGTERM, SIGKILL after 5s), the holders, every session of every
server; each server must exit within 10s, or the test fails. Leftover
processes with the world's `TOWER_HOME` are killed. The world's directory
is removed when the test passed.
