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
| `dirs.go` | git state of sessions and zoxide directories: the refresher (`internal/dirs`) in towerd, and looks |
| `clients.go` | registrations: made by `register`, bound to tmux clients, persisted |
| `keys.go` | `M-o`, `prefix L` and the alert hooks: take, record, restore |
| `keeper.go` | `tower _keep`: ends a dead towerd's control client and `_tower` |
| `remote.go` | the remote role: one record per connected home; the dashboard's `view`; `last` |
| `home.go` | the home role: merged view, loops, switches, prepare and after, standby offers, routing |
| `reap.go` | the home's detach of its stale clients on remotes |
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
    Name      string    // this host's name; empty: TOWER_TEST_NAME, else "local" in views and the short host name in messages
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
    lookAt   time.Time           // the last look
    dirs     *dirs.Refresher     // git state and zoxide directories, own lock
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
  tower-alert` from the alert hooks included, or before tmux 3.4 their
  `%window-renamed`) kicks the pacer (two at
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

`publish(snap)` (in `local.go`) hands a changed snapshot's session
directories to the refresher (it only records them), binds registrations,
bumps, kicks each connected home's state pacer and lets the home role
follow its loops' clients and push views.

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
| capture | `capture-pane -e -p -t <pane, else window, else session>` and `list-panes -t <the same>` in one batch: the text and its window's panes |
| panes | `list-panes -s -t $id` (every pane of a session's windows), or `list-panes -t @id` for a window (`Kind`, or a target with a window and no `Kind`) |
| has | `has-session -t $id`; no control client: `tmux -N has-session`; no server means gone |
| dup | `new-session -d -t $id -s <name>` (grouped) |
| detach | from a home's `exec` only: `detach-client -t <name>` for the client (`pid::name`) registered with that home (the stream's, not the request's), loop and gen, and still the client the registration was bound to; otherwise, or gone meanwhile, "already gone". No re-read before the answer: tmux drops the client only once it has gone, and the watch sees that |

**A pane** is `pane_id`, `window_id`, left, top, width, height, active,
`pane_current_command` and `pane_current_path`, the path with `~` for
this machine's home (or its resolved path: tmux reports a pane's
directory with symlinks resolved).

**A new session's directory** (`Dir`): `~` is this machine's home; a
relative path is taken from the home; none is the home. A new window with
no `Dir` starts in its session's directory (`-c '#{session_path}'`, which
tmux expands against the target). `-c` is format-expanded, so a given
directory goes in with `#` doubled. tmux falls back to the home without a
word when it cannot enter a directory, so a directory that is gone is
refused (`no directory ~/x on B`); one on a network mount is not checked,
since the stat could block.

Kill, rename, new and dup re-read and publish before they answer (read
your writes). Every id is quoted in tmux's language (`'$3'`), since tmux
expands `$name` in unquoted words.

## Git state and zoxide directories (dirs.go)

towerd runs one `dirs.Refresher` ([dirs.md](dirs.md)), started with the
daemon; it finds git and zoxide in its own goroutine. Its answers are
joined in when a state or a view is built, never stored in the snapshot,
so the watch's re-read path is untouched:

- each session gets `Git` for its directory (`withGit`), in the state a
  home receives and in this machine's own host entry (`localHost`);
- the state and the local host entry carry `Dirs`: the zoxide
  directories with no session yet (the refresher leaves out the
  sessions' directories, which it has from `publish`).

A change the refresher reports (`dirsChanged`) bumps the watch generation
and kicks each connected home's state pacer and the home role's view
pacers; each sends only if its body changed, paced as ever. `TOWER_GIT=0`
turns git state off, `TOWER_DIRS=0` the zoxide directories;
`TOWER_DIRS_EVERY` sets the periodic refresh (60s).

**Looks.** A `view` call (a dashboard opening, or following the view) is
a look, at most every `TOWER_LOOK_EVERY` (10s) per towerd: the refresher
refreshes everything not asked within 2s, and the look is passed on as a
`look` message, so every host a dashboard can show refreshes too:

| A look from | Passed on to |
| --- | --- |
| a dashboard here (`view`) | every home connected here, and every up host of this home |
| a host of this home (`look` on a link) | this home's other up hosts |
| a home (`look` on a home's stream) | nobody |

so a look never goes round, and the rate limit bounds a storm of reads.
A dashboard opening on any host has every host's changes within about a
round trip and a git status (A01: 43ms). A peer that does not know `look`
ignores it.

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
unless that index holds something else; removed on stop. tmux before 3.4
(by `#{version}`) shows a control client no message (3.2 takes
`display-message`'s `-c` for a flag), and tells it nothing a hook's own
commands do: there the hook is
`run-shell -b "<tmux> -S #{q:socket_path} rename-window -t =_tower:
tower-alert"`, a client of its own whose rename of `_tower`'s window the
control client, attached there, hears of.

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
disconnect), and a state pacer (100ms): on every published snapshot, and
on every change of git state or zoxide directories, the record's state
(sessions with their git state, the zoxide directories with no session,
and the clients whose registration names that home) is sent if its body
changed. A `look` from the home refreshes this machine (see above). `exec` runs `runLocal`; a change goes
out as a state with `SendNow` before the `ack`. A `view` replaces the held
one unless older than it within the same stream.

`view` (the call) builds a dashboard's `Dash`: the client's registration
names the loop and its home; that home's view is ours when we are it,
else the held one (a home not connected: kept view and the note "home <h>
not connected: other hosts as of Ns ago"); a client no loop owns sees this
machine's home when it has hosts, else the home connected last (a home
role with no hosts, a towerd a dashboard started on a remote, would show
this machine alone), with "not a tower terminal: ⏎
to another host needs the attach loop". This machine's own host entry is
always the live one, ages moved on to now.

`last` (the call): the client's loop's previous target, from the home's
view; on the client's own server it answers `Local` (the CLI runs
`switch-client`), else it stores a switch through the switch path and
passes on the ack's `Ended`: without it the CLI ends the client as a
dashboard does (hold, `detach-client -E 'exit 42'`).

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
    Run(ctx, h, remote string, stdin io.Reader) (string, error) // an install's commands
}
```

**Link**: sweep, then `ssh -T … host -- '<tower> towerd --stdio --tmux …'`,
where `<tower>` is the pinned binary or this build's versioned path under
the install root (`towerCommand`, also used for attaches and standbys); a
127 from an unpinned host installs the build (`internal/install`, status
`installing`) and connects again at once,
with stderr read line by line (a Tailscale check banner ends ssh at once),
a stream with the home's timings, the hello, then the first state (30s
for both, ssh's connect included: 14s on LC05's slowest link):
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
generation grows with each connect that came up: the time in ms, past the
last, so a new link to the host (its entry changed, or the home
restarted) has a later generation than any old one's. Interface addresses are compared
every second (`netchange` simulates a change).

**Merged view**: the local host (status `local`) and every link in
`hosts.toml` order, each with its last known sessions (`hosts.json` keeps
them, with ids, versions and git state, across restarts) and zoxide
directories (from its last state; not kept across restarts: a host not
up cannot make a session in one), plus the loops. A `look` from a host
is passed on to the others (see Looks). Pushed to
every up link through its pacer (150ms) when its body changed, at once to
a link that just came up; numbered per home.

**Loops**:

```go
type loopRec struct {
    id        string
    gen       int
    cur, prev proto.Ref
    sw        *pendingSwitch // target, nonce, gen, at, wait, aborted
    waiters   []chan struct{} // wait-switch calls of the current attach
    woke      *switchWait     // the attach's one wake: held, until, waiting, ended
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
  ends the client. A switch stored while the attach's wake still waits,
  or once the loop ends the client, joins that wake (`woke`, dropped at
  the next prepare) rather than being left to its asker, and waits only
  for what is left of the wake's 100ms: its switches hear one outcome.
- `held`: `End` only while the wake still waits and only once.
- `after`: the table in protocol.md. 42 or `Ended`: a stored switch for
  this attach under `TOWER_HANDOFF_TTL` (30s) hands off; otherwise the
  picker with "exit 42 without a valid hand-off (no request | request
  from an earlier attach … | request is Ns old): ignored"; a switch
  aborted by a stall hands back to `cur` with the reason. 43: the picker.
  255 on a remote: reconnect. Otherwise `has` is asked of the session's
  host: still there → exit (a pending switch discarded, said so); gone →
  the previous session if nobody is on it, else the most recently used
  session nobody is on, on any reachable host ("tower: X ended; now on
  Y"), else exit. A client of this home's whose attach is over (an older
  generation than its loop's, not yet reaped) is nobody, unless its
  host's towerd told the reap it cannot detach (it then stays).
- `standby`: offers for up, unstalled hosts up for 1s, standby allowed,
  not the loop's current host; key `link gen | towerd id | version | tmux
  args | tower path`.
- Loops expire 15s after their last beat; the last target is saved in
  `last.json`.

**Stale clients** (`reap.go`): a loop ends a remote attach by ending its
ssh, which rides a control master, and the session there can outlive it,
keeping its tmux client attached (seen in real use; a plain ssh over a
master, killed, keeps its session until the master exits; decision 112).
So the home detaches its own clients on
remotes once their attach is over: a client of any other generation than
its loop's (the home numbers attaches in `prepare`, which a loop calls
only after its previous attach ended; a home that restarted numbers
afresh, so an older client can carry a higher number), or whose loop the
home has not known for 15s (a restarted home, or one that called a slow
loop gone, learns a live loop again from its next beat). It looks on
every state, on `prepare` (the client goes at once, not at its host's
next state) and every second (a loop gone, a retry), only on hosts up,
and sends `exec detach` off the hand-off path, again after 5s while the
client is still listed (after an hour from a towerd that does not know
`detach`), logging a client's first try and its success. Clients on this
machine are left alone: the loop detaches them itself, and one whose
loop died still has the terminal.

**Routing** (`route`): a request for this machine runs here; otherwise
`exec` on the target's link (refused at once if it is not up, or
stalled), the stream keeping back its margin. A request whose deadline
is within the margin is refused ("too little time left to reach X"),
but only on a measured round trip: before the link's first pong the
margin is the hello's time, which counts the connect, so a request
within two of it (the margin kept back and the trip there) waits for
that pong first (a stale client's detach at link-up), until only the
least margin (`stream.MinMargin`) is left, or the link goes (then the
request hears "X: connection lost" at once). Errors:
"X is not responding", "X did not answer in time", "X is down: reason". A `relay`
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
| the refresher, and up to 4 git runs of a round | git state, zoxide directories |

Everything shared is under `Daemon.mu`; pacers, streams and the
refresher have their own small locks. `Daemon.mu` may be held while
taking the refresher's lock (building a state), never the other way:
the refresher calls `dirsChanged` with no lock held.

## Tests

`unit_test.go` covers the pacer, the re-read parser, registrations, the
answer memory and key listings. `daemon_test.go` runs towerds in one
process, each on a tmux server of its own, joined over their real sockets
by an in-process `Transport`: home and remote (views both ways, read your
writes, relays, a request run once, the home going away), the loop calls
(prepare's refusals, every row of `after`, the stored and the eager
switch, an earlier attach), keys taken and put back, and a host with no
server. Its daemons run with `TOWER_DIRS=0`, so they never read the user's
zoxide database. `dirs_test.go` gives them a home directory, a fake zoxide
and a mount table of their own: a remote's git state (a branch, a linked
worktree) and zoxide directories in the home's view and back on the
remote, a dirty tree after a look from the remote's dashboard, a closed
session's directory listed again; panes and capture's layout through the
home; new in a directory (`~`, a `#`, one that is gone, none), a new window
in its session's directory, a window renamed and killed, and a grouped
duplicate.
