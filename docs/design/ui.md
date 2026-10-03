# ui

The dashboard: tower's own terminal UI, in Bubble Tea v2 and Lip Gloss v2.
It runs in three places with one model and one set of actions:

- **the popup** (`tower` inside tmux): `prefix o` or `M-o` runs it in a
  `display-popup` for the pressing client (`TOWER_CLIENT`); typed at a
  prompt, `tower` opens that popup for its own client
  (`display-popup -E -c <client>`), so tower never nests tmux;
- **the loop's picker** (outside tmux): on the attach loop's terminal
  before an attach or after an error. It returns the chosen target to the
  loop instead of acting on it;
- **the scripted entry points** (`tower _ui …`): the same actions without
  a terminal, for the scenario suite and scripting.

It reads only its own machine's towerd: `view` for the rows, `watch` for
changes, `act` for kill, rename, new, capture and switch.

## v0.0.1 in two stages

1. **Step 2: the picker** (this note).
2. **Step 4: Atlas.** The column dashboard (hosts › sessions and dirs ›
   windows › layout preview), the finder, the add-host picker, and parity
   with the fzf session picker it replaces (zoxide dirs, the git-root
   filter, rename, a named new session, grouped duplicate, kill). It
   replaces the picker's layout and keeps its model, actions and live
   updates. A design note of its own comes with step 4.

## API

```go
// Towerd is what the dashboard asks of its towerd; Calls is it over a
// client.Client (one local call each).
type Towerd interface {
    View(ctx, proto.ViewArgs) (proto.Dash, error)
    Watch(ctx, gen uint64) (uint64, error)
    Act(ctx, proto.Request) (proto.Ack, error)
}

// Tmux runs one command on the dashboard's own server (tmux.Server).
type Tmux interface { Run(ctx, args ...string) (string, error) }

// Conn is one dashboard's link to the world.
type Conn struct {
    Towerd Towerd
    Tmux   Tmux   // nil in the loop's picker
    Client string // TOWER_CLIENT
    Loop   string // the loop's picker: its loop id
    Pick   bool   // the loop's picker: ⏎ returns the target
}

func Dial(ctx, e *config.Env, client string) (*Conn, error) // ensure towerd, take its tmux binary
func Run(ctx, c *Conn) error                                 // the popup
func Open(ctx, sv Tmux, self string, env map[string]string) error // typed `tower`: open the popup
func Script(ctx, c *Conn, args []string, out io.Writer) error     // tower _ui …
func Pick(ctx, c *client.Client, o PickOptions) (Choice, error)   // the loop's picker
```

`cmd/tower` wires them: `tower` and `tower dash` with `$TMUX` set run the
dashboard when `TOWER_CLIENT` is set (the popup) and `Open` otherwise;
outside tmux they call the attach loop through the `attachLoop` hook.
`Open` reads `#{client_pid}:#{client_created}:#{client_name}` with
`display-message -p` and runs `display-popup -E -c <client> -w 80% -h 80%`
(`ui.PopupSize`; towerd's `M-o` binding should use the same size).
When `TOWER_TMUX` is unset, the server comes from `$TMUX`'s socket: `-L
<name>` when it lies in tmux's own socket directory, else `-S <path>`, so a
`tower` typed in a `tmux -L work` session reaches that server's towerd.

## The picker

```
sessions> bra▏                                                  3/12
▌ B  bravo          2w   now   󰧟
  B  banana         1w   3h
  C  bravo-logs     1w   2d   down: timed out
──────────────────────────────────────────────────────────────────────
B:bravo  ($1)  1:fish  2:nvim
<the active pane of the selected session's active window, captured>
──────────────────────────────────────────────────────────────────────
hand-off → B:bravo                    ⏎ attach  - prev  ^x kill  esc quit
```

- **Rows**: every session of every host in three bands (reachable, then
  not reachable, then turned off), by recency within a band across hosts,
  as `tmux ls` would order one server. One line each: host name, session
  name, window count (`2w`), age (`now`, `3m`, `2h`, `4d`), markers
  (current `󰧟`, previous `-`, `󰍺 N` other clients, bell `󰂞`, activity
  `󰐰`), and a host's status when it is not up (`stalled`, `connecting`,
  `down: <reason>`, `failed: …`, `dup: …`, `off`). A host with no sessions
  shows one row, `<host>  (no sessions)`, last in its band (`^n` creates
  one there). The view's own note (`home A not connected (2m)`) shows on
  the prompt line. The prompt is `sessions>`; typing filters fzf-style
  (characters in order, case-insensitive, spaces ignored) over `host name`
  and the session name, keeping the rows' order; matched characters are
  marked. Columns are sized over every row, so they do not move while
  typing.
- **Current and previous** come from the loop in the view (the client's
  loop, or the picker's own); a client no loop owns marks its own session
  (from tmux) as current.
- **Preview**: the selected row's `host:session  ($id)` and its windows
  from the view at once, then the active pane's capture (`act capture`,
  `capture-pane -e`) when it arrives. One capture is in flight at a time; an
  answer for a row no longer selected is dropped and the selected row's
  asked for, so holding `↓` sends one request per answer, not per row. A
  capture is asked again only when the selection or its active window
  changes.
- **Keys**: `⏎` attach; `-` / `.` (with an empty query; otherwise they
  type) are `⏎` on the previous / current session's row; `^x` kill; `^r`
  rename (prompt, prefilled); `^n` new session on the row's host (prompt:
  a name, empty for tmux's default); `^w` the selected session's windows as
  rows (`⏎` attaches that window, `^x` / `^r` act on it; `^w` back, with
  the list's query and cursor); `↑ ↓ ^j ^k ^p pgup pgdn` move; `backspace`,
  `^u` edit the query; `esc` quits (in `tower dash` outside tmux: attach to
  the last target), except while a `⏎` is in flight; `^c` clears the
  query, then quits. In a prompt `⏎` confirms and `esc` / `^c` cancel.
- **Status line** (bottom): the outcome of the last action (`kill on B:
  done`, `starting tmux on N…`, the error), or what `⏎` would do
  (`switch-client → A:alpha`, `hand-off → B:bravo`, `attach → …` in the
  picker, `hand-off to B needs the attach loop`, `C is down: …`), and the
  keys when they fit.
- **Look**: Rosé Pine. This machine's host name rose, others foam, hosts
  not up muted with their status in love (red); the cursor's row on an
  iris `▌` bar over an overlay background; matches rose, bold, underlined.
  In tmux the program draws in true colour (tmux maps it to what the
  terminal outside supports).

## Model

```go
type Model struct {
    view   proto.Dash           // the last view read
    rows   []row                // derived: the mode's rows, hidden ones left out
    shown  []row                // the rows the query matches
    cursor rowKey               // the row under the cursor, by identity
    query  []rune
    mode   mode                 // list, windows, prompt
    hidden map[rowKey]hide      // rows whose kill is in flight
    note   string
    ...
}
```

- **Identity, not position.** A row key is host id + server instance +
  session id [+ window id]: ids are valid only within one instance. After
  any rebuild the cursor goes to its key's new index, or, when the row is
  gone, to the nearest row after it (in the old order) that is still
  there, else before it. Typing puts the cursor on the first match, as fzf
  does; nothing else moves it.
- **Live.** A `watch` command waits on towerd and returns a message when
  the generation moves; the model then reads `view` and rebuilds, at most
  once per 50ms in a burst (a change after a quiet spell is read at once,
  later ones on a timer), with one read in flight at a time so answers
  land in order. Reads run as commands, so typing is never blocked, and
  the rows swap in one update. A failed watch (towerd restarting) retries
  after 500ms. `TOWER_LIVE=0` turns it off.
- **Kill does not wait.** `^x` hides the row at once and sends `act kill`.
  The hide survives every rebuild until the answer; a failure removes it,
  so the row comes back with the error; a success keeps it until a view
  read started after the answer lands (a read already in flight may
  predate the kill), then the row is gone from the view itself. Every
  action's answer triggers a read (read your writes); after `new` the
  cursor goes to the session made.
- **Fresh re-resolve.** `⏎` reads the view afresh (one local call) and
  resolves the row's ids in it: a renamed session is still reached; a row
  gone since it was drawn says `selection is gone`; a server restarted
  since says `<host> restarted since it was listed; pick again`; a host
  not up says `<host> is <status>`. Kill and the prompts refuse a host not
  up at once.
- **Hand-off** (`⏎` on another server, popup): `act switch` with a nonce
  and the client. On `ended` the popup waits for its client's pid to go
  (at most 3s; the loop ends it), so it never closes first. Without it, it
  writes the frame hold (`ESC[?2026h`) to its client's tty
  (`#{client_tty}`, read as the popup opens) unless `TOWER_SYNC=0`,
  `TOWER_TEST_NOTTY`, or the tty cannot be written, and runs
  `detach-client -t <client> -E 'exit 42'`. A detach that fails while the
  client is still there releases the hold and shows the error; one whose
  client went meanwhile is no failure. A client no loop owns (`Owned`
  false) gets `⏎ on another host needs the attach loop (run tower outside
  tmux)`. Test hooks: `TOWER_TEST_GEN` (claim a generation),
  `TOWER_TEST_CRASH=after-switch` (exit once the switch is stored).
- **Same server**: `switch-client -c <client> -t <id>` (with `;
  select-window -t <window id>` for a window, in the same tmux call), then
  quit.
- **Requests** carry a fresh id and a deadline of `TOWER_ACK_TIMEOUT`
  (5s); the call waits a second longer so towerd's own refusal, which says
  why, is what the user sees.

## The loop's side

```go
// Pick runs the picker on the loop's terminal and returns what ⏎ chose.
func Pick(ctx context.Context, c *client.Client, o PickOptions) (Choice, error)

type PickOptions struct {
    Loop string // the loop's id: its view marks current and previous
    Note string // why the picker is up ("B restarted since it was listed; pick again")
    Dash bool   // tower dash: esc means "attach to the last target"
    Input  io.Reader // the terminal; nil: stdin
    Output io.Writer // nil: stdout
}

type Choice struct {
    Target proto.Ref
    Last   bool // esc in tower dash: the loop attaches to its last target
}
```

`Pick` reads `view{Loop}`, returns `ErrQuit` on `esc` (or `Last` in `tower
dash`) and on `^c` with an empty query, and the context's error when the
context ends. In the picker `⏎` on any reachable host is a target for the
loop, re-resolved in a fresh view first, which the loop prepares; no switch
is stored. The note shows in the status line until the first key. The
loop releases its frame hold before calling `Pick`.

## Scripted entry points

Hidden subcommands run the dashboard's own code paths without the TUI, for
the scenario suite and scripting. Each uses `TOWER_CLIENT` like the popup.
Hosts are named by label or towerd id, sessions by name or id.

| Command | Does |
| --- | --- |
| `tower _ui rows` | prints the rows as the picker shows them (unfiltered, no styling), one per line |
| `tower _ui goto <host> <session> [<window index>]` | `⏎` on that row (fresh view, switch-client or hand-off) |
| `tower _ui kill <host> <session>` | `^x` |
| `tower _ui rename <host> <session> <new>` | `^r` |
| `tower _ui new <host> [<name>]` | `^n`; first prints `starting tmux on <host>…` for a host with no server |
| `tower _ui preview <host> <session>` | the preview: the header line from the view, then the capture |

Each action prints `<op> on <host>: done` (towerd's note after it in
parentheses, e.g. `(already gone)`); on an error it prints `tower: <op> on
<host>: <error>` on stderr and exits 1.

## Start-up

The popup is on the hand-off's critical path, so its start is kept short:

- `Dial` takes towerd's resolved tmux binary from the ensure `status`, and
  the opener passes it (`TOWER_TMUX_BIN`), the machine key (`TOWER_MKEY`),
  the server (`TOWER_TMUX`) and every other `TOWER_*` variable in the
  popup's shell command, which runs the tower binary by path: no version
  manager's shim and no `ioreg` on the way.
- The first view is read before the program starts, so the first frame has
  the rows; the client's tty and session are read after it.
- Inside tmux the colour profile is given, which keeps Bubble Tea's
  detection (`tmux info` through `PATH`) off the start.
- Bubble Tea flushes frames on a ticker; at its maximum of 120 fps a frame
  (the first, and each key's echo) waits at most 8.3ms.

Measured on a pty against a stand-in towerd (macOS, M-series): process
start to the first frame on the terminal about 18ms median, of which the
process exec is about 7–8ms, tower's own work (ensure, view, model, first
draw) about 0.7ms, and the renderer's tick about 8ms. Before the colour
profile was given it was about 40ms. Timing marks: `dash: open` (the
typed opener), `dash: start`, `dash: connected`, `dash: view`, `dash:
frame`, `dash: enter`.

## Concurrency

Bubble Tea's update loop owns the model. Calls to towerd and tmux run as
commands (goroutines) and come back as messages; the `Conn` caches the
client's tty under a mutex, since the opening read and a hand-off may both
ask for it.
