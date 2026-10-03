# loop

The attach loop (`tower` outside tmux, and `tower dash`), the attach shim
(`tower attach`, also as a standby), and the loop's set of standby
sessions.

## Attach loop

```go
type Options struct {
    Dash bool // start at the picker
    Env  *config.Env
}
func Run(ctx context.Context, o Options) int // exit code for the process
```

State machine (see protocol.md, "After an attach ends"):

```go
for {
    switch step {
    case picker:   // the dashboard on the loop's terminal; ⏎ → prepare
    case prepare:  // home.prepare(target) → Prepared
    case attach:   // run the attach (local or relayed), with wait-switch alongside
    case after:    // home.after(gen, code) → handoff | picker | reconnect | exit
    case reconnect:// backoff, re-prepare the same target
    }
}
```

- **Start.** Ensure the home towerd; send the first `loop` beat (which
  activates the home role); read `last.json` through the beat's answer. No
  last target, or `Dash`: the picker. `tower dash`'s `Esc` attaches to the
  last target when there is one, else exits.
- **Heartbeat** every 5s: `{id, gen, cur, prev, host}`, so a restarted home
  relearns the loop (S24, V07).
- **Attach.** Local: run `tower attach …` with the terminal as its stdio
  (it execs tmux). Remote: the host's standby if prepare's key matches and
  it is ready and made for this terminal, else `relay.Start` with the
  attach argv; then relay. `TOWER_RELAY=0`: ssh with the terminal itself.
- **wait-switch** runs alongside every attach. When it answers `switch`:
  `Hold`, call `held`; on `end`, end the old client (local: the home detaches
  it through its control client; remote: `Terminate` the ssh, then write the
  client's terminal restore inside the hold), and report exit 42 with
  `Ended`.
- **Frame release.** After the next attach starts: the new client's first
  synced frame releases the hold by itself; as a fallback, 150ms after
  `watch` shows the home bound the new client, or 1.5s, `Release(n)` for
  this hold only (S27). Always released before the picker or an exit.
- **Reconnect** on 255: at once, then backoff 1s doubling to 30s while it
  keeps failing; a failed prepare pauses at most 4s; more than 10s
  attached resets the backoff; the pause ends early when the home's view
  shows a new link generation for that host; `ctrl-c` gives the picker.
- **Exit**: end the standbys, `loop-bye`, release any hold, restore the
  terminal's modes.

## Attach shim

`tower attach --loop L --gen G --home H --inst I --mkey K [--standby] $3
[@7] --tmux …`:

1. With `--mkey` matching a towerd that answers, `register` is the only
   call; otherwise ensure first.
2. Exec `tmux <args> attach-session -t $3 ; if-shell -F
   '#{!=:#{pid}:#{start_time},<inst>}' "detach-client -E 'exit 43'" ;
   select-window -t @7`.

As a standby (`--standby`): ensure the towerd, set the pty's modes (echo
and canonical off), write the ready marker (`relay.MarkerReady`, `ESC ]
7193 ; tower-standby-ready BEL`), read one JSON line a byte at a time (the
go line: loop, gen, home, inst, mkey, session, window), restore the modes,
write the answer marker (`relay.MarkerGo`, `ESC ] 7193 ; tower-standby-go
BEL`), then continue as above. A standby nobody used exits after 12h.

## Standbys

```go
type standbys struct {
    byHost map[string]*standby // towerd id → standby
    ...
}
type standby struct {
    key     string
    sess    *relay.Session
    ready   bool
    env     []string // the loop's environment, tower's variables left out
    modes   relay.Modes
    backoff time.Duration
}
```

- **Refresh** on every view change (`watch`), every 2s, and when one is
  used or dies: ask `standby`, drop those whose key is no longer offered or
  whose host is the current one, start those missing (not while backed
  off).
- **Use**: prepare's key equals the standby's, it is ready, its env and
  modes match the terminal's now. Send the go line; wait for the answer
  marker for `300ms + 2 × SlowRTT` (or `TOWER_STANDBY_TIMEOUT`); on timeout
  `Kill` it and open a new session.
- A standby that dies before ready backs its host off (1s doubling to a
  minute); one not ready in 20s counts as dead.

## Concurrency

The loop's main goroutine runs the state machine. During an attach:
the attach (process wait or relay), `wait-switch`, `watch` (fallback
release, standby refresh), and the heartbeat ticker run as goroutines that
report to the main goroutine through channels. Standbys are managed by one
goroutine of their own; the main goroutine takes one with a request on a
channel, so the set has a single owner.
