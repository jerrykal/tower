# ui

The dashboard: tower's own terminal UI, in Bubble Tea v2 and Lip Gloss v2.
It runs in two places with one model:

- **the popup** (`tower` inside tmux): `prefix o` or `M-o` runs it in a
  `display-popup` for the pressing client (`TOWER_CLIENT`); typed at a
  prompt, `tower` opens that popup for its own client;
- **the loop's picker** (outside tmux): on the attach loop's terminal
  before an attach or after an error, returning the chosen target to the
  loop instead of acting on it.

It reads only its own machine's towerd: `view` for the rows, `watch` for
changes, `act` for kill, rename, new, capture, dup and switch.

## v0.0.1 in two stages

1. **Step 2: the picker.** One list of every host's sessions (host,
   session, windows count, age, markers), filtered as you type, with the
   status note on top. Keys: `⏎` attach (switch-client on the same server,
   hand-off to another), `-` / `.` previous / current (with an empty
   query), `^x` kill, `^r` rename, `^n` new session on the row's host, `^w`
   the windows of the selected session, `esc` quit (in `tower dash`:
   attach to the last target, or quit). Live updates, the cursor kept on
   its row, kill without waiting.
2. **Step 4: Atlas.** The column dashboard (hosts › sessions and dirs ›
   windows › layout preview), the finder, the add-host picker, and parity
   with the fzf session picker it replaces (zoxide dirs, the git-root
   filter, rename, a named new session, grouped duplicate, kill). A design
   note of its own comes with step 4.

## Model

```go
type Model struct {
    dash   proto.Dash   // the last view read
    gen    uint64       // its watch generation
    rows   []row        // derived: filtered, sorted
    cursor rowKey       // the row under the cursor, by identity
    query  []rune
    hidden map[rowKey]time.Time // rows whose kill is in flight
    note   string
    mode   mode         // list, prompt, confirm
    ...
}
```

- **Identity, not position.** The cursor is a row key (host id + session id
  [+ window id]); after any update the cursor goes to that key's new index,
  or to its neighbour when it is gone.
- **Live.** A `watch` command waits on towerd and returns a message when
  the generation moves; the model then reads `view` and rebuilds rows. A
  burst costs one read per 50ms at most. Typing is never blocked by a read:
  reads run as commands, and the rows swap in one update.
- **Kill does not wait.** `^x` hides the row at once (the hidden set
  survives every reload until the answer), sends `act`, and the answer
  removes the hide and sets the note; a failure brings the row back with
  the error.
- **Hand-off.** `⏎` on another server: `act switch` with a nonce; on
  `ended` the popup waits for its client to go (the loop ends it); without
  it, the popup holds the frame on the client's tty and detaches the
  client itself (`detach-client -t <client> -E 'exit 42'`).

## Concurrency

Bubble Tea's update loop owns the model. Calls to towerd run as commands
(goroutines) and come back as messages.
