# towerd

The daemon: one per user and tmux server. It watches its own tmux, keeps
the registrations of the clients tower started there, binds tower's keys,
serves local calls, and plays the remote role for every home that connects
and the home role once an attach loop starts on its machine.

## Files

| File | Holds |
| --- | --- |
| `daemon.go` | `Daemon`: lock, socket, call dispatch, the change notifier, idle exit, shutdown order |
| `watch.go` | the watch on its own tmux: `_tower`, the control client, paced re-reads, the snapshot |
| `local.go` | publishing a snapshot, this machine as a view lists it, the state for one home |
| `act.go` | requests: running them on the local server (deadlines, the answer memory), and the `act` entry that routes them |
| `clients.go` | registrations: made by `register`, bound to tmux clients, persisted |
| `keys.go` | `M-o`, `prefix L` and the alert hooks: take, record, restore |
| `keeper.go` | `tower _keep`: ends a dead towerd's control client and `_tower` |
| `remote.go` | the remote role: one record per connected home; the dashboard's `view`; `last` |
| `home.go` | the home role: merged view, loops, switches, prepare and after, standby offers, routing |
| `link.go` | one host link at the home: the `Transport` seam, ssh, the stream, liveness, reconnect |
| `bridge.go` | `tower towerd --stdio` |
| `pace.go` | the token bucket shared by re-reads and pushes: two at once, then one per interval; a run that sent nothing (an unchanged body) costs no token |
| `testhooks.go` | the scenario suite's hooks: `TOWER_TEST_PROTO`, `TOWER_TEST_FUTURE`, `TOWER_TEST_PAD` |

## Daemon

```go
type Options struct {
    Env       *config.Env
    Version   string
    Bridged   bool      // remote-only until a loop or a reload
    Transport Transport // nil: ssh; tests join daemons in process
    Self      string    // the tower binary for _keep, the shim and key bindings
    LogTo     io.Writer // nil: towerd.log
    Name      string    // this host's label; empty: TOWER_TEST_NAME or the short host name
}

func Start(o Options) (*Daemon, error) // lock, listen, watch; returns once serving
func Run(o Options) error              // Start, then until stopped (SIGTERM, SIGINT)
func (d *Daemon) Stop(why string)
func (d *Daemon) Wait()

type Daemon struct {
    ...
    mu       sync.Mutex     // guards everything below; never held across I/O
    gen      uint64         // the watch generation
    changed  chan struct{}  // closed and replaced on every bump
    snap     *snapshot      // the latest re-read
    regs     *registry
    homes    map[string]*homeRec // remote role, by home id|as
    home     *homeRole           // nil until active
}
```

- **Start**: take the lock (`flock` on `<run>/<tag>.lock`, the `*os.File`
  kept in the daemon for its life; another holder: `towerd already running
  for <tag> (<lock>)`), read `id`, remove a stale socket, listen, write
  `towerd.pid`, restore `clients.json`, start the watch, and, unless
  bridged, the home role. Keep the `TOWER_*` environment for key bindings,
  then drop `TOWER_MKEY`, `TOWER_TMUX_BIN`, `TOWER_FZF_BIN`, `TOWER_CLIENT`,
  `TMUX` and `TMUX_PANE` from the process.
- **Calls**: one goroutine per connection; a call decodes, dispatches by
  `op`, and writes the reply. A read on the connection returning cancels
  the call's context (the caller went away). A `stream` call hands the
  connection, with what its reader buffered, to the remote role.
- **Notifier**: `bump()` (under `mu`) increments `gen` and closes
  `changed`; `watch` callers with an older generation answer at once, else
  wait on `changed` (or 20s). A re-read that found nothing new does not
  bump.
- **Idle exit** checked every second: no loop, no connected home, no
  registration, no call in flight, and no call but `status` for
  `TOWER_IDLE` (10m; 0 never). `status` does not count: ensure and `tower
  status` must not keep an idle towerd alive.
- **Exit** (stop, SIGTERM, socket or state dir gone, idle): restore keys and
  hooks, close the control client by name, kill `_tower`, close the
  listener and remove the socket, then close links and streams; save the
  registrations, `hosts.json` and `last.json`.

**Lock discipline.** `mu` is never held across I/O: a tmux command, a
stream send, a file write or a wait. Code copies what it needs under `mu`,
releases it, then acts. Stream handlers run on the stream's reader
goroutine and take `mu` briefly; anything slow (an exec, a relay) runs on
a goroutine of its own.

## Watch

```go
type snapshot struct {
    At       time.Time
    Inst     string
    NoServer bool
    Sessions []proto.Session // _tower left out
    Clients  []tclient       // pid, created, name, session, window; control clients left out
}
```

- **Probe** with `list-sessions` (which never starts a server). No server,
  or a server with no session but `_tower`: publish that and poll every
  `TOWER_NOSRV_POLL` (2s); `new` pokes the poll.
- **Attach**: make `_tower` if missing with `tmux -N new-session -d -s
  _tower -x 10 -y 3 '<tower> _keep <pid> <state dir>'` and the session's
  `destroy-unattached off`, `remain-on-exit off`, `status off` in the same
  command (`-N`: creating it can never start a server); kill a leftover
  control client from `ctl.pid` (a tmux `-C` client whose parent is not
  this towerd, V07); attach the control client to `=_tower` with
  `no-output,ignore-size`, so no other session's `session_last_attached`
  moves (S22); write `ctl.pid`; read tmux's version; install keys and
  hooks; re-read.
- **Re-reads**: every notification but pane output (`%message
  tower-alert` from the alert hooks included) kicks the pacer (two at
  once, then one per 50ms in a burst); a re-read takes every kick that
  arrived before it started. A re-read is one `DoMany` batch: `display -p
  '#{pid}:#{start_time}'`, `list-sessions`, `list-windows -a`,
  `list-clients`. Re-reads are serialized (`readMu`), and publication
  happens inside, so an older one never lands after a newer.
- **Session ages**: `now - session_last_attached`, or `session_created`
  for a session never attached, in ms against this server's clock.
- **Last user session gone**: detach the control client and kill `_tower`
  so it never keeps a server alive (S17, E03); the watch goes back to
  polling.
- **A client on `_tower`** (a `switch-client` that landed there) is sent
  back with `switch-client -l`, or to the first session (S22).
- **Server gone** (the control client ends): back to probing.

`publish(snap)` (in `local.go`) binds registrations, bumps, kicks each
connected home's state pacer and lets the home role follow its loops'
clients and push views.

## Requests (act.go)

`act` (the call) is the entry for a dashboard's request on this machine:
a default deadline (`TOWER_ACK_TIMEOUT`, 5s), raised to 15s for `new` on a
host known to have no server; `switch` goes to the switch path; a target
on this machine runs here; anything else goes to the client's home (this
towerd's home role, or a relay to the home named by the client's
registration, or with no registration the home connected last).

`runLocal(req)`: once per request id (a repeat, even one arriving while
the first runs, gets the first's answer; kept 10 minutes); refused after
its deadline; a target listed on an earlier server instance is refused
("restarted since it was listed"), or for `kill` and `has` is already
gone. Then on the control client:

| Op | tmux |
| --- | --- |
| kill | `kill-session -t $id` / `kill-window -t @id`; "already gone" if missing |
| rename | `rename-session -t $id <name>` / `rename-window -t @id <name>` (names `#`-escaped) |
| new | `new-session -d -P -F … -s <name> -c <dir>` / `new-window -d -P -F … -t $id: -n <name> -c <dir>`; with no server a one-shot `new-session` that starts it (up to 15s for the user's config), then waits for the watch to take the server |
| capture | `capture-pane -e -p -t <pane, else window, else session>` |
| has | `has-session -t $id`; no control client: `tmux -N has-session`; no server means gone |
| dup | `new-session -d -t $id -s <name>` (grouped) |

Kill, rename, new and dup re-read and publish before they answer (read
your writes). Every id is quoted in tmux's language (`'$3'`), since tmux
expands `$name` in unquoted words.

## Registrations (clients.go)

`register{pid, loop, gen, home, inst}` stores an unbound registration
(one per pid) and schedules re-reads at 150ms, 600ms and 1.5s. A snapshot
binds an unbound registration to the tmux client with that pid (name and
`client_created`) and follows a bound one to its client's session and
window; one whose client is gone, whose server instance changed, or that
found no client within 10s is dropped (S10). The set is saved to
`clients.json` on every change and restored at start (V03). A
`TOWER_CLIENT` value (`pid:created:name`) finds its registration by all
three (by pid alone while unbound, after one forced re-read).

## Keys (keys.go)

On attach (unless `TOWER_BIND=0`): `list-keys -T root` and `-T prefix`.
`M-o` is taken when unbound; `L` when unbound or still `switch-client -l`;
a key that is still towerd's own from a towerd that died keeps its
recorded previous binding. Bound to `run-shell -C` of `display-popup -E -w
90% -h 90% "TOWER_CLIENT=… <env> tower"` and of `run-shell -b "TOWER_CLIENT=…
<env> tower last"`, where `<env>` is towerd's `TOWER_*` environment plus
`TOWER_TMUX`, `TOWER_MKEY` and `TOWER_TMUX_BIN`, with `#` escaped once per
format expansion on the way. `keys.json` records each key's previous
`bind-key` line and towerd's own line as `list-keys` prints it; on stop a
key is put back only if its line is still exactly towerd's (a user's own
binding that also runs tower is never touched). Alert hooks (unless
`TOWER_ALERTS=0`): `set-hook -g alert-bell[7193] 'display-message -c
<control client> tower-alert'`, and the same for activity and silence,
unless that index holds something else; removed on stop.

## Keeper (keeper.go)

`tower _keep <pid> <state dir>` is `_tower`'s pane. Every 250ms it checks
the towerd; once it is gone, a towerd now in `towerd.pid` is watched
instead; with none, it kills the control client in `ctl.pid` (a tmux `-C`
client whose parent is not a live towerd) and exits, which ends `_tower`
(S02, V07).

## Remote role (remote.go)

A `stream` call turns the connection into a stream. The first message must
be a `hello` from a home; the reply hello (after the first look: the watch
has published, or the daemon is 2s old) carries the chosen protocol, our
id, label, machine key, OS (`uname -s`), tmux version and version. An
incompatible range is answered with the error, drained, then closed.
Records are keyed by `home id | as`; a new stream for a key closes the old
one (S13). Remote streams give up after `2 × TOWER_SILENCE + 5s`.

Per home record: the stream, the last view (kept an hour after a
disconnect), and a state pacer (100ms): on every published snapshot the
record's state (sessions, and the clients whose registration names that
home) is sent if its body changed. `exec` runs `runLocal`; a change goes
out as a state with `SendNow` before the `ack`. A `view` replaces the held
one unless older than it within the same stream.

`view` (the call) builds a dashboard's `Dash`: the client's registration
names the loop and its home; that home's view is ours when we are it,
else the held one (a home not connected: kept view and the note "home <h>
not connected: other hosts as of Ns ago"); a client no loop owns sees this
machine's home, or the home connected last, with "not a tower terminal: ⏎
to another host needs the attach loop". This machine's own host entry is
always the live one, ages moved on to now.

`last` (the call): the client's loop's previous target, from the home's
view; on the client's own server it answers `Local` (the CLI runs
`switch-client`), else it stores a switch through the switch path.

## Home role (home.go, link.go)

A towerd started by a bridge (`tower towerd --bridged`) is remote-only
until a `loop`, `reload` or `plant` call activates the home role; one
started any other way (by the loop, a dashboard, the CLI) plays home from
the start. It reads `hosts.toml` (again on `reload`, and on `view` when
the file changed) and keeps one link per host; a link whose entry
changed is replaced.

**Transport** is the seam to the hosts:

```go
type Transport interface {
    Dial(h config.Host, remote string) (*Pipe, error) // the stream's ssh
    Probe(ctx, h) error                               // ControlMaster=no … true
    Exit(ctx, h) error                                // ssh -O exit
    AttachArgv(h, remote string) []string             // ssh -t …
    Sweep()                                           // stale control sockets
}
```

**Link**: sweep, then `ssh -T … host -- '<tower> towerd --stdio --tmux …'`
with stderr read line by line (a Tailscale check banner ends ssh at once),
a stream with the home's timings, the hello (15s), then the first state:
the link is `up` only once a state is in, so a host never shows up empty
(LC01). Status: `connecting`, `up`, `stalled`, `down`, `failed`, `dup`,
`off` (status reports `nosrv` for a host up with no server). A second
alias of a towerd already linked is `dup: same towerd as <name>`, its
sessions not listed, its stream closed. A version mismatch is a warning:
`tower X there, Y here (protocol N)`.

Liveness: on a stall (marked by the stream), abort stored switches to the
host, probe ssh (2s plus two slow round trips), and give the link up after
`3s + 4 × SlowRTT`: only the stream if the probe answered (a wedged towerd,
LH06), else with `ssh -O exit` too. A stall on a host not heard since a
network change gives up at once, master included. A stream that went
silent for `TOWER_SILENCE` (a sleep) resets the master too. The next
connect waits for a master reset under way. Reconnect with backoff
`TOWER_BACKOFF_BASE` (1s) doubling to `TOWER_BACKOFF_CAP` (2m) with
jitter, reset after `TOWER_STABLE` (30s) up; 100ms after a drop that
followed the stable period; at once after a give-up or a network change
(a down host); a failed host waits the cap. A wake (wall clock ahead of
the monotonic clock by more than 2s, checked every second; or the `wake`
call) closes every stream and exits every master in parallel. The link
generation counts connects that came up. Interface addresses are compared
every second (`netchange` simulates a change).

**Merged view**: the local host (status `local`) and every link in
`hosts.toml` order, each with its last known sessions (`hosts.json` keeps
them, with ids and versions, across restarts), plus the loops. Pushed to
every up link through its pacer (150ms) when its body changed, at once to
a link that just came up; numbered per home.

**Loops**:

```go
type loopRec struct {
    id        string
    gen       int
    cur, prev proto.Ref
    sw        *pendingSwitch // target, nonce, gen, at, held, waiting, ended, aborted
    waiters   []chan struct{} // wait-switch calls of the current attach
    seen      bool            // the home has seen this attach's client
    beat      time.Time
}
```

- `loop` (beat): a loop the home does not know is learned from the beat
  (cur, prev, gen), and every host's last client list is replayed so moves
  made while the home was gone apply (V07); a new loop with no target
  gets `last.json`'s.
- `prepare`: wait for the host (8s; stalled, failed, dup or off refused at
  once), check the instance and the session (and window) — "restarted
  since it was listed: … is gone", "session … no longer exists" — then
  `gen++`, `prev = cur` (a new session), `cur = target`, drop a pending
  switch, wake the old attach's waiters, and build the argv (local: the
  shim; remote: `ssh -t` and the shim), the go line and the standby key.
  An unknown loop is created.
- `switch` (an `act` here or a relay): the client's registration gives
  loop and gen (a request's own gen wins, for tests); refuse an unknown
  loop, an earlier attach ("request from an earlier attach (gen N, now
  M)"), or an unreachable target, before anything detaches; store
  (committed); if a `wait-switch` is waiting and `TOWER_EAGER` is on, wake
  it and wait up to 100ms for `held`: the answer says `Ended` when the loop
  ends the client.
- `held`: `End` only while the switch still waits and only once.
- `after`: the table in protocol.md. 42 or `Ended`: a stored switch for
  this attach under `TOWER_HANDOFF_TTL` (30s) hands off; otherwise the
  picker with "exit 42 without a valid hand-off (no request | request
  from an earlier attach … | request is Ns old): ignored"; a switch
  aborted by a stall hands back to `cur` with the reason. 43: the picker.
  255 on a remote: reconnect. Otherwise `has` is asked of the session's
  host: still there → exit (a pending switch discarded, said so); gone →
  the previous session if nobody is on it, else the most recently used
  session nobody is on, on any reachable host ("tower: X ended; now on
  Y"), else exit.
- `standby`: offers for up, unstalled hosts up for 1s, standby allowed,
  not the loop's current host; key `link gen | towerd id | version | tmux
  args | tower path`.
- Loops expire 15s after their last beat; the last target is saved in
  `last.json`.

**Routing** (`route`): a request for this machine runs here; otherwise
`exec` on the target's link (refused at once if it is not up, or
stalled), the stream keeping back its margin. Errors: "X is not
responding", "X did not answer in time", "X is down: reason". A `relay`
from a remote: the switch path or `route`, then the asking host gets the
merged view (`SendNow`), then the `ack`.

## Bridge (bridge.go)

`tower towerd --stdio --tmux …`: ensure the local towerd as remote-only
(replacing an older or wedged one), dial, send the `stream` call, then copy
stdin to the socket and the socket to stdout until either ends; stdin's
end half-closes the socket so towerd drops the home at once. Logs go to
the towerd log, never stdout.

## Concurrency summary

| Goroutine | Owns |
| --- | --- |
| accept loop, one per connection | a call |
| the watch | the control client's lifetime, polling for a server |
| watch pacer | re-reads |
| control client reader | tmux's output |
| per stream: reader, writer, keepalive | the stream |
| per home record: state pacer | – |
| per link: the connect loop, view pacer, stall watch | the ssh process, reconnects |
| home: net watch, wake check | – |
| housekeeping | idle exit, expiries |

Everything shared is under `Daemon.mu`; pacers and streams have their own
small locks.

## Tests

`unit_test.go` covers the pacer, the re-read parser, registrations, the
answer memory and key listings. `daemon_test.go` runs towerds in one
process, each on a tmux server of its own, joined over their real sockets
by an in-process `Transport`: home and remote (views both ways, read your
writes, relays, a request run once, the home going away), the loop calls
(prepare's refusals, every row of `after`, the stored and the eager
switch, an earlier attach), keys taken and put back, and a host with no
server.
