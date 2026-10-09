# loop

The attach loop (`tower` outside tmux, and `tower dash`), the attach shim
(`tower attach`, also as a standby), and the loop's set of standby
sessions.

| File | Holds |
| --- | --- |
| `loop.go` | `Run`, start and close, the heartbeat, frame holds, the state machine, the reconnect's pause |
| `attach.go` | one attach: local through the shim, relayed (standby or new session), ssh given the terminal; wait-switch, ending the old client, the give-up, the fallback release |
| `view.go` | the loop's copy of the home's view (`watch` + `view`) |
| `standby.go` | the standby set |
| `shim.go` | `tower attach` |

## Attach loop

```go
type Options struct {
    Dash bool        // tower dash: start at the picker; esc there attaches to the last target
    Env  *config.Env // the home: this machine and tmux server
}
func Run(ctx context.Context, o Options) int // exit code for the process
```

`cmd/tower` sets the `attachLoop` hook to it. SIGHUP and SIGTERM end the
loop as an exit does.

**Start.** The terminal (stdin, stdout) must be a tty; its modes are kept
as found. Ensure the home towerd (not bridged, so it plays home), take its
tmux binary, send the first `loop` beat, which answers the last target
(`last.json`). Write `loop-<id>.pid` in the towerd's state dir. Start the
view watcher, the heartbeat and, unless `TOWER_RELAY=0` or
`TOWER_STANDBY=0`, the standbys. The first step is the picker with
`Dash`, with no last target, or with `TOWER_TEST_PICKER`; otherwise the
last target.

**State machine** (protocol.md, "After an attach ends"):

```
picker ──⏎──▶ prepare ──▶ attach ──▶ after ──┬─ handoff ──▶ prepare (the note on the next client)
   ▲              │ fails                     ├─ reconnect ─▶ [pause] ─▶ prepare
   │              ▼                           ├─ picker (with the note)
   └──── back where the terminal was,         └─ exit (the note on stderr)
         or the picker, saying why
```

- **Picker**: release any hold, restore the terminal's modes, `ui.Pick`.
  `esc` (`ErrQuit`) exits 0; `tower dash`'s `esc` (first picker only) is
  the last target, or exits with none.
- **Prepare** failing: for a hand-off's target, back to where the terminal
  was with the error as the note (a switch to a stalled host fails fast
  and lands back home); while reconnecting, a pause and another try;
  otherwise the picker with the error. An attach given up (see below)
  goes the same way, except that with nowhere to go back to the loop
  retries the target instead of the picker.
- **After**'s note goes on the next client's status line (the shim's
  `--note`, a standby's go line) or, at exit, to stderr.
- **Reconnect** on 255: at once, then backoff 1s doubling to 30s while it
  keeps failing; a failed prepare (or an attach given up) pauses at most
  4s; more than 10s attached starts the backoff afresh. The pause shows
  why on the terminal, ends early when the home's view shows a new link
  generation for that host, and `ctrl-c` (the terminal raw) gives the
  picker.
- **Heartbeat** every 5s: `{id, gen, cur, prev}`; at once when wait-switch
  answers at once without a switch (the home forgot the loop: it
  restarted). The view watcher keeps `cur` and `prev` as the home has them,
  so moves on a server travel in the beat too.
- **ctrl-c** between attaches gives the picker: SIGINT (the terminal
  cooked: the first prepare, `after`) cancels the wait under way; while
  reconnecting the terminal is raw and the ctrl-c byte does it. It never
  ends the loop, so `loop-bye` and the terminal's restore always run.
- **A towerd gone** (the socket refuses) is started again and told about
  the loop (a beat) by the call that found it gone: at once for the
  user's own steps (prepare, `after`, `held`, anything at the picker),
  after 10s for the background ones (heartbeat, view, standbys,
  wait-switch), so a home being restarted is not raced.
- **Exit**: end the standbys, `loop-bye`, release any hold, restore the
  terminal's modes, remove the pid file.

## Attach

- **Local**: run the prepared argv (`tower attach …`, which execs tmux)
  with the terminal as its stdio, its modes restored first. Ending it for
  a switch: `detach-client -t <its client> -E 'exit 42'` on the local
  server (the client named by the loop's own tty) through towerd's
  control client (`end-client`), so tmux restores the terminal and prints
  nothing; a towerd that cannot leaves it to one tmux run.
- **Remote**: the host's standby if prepare's key matches and it is ready
  (one being recycled is waited for, up to the standby's wait) and made
  for this terminal; its pty sized, a newline and the go line sent, the
  go marker awaited `300ms + 2 × RTT` (`TOWER_STANDBY_TIMEOUT`), else
  `Kill` and a new session. A new session is `relay.Start` with the
  attach argv, the terminal's original modes and size. The terminal goes
  raw and the session is relayed.
- **Reuse**: prepare asks for a nonce (`Again`) whenever the loop has
  standbys, unless `TOWER_REUSE=0`. A standby whose shim wrote the again marker, given one, is
  reused: its relay is armed (`Reuse`), ending it for a switch is
  `Release` (the relay stops at once, ssh runs on), and a released relay
  hands the session back to the standby set (`recycle`), where it waits
  for the home to have the client detached back into a standby. A relay
  that ends otherwise (ssh exited: the client ended by itself) ends the
  session. Any other session: ending it for a switch is `Terminate` and
  `Abandon` (the relay stops at once; ssh is reaped in the background),
  so the next client waits for nothing.
- **`TOWER_RELAY=0`**: ssh with the terminal itself, ended with SIGTERM.
- **wait-switch** runs alongside (not with `TOWER_EAGER=0`, so the home
  leaves the old client to the dashboard). On `switch`: `Hold`, `held`; on
  `end`, end the client and report 42 with `Ended`. Without `end` the
  dashboard detaches; should nothing end the client, that hold is
  released after 1.5s.
- **Give-up**: a remote attach whose host the view shows stalled or down
  before the home has seen its client is killed: a hand-off goes back
  where the terminal was, saying why; otherwise the loop pauses and tries
  that target again, as after a lost connection. Only a view of the link
  the attach was prepared on (`Prepared.Link`), or a later one, counts:
  the loop's view can still show the earlier link connecting when a
  prepare that waited for the link answers. A link's generation is the
  time it came up (ms, past the last its towerd gave), so a later link's
  is greater even after the home restarts or the host's entry changes.
- **The old client gone**, the frame is held again at once, then a remote
  client hung up, released or lost (255) gets the terminal restore its
  own never wrote (`relay.ClientRestore`), if it drew anything.
- **Fallback release** of that hold: 150ms after the view shows the home
  saw the new attach's client, or 1.5s, `Release(n)` for that hold only
  (S27). Always released before the picker or an exit.
- Marks: `attach`, `attach: standby`, `attach: session`, `standby: go`,
  `standby did not answer`, `switch stored: hold`, `end attach`, `exited
  <code>`, `attach given up: <why>`.

## Attach shim

`internal/loop/shim.go`: `tower attach --loop L --gen G --home H --inst I
--mkey K [--standby] [--note TEXT] --tmux ARGS $3 [@7]`:

1. With `--mkey` and a towerd answering under it with that key,
   `register` is the only call (its answer carries the key and the tmux
   binary); otherwise ensure first. A towerd the shim starts is
   remote-only (`--bridged`): an attach never makes a machine a home.
2. Exec `tmux <args> attach-session -t $3 ; if-shell -F
   '#{!=:#{pid}:#{start_time},<inst>}' "detach-client -E 'exit 43'" ;
   select-window -t @7 ; display-message -d 4000 <note>`
   (`AttachCommand`), with `TOWER_CLIENT` and `TOWER_MKEY` dropped from its
   environment.

As a standby (`--standby`, no target): ensure the towerd, turn the pty's
echo, line editing and signal keys off, throwing away input not read yet
(one `TCSETSF`/`TIOCSETAF`), and write, in one write on stdout (where an
attach's output goes), the ended marker with `--ended`'s nonce if given
(`relay.Ended`: `ESC ] 7193 ; tower-standby-ended ; <nonce> BEL`), the
again marker if it can be detached back into (`relay.MarkerAgain`:
`ESC ] 7193 ; tower-standby-again BEL`) and the ready marker
(`relay.MarkerReady`: `ESC ] 7193 ; tower-standby-ready BEL`). Then read
JSON lines a byte at a time until one is a go line (loop, gen, home,
inst, mkey, session, window, note, again), restore the modes, write the
answer marker (`relay.MarkerGo`, `ESC ] 7193 ; tower-standby-go BEL`),
and continue as above. A standby nobody used exits after 12h, and one
that hears no heartbeat from its loop (an empty line every 2s) for 30s
exits at once.

A go line with a nonce (`again`) registers it, with the command that
makes the client this standby again (`resumeCommand`: `exec <tower>
attach --standby --ended <nonce> --mkey K --tmux …`, each word
single-quoted, for whatever `default-shell` the session has; a word with
a quote or a backslash, or `TOWER_REUSE=0`, means no again marker). tmux runs it in the
client's process, the pid unchanged, once the home has the client
detached with `detach-client -E`.

## Standbys

```go
type standbys struct {
    byHost   map[string]*standby // towerd id → standby
    backoff  map[string]*hostBackoff
    reserved string              // the host an attach is being prepared for
}
type standby struct {
    host, name, key string
    sess  *relay.Session
    env   []string    // the loop's environment, tower's variables left out
    modes relay.Modes // the terminal's original modes
    again bool        // its shim wrote the again marker
    state state       // starting, draining, ready, inUse
}
```

A standby goes forward through its states once: `starting` (a new
session) or `draining` (an attach's session handed back), then `ready`,
then `inUse` once an attach takes it. A session handed back is a new
standby, so nothing that held the old one acts on the new.

- **Refresh** on every change of the view, every 2s, and when one is used
  or dies: ask `standby`; drop those whose key is no longer offered (host
  gone, down, stalled, reconnected), except the host being prepared and
  one in use; start those missing, once the view names their host, and
  not while backed off. Marks: `standby:
  start|ready|drop|recycle <host>`, `standby: not used <host>: <why>`,
  `standby: given up <host>: <why>`, `standby: wait for <host>`.
- **Use** (`take`): ready, prepare's key, the same environment and modes
  (`relay.Modes.Same`) as the terminal's now; one made for another
  terminal is dropped. One being recycled is waited for, up to the
  standby's wait. A standby taken for an attach with a nonce, whose
  client can be detached back into one, stays in the set, in use, so no
  refresh starts another for its host; any other is taken out of it.
- **Recycle**: the attach's session back in the set under its old key,
  watched as a new standby is: drained to the ended marker of its
  client's nonce within `1s + 4 × RTT` (`TOWER_RECYCLE_WAIT`), then
  ready within 1s. Given up (killed, the host refreshed) when either
  does not come, ssh exits, or the key is dropped; one whose host has
  another standby by then is ended. An attach that will not hand its
  session back (ssh exited, the standby did not answer) lets it go
  (`release`), and its host gets a new standby.
- A standby that dies (or is not ready in 20s) before it is ready backs
  its host off, 1s doubling to a minute; one that dies ready is replaced;
  a recycled one given up is replaced without a backoff.
- The home offers one for the host the loop is attached to as well; the
  loop makes it only when the attach's session will not come back (a
  session it opened itself, or a shim that cannot go again): the host a
  hand-off leaves has one ready for a switch straight back either way.
- **Status**: per host, the standby's state and time in it, and the
  sessions opened, reused and given up (the last one's reason) in the
  loop's life, sent with every `standby` call; `tower status` on the home
  shows them under the loop, with the last detach of a reused attach's
  client, and a remote's shows which clients are reusable.
- **Closing**: a standby's watch is the only thing that closes its
  session until an attach takes it, and the attach after; dropping one
  kills its ssh, and the watch, its read ended, closes it.

## Concurrency

The main goroutine runs the state machine and owns the attach. Alongside
an attach: its runner (process wait or relay), wait-switch, the host
watch and the fallback release. For the loop's life: the view watcher,
the heartbeat and the standbys' refresh, each a goroutine; the standby
set is under its own mutex (refresh, the watch per standby, and `take`
from the main goroutine); the loop's `gen`, `cur`, `prev` and the hold
number under the loop's mutex.
