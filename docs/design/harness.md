# The scenario harness

`test/scenario` is the acceptance suite. It runs only on request
(`TOWER_SCENARIOS=1`, which `mise run scenarios` sets with an isolated
`TMUX_TMPDIR`, `TOWER_TEST_DIR` and `TMPDIR`), on any backend: the
fake ssh; host containers over real ssh (`TOWER_HOSTS=container`); or,
macOS's, hosts of this machine behind sshds of their own over real ssh
(`TOWER_HOSTS=sshd`).

## Tiers

A scenario that measures or bounds a time runs alone: the serial tier
(LC, LH, LV, LD, LS08, U08, A01, S15, S20, S23, `TestHarnessLinks`, the
R family), which `go test` runs first. Every other scenario calls
`parallel(t)` first thing and runs, once the serial tier is done,
alongside the others: `TOWER_PARALLEL` worlds at a time (4 by default),
as `-test.parallel`, which `TestMain` sets unless the command line gives
it. A parallel test's time is counted from its turn. Nothing a world
makes is shared with another (its directory, tmux sockets, ssh config,
containers), and the sweeps match a world's own directory with its
trailing `/`, so `i04` does not reach `i04-dev`.

## Pieces

| File | Holds |
| --- | --- |
| `main_test.go` | builds `tower` at `0.0.1-test`, `tower-v2` at `0.0.2-test`, `tower-dev` at `0.0.3-dev+test` (a development build, no release) and the fake ssh into `$TOWER_TEST_DIR/bin`; with `TOWER_RACE=1` tower is built with the race detector, every process writes its reports under the world's `race/`, and a report fails the scenario at teardown |
| `world_test.go` | `World` (one scenario), `Host` (one simulated machine and tmux server), homes, waits on links and loops, timing marks |
| `term_test.go` | `Term`: a terminal running the attach loop, driven with `send-keys`, read with `capture-pane` |
| `teardown_test.go` | ends everything and fails the test if a tmux server outlives its sessions |
| `towerd_test.go` | towerd-side helpers (pids, live homes, registrations, `view` and `act` calls, other binaries) and `FakeLoop` |
| `fakenet/` | the knobs contract between the harness and the fake ssh |
| `fakessh/` | the fake ssh |
| `link_test.go` | links and faults: the World's calls, put in place as the fake's knobs or by the container backend |
| `links_check_test.go` | `TestHarnessLinks`: each link setting and fault checked on either backend |
| `tiers_test.go` | the serial and parallel tiers |
| `container_test.go`, `hosts/`, `hostagent/` | the container backend: host containers reached over real ssh (`TOWER_HOSTS=container`), and the agent in each |
| `sshd_test.go` | the sshd backend: hosts of this machine behind sshds of their own (`TOWER_HOSTS=sshd`), lo0's rules through pf on macOS |
| `procs_*_test.go` | this user's processes, per platform: argv and environment, and the process tree |
| `sshcall/` | an ssh command line read as ssh does, and the call log (`fake/ssh.log`) both ssh stand-ins write |
| `sshwrap/` | ssh for hosts over real ssh: logs the call, then runs real ssh with the world's config |
| `load_test.go` | load programs run on a host or beside a terminal (flood, build log, terminal reader): the suite's binary with `SCENARIO_HELPER` set |
| `s*_test.go` … | the scenarios, one file per family (`s_core`, `s_links`, `s_data`, `s_handoff`, `s_loop`, `dash`, `e_extras`, `v_towerd`, `i_install`, `lh`, `lv`, `lc`, `ld`, `ls`, `a_atlas`, `u_dash`, `r`, …) |

## Hosts

`w.Host(name, sessions, opts…)` makes a tmux server `-L tt-<id>-<name>`
with a generated config (`exit-empty on`, `/bin/sh`, `base-index`,
`detach-on-destroy no-detached`, `M-o` running the dashboard in a full-size
popup, `M-l` running `tower last`) and registers the ssh name `name` for
it (`w.SSH`, below). A host's processes run with:

- `HOME` and `XDG_CONFIG_HOME` in an empty directory, so tmux servers tower
  starts read no user config;
- `TOWER_HOME=<dir>/home-<machine>`, `TOWER_MACHINE_ID=<machine>` (a
  container's own `/etc/machine-id` for a container host),
  `TOWER_TMUX=-L <sock>`;
- `TOWER_SSH=<the fake ssh>` (`sshwrap` over real ssh),
  `TOWER_FAKE_DIR=<dir>/fake`;
- `TOWER_TEST_TIMING=<dir>/marks` (timing marks, `config.Mark`),
  `TOWER_TEST_NAME=<host>`;
- the test timings below, then the host's own extras.

Options: `Machine(m)` (several servers on one machine), `HomeName(n)` (a
shared home directory), `BaseIndex(n)`, `Env(k, v)`, `Platform(uname)` (a
`uname` shim first on the host's PATH answers `uname -s -m` with another
platform, for install on connect), `Zoxide(dirs…)` (a zoxide first on the
host's PATH lists these, `~` for the world's home directory) and
`MountTable(table)` (the host's mount table, in `/proc/self/mounts`
format, through `TOWER_TEST_MOUNTS`). zoxide's own `_ZO_*` variables are
dropped, so a host without a fake zoxide reads the empty database of the
world's home directory, never the user's. `w.repo(rel)` makes a git repo
with one commit under the world's home directory, without the user's git
configuration.

Every machine has its own install root, `TOWER_INSTALL_DIR=<dir>/install-<machine>`
(`h.InstallDir()`). `h.Remote()` pins the built binary (`tower =`);
`h.Unpinned()` leaves it out, so the home installs its build there.

## Homes and links

`w.Home(h, remotes…)` writes `h`'s `hosts.toml` and starts `tower towerd`
there (or sends `reload` to a towerd a bridge already started, which
activates the home role). `h.Remote()` is the `hosts.toml` entry for a
simulated host: its fake ssh name, its tmux socket, the built binary.
`w.WaitLink`, `w.WaitUp` and `w.WaitLoop` poll the home's full status.

## Driving towerd directly

Scenarios that test towerd's side of a behaviour whose other side is the
attach loop or the dashboard call towerd as those would:

- `h.View(client)` and `h.Act(client, req, ms)` are the dashboard's `view`
  and `act` calls on `h`'s towerd, as the client `pid:created:name`
  (`h.ClientIDs(session)`), with a deadline `ms` from now;
- `w.FakeLoop(home)` beats every 2s like a loop and makes its attaches
  with `Prepare`, running the prepared argv (the real attach shim, over the
  fake ssh for a remote) in a terminal of its own; `Attach` ends the
  previous attach's terminal first, as a loop ends its old client.
  `WaitSwitch`, `Held` and `After` are the loop's other calls;
- `h.TowerdPid()` (0 when none runs; `Kill9` and `Signal` leave 0
  alone, which kill(2) would take for the test's own group), `h.LiveHomes()`, `h.Regs()`, `h.HiddenSessions()`,
  `h.TowerdProcs()` and `h.TowerBin(bin, args…)` (another build, for
  upgrades).

The parts of a scenario that need the loop or the dashboard are marked
`// loop part:` and come with them.

Family timings: the slow-host family (LH) runs with `TOWER_SILENCE=15000`
and `TOWER_TEST_PAD=30000` (states and views padded to 30 KB, so a stalled
pipe fills); the connection family (LC) and LH06 with `ProductionTimings`.

Recoveries (LC03, LC04, LC06: a fault, the network's return or a pick,
until the client is back) are checked in three parts (`checkRecoveries`),
since a new connection costs what the ssh at the other end makes it
cost: the fake's about 6.5 round trips, real ssh's 13. The connection,
from the link's last attempt to the link up (ssh, tower started there,
the hello, the first state), is at most 14 round trips past R0's in the
same world, counted at RTT 150 and 400 and only for a connection begun
within the recovery. Tower's part, the rest, is within the scenario's
limit less 6.5 round trips: the time the limit left tower on the fake.
The client is back within 1s and 4 round trips of its link (of the last
pick, in LC06). The tracker gives each part (`lastattempt`).

## Terminals

`w.Loop(name, home, extra)` starts the attach loop in a terminal of its
own (`-L tt-<id>-term-<name>`, 110×32). `t.Pick(query)` types a query into
the dashboard and presses Enter; `t.DashTo(query)` opens the dashboard with
`M-o` first. `t.Screen()` joins wrapped lines (`capture-pane -J`), so
what a line says does not depend on where it wraps. The dashboard's
prompt is `sessions>`. `t.Until(re, present,
d)` polls the screen every 3ms (the dashboard's timings), `t.CloseDash()`
closes an open popup, and `rowRe(host, name)` matches a session's row.
`h.UI(client, extra, args…)` runs `tower _ui …` on a host as the dashboard
of a client would (`TMUX` and `TOWER_CLIENT` set).

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
`TOWER_TEST_FUTURE` (a remote also sends an unknown message type),
`TOWER_TEST_MOUNTS` (a mount table file in place of the machine's),
`TOWER_DIRS_EVERY` and `TOWER_LOOK_EVERY` (the git and zoxide refreshes).

Timing marks the suite reads: `prepare`, `prepared <host>`, `attach`,
`attach: standby`, `attach: session`, `standby did not answer`, `exited
<code>`, `after <do>`, `switch stored: hold`, `end attach`, `home sees the
new client`, `shim: exec tmux`, `dash: enter`, `dash: kill`, `dash: kill
answered` (LD01: a row goes before its answer), `standby: start <host>`,
`standby: ready <host>`, `standby: go`, `standby: taken`, `standby: drop
<host>`.

## Links and faults

Scenarios shape links and set faults per ssh name through the World,
never the fake's knobs; `w.SSH(alias, host)` registers another name for
a host. Each host's backend puts them in place: the fake ssh's knobs for
a host on this machine, real mechanisms for a container or for a host
behind sshds of its own.

| Call | The fake ssh | A host container | A host behind sshds |
| --- | --- | --- | --- |
| `w.Shape(alias, f)`: `DelayMs`, `JitterMs`, `BwKBps` | delayed, paced chunks | `tc netem` delay, jitter and rate on `eth0` and, through `ifb0`, its ingress | dummynet pipes on lo0 for the ports of the machine's names, one each way; with jitter, five each way (below) |
| `Shape`: `WindowKB` | bytes in flight per direction | none: ssh's channel window is 2 MB, so a stalled pipe needs `Stall` alone (`lhWindow`: LH, LS09); any other use fails the scenario | none, as on a container |
| `Shape`: `Pty`, `Mux` | a pty for `ssh -t`; a shared master | always: ssh's own | always |
| `w.Freeze(alias, on)` | new connections hang; sessions off a master give up after the alive window (those on a master ride it out) | `iptables` DROP of the name's port, both ways (`TT-FREEZE-IN` by destination port, `TT-FREEZE-OUT` by source port): every connection gives up, the far side never hears | pf `block drop` of the name's port, both ways |
| `w.NetworkChange(alias, at)` | masters made before `at` dead from `at` | DROP, both ways and for good, of each connection established now (`TT-HALFOPEN`); `at` not in the future | pf `block drop`, both ways and for good, of each connection to the machine's ports established now (`netstat`) |
| `w.Stall(alias, on)` | no byte moves; ssh never gives up | the processes the name's sessions run there (their `SSH_CONNECTION` names its port) stopped by the agent, and new ones as they come, every 20ms; sshd still answers, other names' sessions run on; a daemon a session started (a towerd, below) runs on | the processes under the name's sshd's connections, not sshd's own nor a daemon's, stopped by the harness, and new ones as they come, every 20ms |
| `w.Drop(alias)` | live connections closed | the sshd processes holding the name's port's connections (`ss -p`) killed | the name's sshd's connection processes killed |
| `w.Down(alias, how)` | ssh's message for `how` | per name in the world's ssh config: `refused` port 1, `timeout` port 2222 (SYNs dropped), `password` port 2223 (an sshd with key auth off), `resolve` an `.invalid` name, `hostkey` a known_hosts with another key, `auth` a key no host authorizes; `tscheck` none | as on a container, `timeout` a port of the name's whose SYNs pf drops, `password` the name's sshd's second port, key auth off there |
| `w.ExitWith(alias, code)`, `w.SlowControl(alias, ms)` | forced exit status; a slow `ssh -O` | none (fails the scenario): exit 255 is a `Drop`; 42 and 43 from ssh itself are fake-only steps (S06 (c), LS09 3–4), towerd's reading of them unit-tested; a wedged master is the home's real one stopped (`w.masterPid`, LH05) | none, as on a container |
| `w.Heal(alias)`, `w.Reset(alias)` | faults off (and the link unshaped) | the same; a network change's connections stay dead | the same |

On a container the shape and a network change belong to the container,
whichever of its names sets them, as a machine's network does; a
freeze, a stall, a drop and `Down` belong to the name (its port); behind
sshds, likewise, the machine's names' ports and the name's. A stall
leaves out a daemon a session started, and what runs under it: a
process leading a process group of its own whose parent is no sshd, as
the towerd a bridge starts when it finds none. It serves every name, as
on a machine of its own, and a stalled network never stops it; LH04 has
the stalled name's bridge start it.
`TestHarnessLinks` checks each row on every backend: a session on a
master at RTT 100ms takes 205–208ms (221ms on the fake on a macOS
runner, 265ms behind sshds there), 512 KiB at 256 KB/s 1.9–2.2s, each
`Down` reads as tower expects, a drop or a freeze ends a session (the
frozen one after the alive window, 15s on the fake, 18–20s over real
ssh, its far side still running), a network change leaves the old
master to give up and a new one working, a stall stops output for the
alive window and more without ssh giving up, the other name's lines
coming every second.

Over real ssh a session riding a master that dies hears nothing: its
ssh exits 255 with stderr empty, so tower reads "ssh exited 255"; off a
master (or from the fake) it reads "connection lost" or "connection
timed out".

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
6 round trips for a new one, 2 for a session on it; a session rides an
existing master whatever its `ControlMaster`, `no` only declines to make
one, as with ssh; `-O exit` ends its sessions) and `halfopen_at` (a network change: masters made before it are
dead and give up after the alive window).

## Host containers

With `TOWER_HOSTS=container` a host made with `SSHHost()` is a container
running sshd and tmux, reached over real ssh; on the fake backend, or for a host
made without `SSHHost()`, the fake ssh as before. Every scenario's
remote hosts are `SSHHost()`; homes stay on this machine. What only the
fake can do stays in fake-only steps (`if !w.real`): tower's own exit
codes from ssh itself, the Tailscale check (S15 `ts`, `ts2`).

- `hosts/` holds the image (Ubuntu 24.04, OpenSSH, tmux built from
  source at `TMUX_VERSION`, 3.7c by default, `iproute2`, `iptables`) and a
  compose file: a pool of `TT_HOSTS` hosts (by default 4 for each world
  that runs at once, the most a world takes: S18), `tt-<run>-host-<n>`.
  The image has git, for the dashboard's repo data. `TestMain` builds the
  image (`tt-scenario-host`, from the build cache after the first build;
  `TT_TMUX_VERSION` picks tmux) under the run's own tag, so a run building
  at once from other sources or another tmux starts its own; it takes
  down earlier runs' projects, and untags their images, where the run's
  process is gone (a `go test` timeout skips teardown), brings the
  project up (`tt-<run>`, the pid and a nonce) and down at the end, and
  untags its image. Containers, network and image carry the label
  `tower-test`, and `tower-test.owner`: the machine, user and pid
  namespace of the run. A sweep judges only its owner's runs, and
  another user's process (`EPERM`) runs: a daemon can serve other users,
  and a devcontainer has pids of its own.
- The run's `TOWER_TEST_DIR`, `TMUX_TMPDIR` and `TMPDIR` are bind-mounted
  at their own paths and the container user `tt` has the test user's
  uid, so a container host's `TOWER_HOME`, timing marks and tmux and
  towerd sockets are where the harness looks; only ssh crosses the
  network. `h.Tmux` starts a container host's server in the container
  (any command with `-f`, through the agent, by tmux's path there) and
  reaches it through its socket otherwise; `h.Run` runs any of the host's
  commands there (towerd started detached), and a terminal on a
  container host is a `docker exec -it` as `tt`, `LANG` and `LC_*`
  passed.
- Each container has pids of its own, as a machine has, with tini as
  pid 1 to reap what daemons leave. This machine still sees its
  processes, under other pids: a pid read on a container host (a pid
  file, a tmux client) is translated before the harness signals it
  (`h.HostPid`, from `NSpid` in `/proc`; `h.TowerdPid` and teardown do).
  The sweeps by `TOWER_HOME` and by control path see container
  processes as any other; `h.SplitFar` sets a container's apart (a far
  side real ssh strands, LS03). The agent has `SYS_PTRACE` to read a
  session process's environment and `ss -p` across users.
- An agent in each container (`hostagent`, root) serves the harness on
  `$TOWER_TEST_DIR/slots/<hostname>.sock`, owned by the test user, one
  JSON call per connection: `assign` (the world's environment file,
  then `reset`), `reset` (no stall, no shaping, no faults, every process
  of `tt` killed), `exec` (a command as `tt` through `tt-run`, or as
  root), `netem`, `freeze`, `halfopen`, `drop`, `stall`. It sees only its
  container's processes, so whatever it signals is the container's; a
  change costs a unix-socket call, not a `docker exec`.
- A world takes a free container for each machine of its `SSHHost()`
  hosts (`assign`; hosts with one `Machine` share it, waiting while the
  pool is taken); teardown, once its own checks are done, resets it
  (`release`) for the next world. Each container writes a random
  machine id at start, which its hosts' `TOWER_MACHINE_ID` reports.
- sshd takes key auth only and runs every command through `tt-run`
  (`ForceCommand`), which sources the environment of the host the ssh
  name stands for: `<world>/ctr-<hostname>/<port>.env`, picked by the
  server port in `SSH_CONNECTION`, in the directory `assign` wrote to
  `/etc/tt-env-dir`; then `sh -c` the command, as sshd runs it.
  Everything else is sshd's default, `MaxSessions` included.
- On the container backend every host of every world has
  `TOWER_SSH=sshwrap` from the world's start (`w.real`), so a tmux
  server started before the first container host has it too. It logs
  the call to `fake/ssh.log` as the fake
  does (`w.SSHLog` reads both) and runs `ssh -F $TOWER_TEST_SSH_CONFIG`:
  the world's names to the containers' addresses, the run's keys and
  known hosts, neither the user's nor the system's config. One key per
  run serves every world (each world's names, config and control
  masters are its own); the control paths are tower's own, short
  enough. `w.masterPid(home, alias)` is a home's control master
  (`ssh -O check`).
- Each ssh name of a container gets a port of its own (sshd listens on
  22 and 2201–2215, IPv4 only, its limit of 16 sockets): ssh keys a
  control master by host, port and user, so names of one host would
  otherwise share one.
- Nothing signals a process by name: the agent signals pids from its
  own `/proc`, the harness translated pids.

## Hosts behind sshds

With `TOWER_HOSTS=sshd` a host made with `SSHHost()` is a tmux server of
this machine, as with the fake ssh, reached over real ssh: the backend
for macOS, which runs no containers of its own. It needs neither Remote
Login nor an account: the harness runs every sshd as the test user.

- Each ssh name of such a host has an sshd of its own (`/usr/sbin/sshd
  -D -e`, its config and log in `<world>/sshd-<name>/`) on 127.0.0.1, at
  two free ports, the second with key auth off (`Down password`), and a
  third where nothing listens (`Down timeout`). `UsePAM no`,
  `StrictModes no`, `AllowUsers` the test user; every command runs
  through `sshd-run` (`ForceCommand`), which sources the host's
  environment as the name was registered (`<world>/sshd-<name>/env`,
  sshd's own variables, `TERM` and `SSH_*`, left as sshd sets them),
  then `sh -c` the command.
- One host key and one client key per run. Every name's ssh config has
  `HostKeyAlias tt-sshd`, so one known_hosts line holds for every port.
- The world's ssh config, `sshwrap` and `w.real` work as on the
  container backend. Pids are this machine's (`h.HostPid` is the
  identity); `h.SplitFar` sets apart what runs under the host's sshds.
- On macOS lo0's rules are pf's: one anchor per run under `com.apple`
  (`com.apple/tt-<pid>`, which macOS's main ruleset evaluates) holds
  every world's dummynet and filter rules, loaded whole on each change;
  a run's pipes are a block of 1000 numbers no pipe is in yet (from one
  of its pid's), and a pipe given back is used again. `TestMain` takes a
  reference on pf (`pfctl -E`), flushes the anchors of earlier runs whose
  process is gone (another user's stay), checks a 50ms pipe each way
  gives a 90–200ms round trip, and at
  the end flushes the anchor, deletes its pipes and gives the reference
  back (the empty anchor stays listed). All through `sudo -n`, which
  GitHub's macOS runners have. Elsewhere (Linux, for working on the
  backend) a scenario that needs a shape, a freeze, a network change or
  `Down timeout` is skipped.
- dummynet has no jitter: a jittered link sends each packet through one
  of five pipes each way, of delays spread across ±jitter, picked at
  random (pf's `probability`), so packets reorder as with netem; a
  bandwidth is split among the five. At 50±20ms each way a ping-pong
  ran 59–270ms, median 121ms, where netem's spread is 60–140ms.
- On macOS a stopped process whose terminal hangs up stays stopped
  (Linux continues it): a far side stalled when its connection ends goes
  only once the stall does (LS03).
- On macOS a blocking write to a pty can come back short, part of it
  taken. LS08's build-log helper, a write a line, once dropped the rest,
  so its 22.8 MB arrived a few bytes short, plain and relayed; it now
  writes on, as `cat` and Go's own writes do. LS08's reader gives up
  after 10s without a byte, so bytes lost on the way fail the run
  instead of hanging it.
- Teardown stops the world's sshds first, killing their connections'
  processes (found under the sshd), then takes the world's rules and
  pipes off lo0.

## Teardown

Terminals first (a live loop would restart a towerd), then every towerd
(SIGCONT, SIGTERM, SIGKILL after 5s), the holders, every session of every
server; each server must exit within 10s, or the test fails. Leftover
processes with the world's `TOWER_HOME`, or naming the world's directory
(`pkill -f <dir>/`), are killed. The world's directory
is removed when the test passed. Over real ssh, the home's control
masters are ended by their control path: ssh's `[mux]` process title
overwrites their environment.
