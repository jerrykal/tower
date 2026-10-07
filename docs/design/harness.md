# The scenario harness

`test/scenario` is the acceptance suite. It runs only on request
(`TOWER_SCENARIOS=1`, which `mise run scenarios` sets with an isolated
`TMUX_TMPDIR`, `TOWER_TEST_DIR` and `TMPDIR`). Its remote hosts are
reached over real ssh: on Linux host containers (`TOWER_HOSTS=container`,
the default there), on macOS hosts of this machine behind sshds of their
own (`TOWER_HOSTS=sshd`, the default there).

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
| `main_test.go` | builds `tower` at `0.0.1-test`, `tower-v2` at `0.0.2-test`, `tower-dev` at `0.0.3-dev+test` (a development build, no release), `sshwrap` and the container agent into `$TOWER_TEST_DIR/bin`; with `TOWER_RACE=1` tower is built with the race detector, every process writes its reports under the world's `race/`, and a report fails the scenario at teardown |
| `world_test.go` | `World` (one scenario), `Host` (one simulated machine and tmux server), homes, waits on links and loops, timing marks |
| `term_test.go` | `Term`: a terminal running the attach loop, driven with `send-keys`, read with `capture-pane` |
| `teardown_test.go` | ends everything and fails the test if a tmux server outlives its sessions |
| `towerd_test.go` | towerd-side helpers (pids, live homes, registrations, `view` and `act` calls, other binaries) and `FakeLoop` |
| `link_test.go` | links and faults: the World's calls, put in place by the backend |
| `links_check_test.go` | `TestHarnessLinks`: each link setting and fault checked on either backend |
| `tiers_test.go` | the serial and parallel tiers |
| `container_test.go`, `hosts/`, `hostagent/` | the container backend: host containers reached over real ssh (`TOWER_HOSTS=container`), and the agent in each |
| `sshd_test.go` | the sshd backend: hosts of this machine behind sshds of their own (`TOWER_HOSTS=sshd`), lo0's rules through pf on macOS |
| `procs_*_test.go` | this user's processes, per platform: argv and environment, and the process tree |
| `sshcall/` | an ssh command line read as ssh does, and the call log (`ssh.log`) |
| `sshwrap/` | tower's ssh in a world: logs the call, then runs real ssh with the world's config |
| `load_test.go` | load programs run on a host or beside a terminal (flood, build log, terminal reader): the suite's binary with `SCENARIO_HELPER` set |
| `s*_test.go` … | the scenarios, one file per family (`s_core`, `s_links`, `s_data`, `s_handoff`, `s_loop`, `dash`, `e_extras`, `v_towerd`, `i_install`, `lh`, `lv`, `lc`, `ld`, `ls`, `a_atlas`, `u_dash`, `r`, …) |

## Hosts

`w.Host(name, sessions, opts…)` makes a tmux server `-L tt-<id>-<name>`
with a generated config (`exit-empty on`, `/bin/sh`, `base-index`,
`detach-on-destroy no-detached`, `M-o` running the dashboard in a full-size
popup, `M-l` running `tower last`): a server of this machine, as a home
is, or with `SSHHost()` a remote, for which it registers the ssh name
`name` (`w.SSH`, below). A host's processes run with:

- `HOME` and `XDG_CONFIG_HOME` in an empty directory, so tmux servers tower
  starts read no user config;
- `TOWER_HOME=<dir>/home-<machine>`, `TOWER_MACHINE_ID=<machine>` (a
  container's own `/etc/machine-id` for a container host),
  `TOWER_TMUX=-L <sock>`;
- `TOWER_SSH=sshwrap`, `TOWER_TEST_SSH_CONFIG=<dir>/ssh_config`,
  `TOWER_TEST_SSH_LOG=<dir>/ssh.log`;
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
simulated host: its ssh name, its tmux socket, the built binary.
`w.WaitLink`, `w.WaitUp` and `w.WaitLoop` poll the home's full status.

## Driving towerd directly

Scenarios that test towerd's side of a behaviour whose other side is the
attach loop or the dashboard call towerd as those would:

- `h.View(client)` and `h.Act(client, req, ms)` are the dashboard's `view`
  and `act` calls on `h`'s towerd, as the client `pid:created:name`
  (`h.ClientIDs(session)`), with a deadline `ms` from now;
- `w.FakeLoop(home)` beats every 2s like a loop and makes its attaches
  with `Prepare`, running the prepared argv (the real attach shim, over
  ssh for a remote) in a terminal of its own; `Attach` ends the
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
until the client is back) are checked whole and in three parts
(`checkRecoveries`), since a new connection costs what the ssh at the
other end makes it cost: real ssh's 13 round trips, where the fake ssh
the limits were set with took about 6.5 (decision 120). The whole
recovery is within the scenario's limit plus 7.5 round trips (14 for the
connection in place of 6.5) and R0's connection; this bounds too a
connection under way as the network came back, which the parts do not. The connection,
from the link's last attempt to the link up (ssh, tower started there,
the hello, the first state), is at most 14 round trips past R0's in the
same world, counted at RTT 150 and 400 and only for a connection begun
within the recovery. Tower's part, the rest, is within the scenario's
limit less 6.5 round trips: the time the limit left tower when it was set.
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

Scenarios shape links and set faults per ssh name through the World;
`w.SSH(alias, host)` registers another name for an `SSHHost()` host.
Each host's backend puts them in place, with the mechanisms of a real
network and sshd: a container's, or a host's behind sshds of its own.

| Call | A host container | A host behind sshds |
| --- | --- | --- |
| `w.Shape(alias, f)`: `DelayMs`, `JitterMs`, `BwKBps` | `tc netem` delay, jitter and rate on `eth0` and, through `ifb0`, its ingress | dummynet pipes on lo0 for the ports of the machine's names, one each way; with jitter, five each way (below) |
| `w.Freeze(alias, on)` | `iptables` DROP of the name's port, both ways (`TT-FREEZE-IN` by destination port, `TT-FREEZE-OUT` by source port): every connection gives up, the far side never hears | pf `block drop` of the name's port, both ways |
| `w.NetworkChange(alias, at)` | DROP, both ways and for good, of each connection established now (`TT-HALFOPEN`); `at` not in the future | pf `block drop`, both ways and for good, of each connection to the machine's ports established now (`netstat`) |
| `w.Stall(alias, on)` | the processes the name's sessions run there (their `SSH_CONNECTION` names its port) stopped by the agent, and new ones as they come, every 20ms; sshd still answers, other names' sessions run on; a daemon a session started (a towerd, below) runs on | the processes under the name's sshd's connections, not sshd's own nor a daemon's, stopped by the harness, and new ones as they come, every 20ms |
| `w.Drop(alias)` | the sshd processes holding the name's port's connections (`ss -p`) killed | the name's sshd's connection processes killed |
| `w.Down(alias, how)` | per name in the world's ssh config: `refused` port 1, `timeout` port 2222 (SYNs dropped), `password` port 2223 (an sshd with key auth off), `resolve` an `.invalid` name, `hostkey` a known_hosts with another key, `auth` a key no host authorizes | as on a container, `timeout` a port of the name's whose SYNs pf drops, `password` the name's sshd's second port, key auth off there |
| `w.Heal(alias)`, `w.Reset(alias)` | faults off (and the link unshaped); a network change's connections stay dead | the same |

ssh exiting 255 is a `Drop`; an attach exiting 42 or 43 is the host's
client detached so (`detach-client -E 'exit 43'`, LS09), which ssh
passes on; a wedged master is the home's own stopped (`w.masterPid`,
LH05). ssh's channel window is 2 MB, so a stalled pipe needs `Stall`
alone (LH, LS09).

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
master at RTT 100ms takes 205–208ms (265ms behind sshds on a macOS
runner), 512 KiB at 256 KB/s 1.9–2.2s, each
`Down` reads as tower expects, a drop or a freeze ends a session (the
frozen one after the alive window, 18–20s, its far side still
running), a network change leaves the old
master to give up and a new one working, a stall stops output for the
alive window and more without ssh giving up, the other name's lines
coming every second.

A session riding a master that dies hears nothing: its ssh exits 255
with stderr empty, so tower reads "ssh exited 255"; off a master it
reads "connection lost" or "connection timed out".

## Host containers

On the container backend (Linux's) a host made with `SSHHost()` is a
container running sshd and tmux, reached over real ssh. Every
scenario's remote hosts are `SSHHost()`; homes stay on this machine.

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
  network. `h.Tmux` runs the container's own tmux, through the agent
  (0.3ms more than this machine's), as a client of another version may
  not reach the server: Ubuntu 24.04's 3.4 reaches 3.5a, not 3.6b. So
  does `h.TmuxServer`, for a client the test keeps open (`docker exec
  -i`; a 3.7c control client does not attach to 3.2a), and teardown;
  `h.Run` runs any of the host's
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
- Every host of every world has `TOWER_SSH=sshwrap`. It logs the call
  to the world's `ssh.log` (`w.SSHLog`) and runs `ssh -F
  $TOWER_TEST_SSH_CONFIG`:
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

On the sshd backend (macOS's) a host made with `SSHHost()` is a tmux
server of this machine reached over real ssh: macOS runs no containers
of its own. It needs neither Remote
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
- A macOS runner is too slow for LS08's throughput floor, on either
  backend, and a slower Linux runner for the containers' (77–92 MB/s
  relayed, 0.85–0.89 of plain, where a faster one relays 130). With
  `TT_LS08_REPORT=<file>` (the workflow sets it on every runner) each
  rate is measured three times, the best of each compared, and a
  shortfall written to the file rather than failed; bytes lost or extra
  fail as ever.
- Teardown stops the world's sshds first, killing their connections'
  processes (found under the sshd), then takes the world's rules and
  pipes off lo0.

## CI

`.github/workflows/scenarios.yml` runs a shard a job through
`scripts/ci-scenarios.sh`, as `mise run scenarios` does but keeping the
run's directories, which a failed job uploads (`worlds.tgz`, three
days). A scenario that failed is run again alone and its result made a
warning; the job stays red. A push that changes code, to any branch but
`chore/…`, runs the container shards at tmux 3.2a and 3.7c on
`ubuntu-24.04`, after loading the kernel modules the containers shape
with (`ifb`, `sch_netem`, `sch_ingress`, `cls_matchall`, `act_mirred`);
the home's tmux is the runner's. By hand (`workflow_dispatch`), `full`
adds 3.3a, 3.4, 3.5a and 3.6b, and sshd on `macos-15`; `macos-sshd`
runs that alone. A pull request
from a branch here has its push's run; one from a fork runs its own.

## Teardown

Terminals first (a live loop would restart a towerd), then every towerd
(SIGCONT, SIGTERM, SIGKILL after 5s), every session of every server; each server must exit within 10s, or the test fails. Leftover
processes with the world's `TOWER_HOME`, or naming the world's directory
(`pkill -f <dir>/`), are killed. The world's directory
is removed when the test passed. The home's control masters are ended
by their control path: ssh's `[mux]` process title
overwrites their environment.
