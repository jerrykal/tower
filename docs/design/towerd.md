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
| `act.go` | running requests on the local server, with deadlines and the answer memory |
| `clients.go` | registrations: made by `register`, bound to tmux clients, persisted |
| `keys.go` | `M-o`, `prefix L` and the alert hooks: take, record, restore |
| `keeper.go` | `tower _keep`: ends a dead towerd's control client and `_tower` |
| `remote.go` | the remote role: one record per connected home |
| `home.go` | the home role: merged view, loops, switches, prepare and after, standby offers, routing |
| `link.go` | one host link at the home: ssh, the stream, liveness, reconnect |
| `bridge.go` | `tower towerd --stdio` |
| `pace.go` | the token bucket shared by re-reads and pushes |

## Daemon

```go
type Daemon struct {
    env     *config.Env
    id      string
    version string

    mu      sync.Mutex   // guards everything below
    gen     uint64       // the watch generation
    changed chan struct{} // closed and replaced on every bump
    regs    *registry
    homes   map[string]*homeRec // remote role, by home id|as
    home    *homeRole           // nil until a loop calls
    ...
}
```

- **Start**: take the lock (`flock` on `<run>/<tag>.lock`, the `*os.File`
  kept in the daemon for its life), remove a stale socket, listen, write
  `towerd.pid`, read `id`, start the watch, restore `clients.json`, then
  answer calls. Drop `TOWER_MKEY`, `TOWER_TMUX_BIN` and `TOWER_FZF_BIN`
  from the process environment after reading them.
- **Calls**: one goroutine per connection; a call decodes, dispatches by
  `op`, and writes the reply. Long calls select on their condition, the
  connection closing (a read on it returns), and the daemon stopping.
- **Notifier**: `bump()` (under `mu`) increments `gen` and closes
  `changed`; `watch` callers with an older generation answer at once, else
  wait on `changed` (or 20s).
- **Idle exit** checked every second: no loop, no connected home, no live
  registration, no call for `TOWER_IDLE`.
- **Exit** (stop, SIGTERM, socket or state dir gone, idle): restore keys and
  hooks, close the control client by name, kill `_tower`, close the
  listener and remove the socket, then close links and streams.

**Lock discipline.** `mu` is never held across I/O: a tmux command, a
stream send, a file write or a wait. Code copies what it needs under `mu`,
releases it, then acts. Stream handlers run on the stream's reader
goroutine and take `mu` briefly.

## Watch

```go
type snapshot struct {
    Inst     string
    NoServer bool
    Sessions []proto.Session // _tower left out
    Clients  []tmuxClient    // name, pid, created, session, window; control clients left out
}
```

- **Attach**: probe with `list-sessions` (which never starts a server). No
  server: poll every 2s, and publish `NoServer`. A server: create `_tower`
  if missing (`new-session -d -s _tower -x 10 -y 3 'tower _keep <pid>
  <state dir>'`, `remain-on-exit off`, `destroy-unattached off`, status
  off), kill a leftover control client from `ctl.pid` (V07), attach the
  control client, write `ctl.pid`, install keys and hooks.
- **Session last attached** is never disturbed: the control client attaches
  to `_tower` only, with `ignore-size`.
- **Re-reads**: every notification marks the view dirty; a re-read runs
  through the pacer (two at once, then one per 50ms in a burst), and takes
  all notifications that arrived before it started. A re-read is three
  commands sent back to back: `display -p` (instance), `list-sessions`,
  `list-windows -a`, `list-clients`. `tower-alert` messages from the alert
  hooks also trigger one.
- **Last user session gone**: `_tower` is killed so it never keeps a server
  alive (S17, E03); the watch goes back to polling for a server.
- **Server gone** (`%exit`): back to polling.
- Re-reads are serialized: one at a time, so an older one never lands after
  a newer one.

The watch publishes each snapshot to the daemon, which binds
registrations, builds each home's `state`, bumps rows, and lets the home
role merge.

## Requests (act.go)

`run(req)`: refuse if past its deadline; answer a repeat id from memory (10
minutes); otherwise run on the control client:

| Op | tmux |
| --- | --- |
| kill | `kill-session -t $id` / `kill-window -t @id`; "already gone" if missing |
| rename | `rename-session -t $id <name>` / `rename-window -t @id <name>` |
| new | `new-session -d -P -F … -s <name> -c <dir>` / `new-window -d -P -F … -t $id -n <name> -c <dir>`; with no server: `start-server ; source-file -` first (up to 15s) |
| capture | `capture-pane -e -p -t <pane>` (the active pane of the window) |
| has | `has-session -t $id` with the instance check; no server means gone |
| dup | `new-session -d -t $id -s <name>` (grouped) |

Kill, rename, new and dup force a re-read and its publication before they
answer (read your writes).

## Registrations (clients.go)

`register{pid, loop, gen, home, inst}` stores an unbound registration and
schedules re-reads at 150ms, 600ms and 1.5s. A snapshot binds an unbound
registration to the tmux client with that pid (name and `client_created`);
a bound registration whose client is gone is dropped (S10). The set is
saved to `clients.json` on every change and restored at start (V03). A
`TOWER_CLIENT` value (`pid:created:name`) finds its registration by all
three.

## Keys (keys.go)

On attach (unless `TOWER_BIND=0`): `list-keys` for `M-o` in root and `L` in
prefix. Take each that is unbound, or (`L`) still `switch-client -l`;
record the previous binding in `keys.json`; bind tower's command with
towerd's `TOWER_*` environment and `TOWER_MKEY`. On stop, put back each key
that still has tower's binding. Alert hooks (unless `TOWER_ALERTS=0`):
`set-hook -g alert-bell[7193] …` and the same for activity and silence,
unless that index is taken by something else; removed on stop.

## Remote role (remote.go)

A `stream` call turns the connection into a stream. The first message must
be a `hello` from a home; the reply hello (after the first look: the watch
has a snapshot, or the daemon is 2s old) carries the chosen protocol, our
id, machine key, OS, tmux version and version, or the error. Records are
keyed by `home id | as`; a new stream for a key closes the old one.

Per home record: the stream, the last view (kept an hour after a
disconnect, marked not connected), and a state pacer (100ms). On every
snapshot each record gets its `state`: the sessions, and the clients whose
registration names that home. `exec` runs `act.run` and answers; a `view`
replaces the held one if newer.

Requests from local dashboards whose target is not this machine, and
switches from a client whose registration names another home, are sent to
that home as `relay` with the deadline converted.

## Home role (home.go, link.go)

Activated by the first `loop` call (or at start if `last.json` lists
loops). It reads `hosts.toml` (again on `reload` and when a dashboard
opens) and keeps one link per enabled host.

**Link**: `ssh -T … host -- '<tower> towerd --stdio --tmux …'` with stderr
read line by line (Tailscale check), a stream with the home's timings, the
hello (15s), then `up`. Status: `connecting`, `up`, `stalled`, `down`,
`failed`, `dup`, `off`. On a stall: probe ssh; give up after `3s + 4 ×
SlowRTT` (stream only if the probe answered, else `ssh -O exit` first).
Reconnect with backoff 1s → 2m with jitter, reset after 30s up; retry in
200ms after a drop that followed the stable period; at once after a give-up
or a network change. A wake (wall clock jumped past the monotonic clock by
more than 2s) closes every stream and exits every master in parallel. The
link generation counts connects.

**Merged view**: the local host (status `local`) and every link, each with
its last known sessions (`hosts.json` keeps them across restarts), plus the
loops. Pushed to every up link through its pacer (150ms) when it changes,
and sent at once to a link that just came up.

**Loops**:

```go
type loopRec struct {
    id      string
    gen     int
    cur, prev proto.Ref
    host    string        // towerd id of cur
    sw      *pendingSwitch // stored switch: target, nonce, gen, at
    waiter  chan struct{}  // wait-switch
    heldCh  chan bool
    beat    time.Time
}
```

- `prepare`: wait for the host (8s), check the session and window in the
  listed instance, bump `gen`, `prev = cur`, `cur = target`, drop a pending
  switch, build the argv (local: the shim; remote: `ssh -t` and the shim),
  the go line and the standby key.
- `switch` (local `act` or a relay): resolve the client's registration →
  loop and gen; refuse a stale gen or a stalled target host; store; wake
  the loop's `wait-switch`; wait up to 100ms for `held`; answer `end` and
  ack `ended`, or ack without it.
- `after`: the table in protocol.md, with `has` asked of the session's host
  directly.
- `standby`: offers for up, unstalled hosts up for 1s with standby allowed.
- `last`: the loop's previous target; same server → `switch-client`, else a
  stored switch.
- Loops expire 15s after their last beat; a loop's state is saved in
  `last.json` for the next `tower`.

**Routing**: a request whose target host is this machine runs locally;
otherwise it is sent as `exec` on the target's link (refused at once if
stalled or down) with the margin kept back; a `relay` from a remote first
sends that remote the merged view, then the ack.

## Bridge (bridge.go)

`tower towerd --stdio --tmux …`: ensure the local towerd (replacing an
older or wedged one), `Dial`, send the `stream` call, then copy stdin to the
socket and the socket to stdout until either ends. Logs go to the towerd
log, never stdout.

## Concurrency summary

| Goroutine | Owns |
| --- | --- |
| accept loop, one per connection | a call |
| the watch | the control client, re-reads, polling for a server |
| control client reader | tmux's output |
| per stream: reader, writer, ticker | the stream |
| per link: the connect loop | the ssh process, reconnects |
| home: net watch, wake check, view pacer | – |
| idle check | – |

Everything shared is under `Daemon.mu`; pacers and streams have their own
small locks.
