# ui

The dashboard: tower's own terminal UI, in Bubble Tea v2 and Lip Gloss v2.
It runs in two places with one model:

- **the popup** (`tower` inside tmux): `prefix o` or `M-o` runs it in a
  `display-popup` for the pressing client (`TOWER_CLIENT`); typed at a
  prompt, `tower` opens that popup for its own client
  (`display-popup -E -c <client>`), so tower never nests tmux;
- **the loop's picker** (outside tmux): on the attach loop's terminal
  before an attach or after an error. It returns the chosen target to the
  loop instead of acting on it.

It reads only its own machine's towerd: `view` for the rows, `watch` for
changes, `act` for kill, rename, new, capture, dup and switch.

## v0.0.1 in two stages

1. **Step 2: the picker** (this note).
2. **Step 4: Atlas.** The column dashboard (hosts › sessions and dirs ›
   windows › layout preview), the finder, the add-host picker, and parity
   with the fzf session picker it replaces (zoxide dirs, the git-root
   filter, rename, a named new session, grouped duplicate, kill). It
   replaces the picker's layout and keeps its model, actions and live
   updates. A design note of its own comes with step 4.

## The picker

```
sessions> bra▏                                                  3/12
B  bravo          2w   now   󰧟
B  banana         1w   3h
C  bravo-logs     1w   2d
──────────────────────────────────────────────────────────────────────
B:bravo  ($1)  1:fish  2:nvim
<the active pane of the selected session's active window, captured>
──────────────────────────────────────────────────────────────────────
hand-off → B:bravo                    ⏎ attach  - prev  ^x kill  esc quit
```

- **Rows**: every session of every host, reachable hosts first, then by
  recency; one line each: host name, session name, window count (`2w`),
  age (`now`, `3m`, `2h`), markers (current `󰧟`, previous `-`, bell,
  activity, other clients), and a host's status when it is not up
  (`stalled`, `down: <reason>`, `not connected`). A host with no sessions
  shows one row: `<host>  (no sessions)` (`^n` creates one there). The
  prompt is `sessions>`; typing filters fzf-style (characters in order,
  case-insensitive, spaces ignored) over `host name` and the session name.
- **Preview**: the selected row's `host:session  ($id)` and its windows
  from the view at once, then the active pane's capture (`act capture`)
  when it arrives; previews travel only for the selected row, and a
  capture older than the selection is dropped.
- **Keys**: `⏎` attach, `-` / `.` previous / current (with an empty query;
  otherwise they type), `^x` kill, `^r` rename (prompt), `^n` new session
  on the row's host (prompt: a name, empty for tmux's default), `^w` the
  selected session's windows as rows (`⏎` attaches that window; `^w` back),
  `↑ ↓ ^j ^k ^p` move, `esc` quits (in
  `tower dash` outside tmux: attach to the last target, or quit), `^c`
  clears the query, then quits.
- **Status line** (bottom): the outcome of the last action (`kill on B:
  done`, `starting tmux on N…`, the error), or what `⏎` would do
  (`switch-client →`, `hand-off →`).

## Model

```go
type Model struct {
    dash   proto.Dash           // the last view read
    gen    uint64               // its watch generation
    rows   []row                // derived: filtered, sorted
    cursor rowKey               // the row under the cursor, by identity
    moved  bool                 // the user moved since the last rebuild
    query  []rune
    hidden map[rowKey]time.Time // rows whose kill is in flight
    note   string
    mode   mode                 // list, windows, prompt
    ...
}
```

- **Identity, not position.** The cursor is a row key (host id + session
  id [+ window id]); after any update it goes to that key's new index, or
  to its neighbour when the row is gone.
- **Live.** A `watch` command waits on towerd and returns a message when
  the generation moves; the model then reads `view` and rebuilds its rows,
  at most once per 50ms in a burst. Reads run as commands, so typing is
  never blocked, and the rows swap in one update. `TOWER_LIVE=0` turns it
  off.
- **Kill does not wait.** `^x` hides the row at once (the hidden set
  survives every rebuild until the answer), sends `act kill`, and the
  answer removes the hide and sets the note; a failure brings the row back
  with the error.
- **Fresh re-resolve.** `⏎` acts on the row's ids as they are in the
  latest view: a row gone since it was drawn says `selection is gone`
  instead of acting.
- **Hand-off** (`⏎` on another server, popup): `act switch` with a nonce.
  On `ended` the popup waits for its client to go (the loop ends it);
  without it, it holds the frame on its client's tty (`#{client_tty}`,
  unless it cannot write there) and runs `detach-client -t <client> -E
  'exit 42'`. A client no loop owns gets `⏎ on another host needs the
  attach loop (run tower outside tmux)`.
- **Same server**: `switch-client -c <client> -t <id>` (and `select-window`
  for a window), then quit.

## The loop's side

```go
// Pick runs the picker on the loop's terminal and returns what ⏎ chose.
func Pick(ctx context.Context, c *client.Client, o PickOptions) (Choice, error)

type PickOptions struct {
    Loop string // the loop's id: its view marks current and previous
    Note string // why the picker is up ("B restarted since it was listed; pick again")
    Dash bool   // tower dash: esc means "attach to the last target"
}

type Choice struct {
    Target proto.Ref
    Last   bool // esc in tower dash: the loop attaches to its last target
}
```

`Pick` returns `ErrQuit` on `esc` (or `Last` in `tower dash`); the loop then
exits or attaches. In the picker `⏎` on any host is a target for the loop,
which prepares it; no switch is stored.

## Scripted entry points

Hidden subcommands run the dashboard's own code paths without the TUI, for
the scenario suite and scripting. Each uses `TOWER_CLIENT` like the popup.

| Command | Does |
| --- | --- |
| `tower _ui rows` | prints the rows as the picker would show them, one per line |
| `tower _ui goto <host> <session> [<window index>]` | `⏎` on that row |
| `tower _ui kill <host> <session>` | `^x` |
| `tower _ui rename <host> <session> <new>` | `^r` |
| `tower _ui new <host> <name>` | `^n` |
| `tower _ui preview <host> <session id>` | the preview text |

Each action prints `<op> on <host>: done` or the error, and exits non-zero
on an error.

## Concurrency

Bubble Tea's update loop owns the model. Calls to towerd run as commands
(goroutines) and come back as messages.
