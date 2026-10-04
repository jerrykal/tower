# ui

The dashboard: tower's own terminal UI, in Bubble Tea v2, drawn on a cell
canvas of its own with plain SGR sequences (see Start-up for why not Lip
Gloss). It runs in three places with one model and one set of actions:

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
changes, `act` for kill, rename, new, dup, panes, capture and switch, and
`retry` to connect a host that is down now. On the home's machine it also
edits the host list (`internal/hosts`).

## v0.0.1 in two stages

1. **Step 2: the picker.** One list of every session, typed to filter.
2. **Step 4: Atlas** (this note). The column dashboard (hosts › sessions
   and dirs › windows › layout preview), the finder, the add-host picker,
   and parity with the fzf session picker it replaces (zoxide dirs, the
   git-root filter, rename, a named new session, grouped duplicate,
   kill). It kept the picker's model, actions, live updates, hand-off and
   `ui.Pick`; the finder took the picker's place as the screen the
   dashboard opens on.

## API

```go
// Towerd is what the dashboard asks of its towerd; Calls is it over a
// client.Client (one local call each).
type Towerd interface {
    View(ctx, proto.ViewArgs) (proto.Dash, error)
    Watch(ctx, gen uint64) (uint64, error)
    Act(ctx, proto.Request) (proto.Ack, error)
    Retry(ctx, host string) error // retry: that host connects now
}

// Tmux runs one command on the dashboard's own server (tmux.Server).
type Tmux interface { Run(ctx, args ...string) (string, error) }

// HostList is hosts.toml on the home's machine (*hosts.List).
type HostList interface {
    Load() ([]config.Host, error)
    Aliases() []string                                  // ssh config Host names
    Add(ctx, config.Host, step func(hosts.Step)) ([]transport.Check, error)
    Check(ctx, config.Host, step func(hosts.Step)) []transport.Check
    Remove(name string) error
    Rename(old, name string) error
    SetOn(name string, on bool) error
}

// Conn is one dashboard's link to the world.
type Conn struct {
    Towerd Towerd
    Tmux   Tmux     // nil in the loop's picker
    Client string   // TOWER_CLIENT
    Loop   string   // the loop's picker: its loop id
    Pick   bool     // the loop's picker: ⏎ returns the target
    Hosts  HostList // nil: hosts are not edited here
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
`display-message -p` and runs `display-popup -E -c <client>` at
`proto.PopupSize` (towerd's `M-o` binding uses the same size). When
`TOWER_TMUX` is unset, the server comes from `$TMUX`'s socket: `-L <name>`
when it lies in tmux's own socket directory, else `-S <path>`, so a
`tower` typed in a `tmux -L work` session reaches that server's towerd.

## Files

| File | Holds |
| --- | --- |
| `ui.go` | `Run`, `Pick`, the program's options |
| `conn.go`, `popup.go` | `Conn`, `Dial`, `Calls`, the client's tty; `Open` |
| `actions.go` | `Conn`'s side of every action: ⏎ (fresh re-resolve, switch-client, hand-off), kill, rename, new, dup, panes, capture |
| `world.go` | `rowKey`, `item` and `world`: what a view lists, in the dashboard's order |
| `model.go` | the `Model`, its messages, live reads, rebuild |
| `columns.go` | the columns' lists, selection memory, moving, `-` and `.` |
| `find.go`, `match.go` | the finder's rows and scoring; the columns' filter |
| `keys.go`, `input.go` | keys per mode; a line of input |
| `commands.go`, `prompt.go`, `addhost.go` | what each action does to the model and the command it sends; prompts and confirm; the add-host picker and checks |
| `layout.go`, `draw.go`, `preview.go`, `help.go`, `mouse.go` | widths and geometry; drawing; the layout preview and captures; the key reference; the mouse |
| `canvas.go`, `style.go`, `text.go` | the cell grid and SGR; Rosé Pine and the glyphs; fitting text into cells |
| `script.go` | `tower _ui` |

## The screen

```
 FIND  bra                                                            3/12
──────────────────────────────────────────────────────────────────────────
  B   bravo  󰓩 1:fish   󰘬 main*                         󰌘 connected · 14ms
────────────────────────────────────────────┬─────────────────────────────
 [f] every host                             │ layout · 1:fish · 2 panes
                                            │
▌ B     bravo                     󰓩 2 󰂞  1m │ ┌─ %0 ──────┐┌─ %3 ────────┐
  B     cobra › 2:bravo-logs              3h │ │ fish      ││ htop        │
  A     ~/src/bravo                    main │ └ active ·… ┘└──── 40×24 ──┘
                                            │ active pane
                                            │ ┌──────────────────────────┐
                                            │ │ <capture-pane -e>        │
──────────────────────────────────────────────────────────────────────────
  ^j ^k move   ⏎ attach   tab windows   ^l columns   ^x kill   esc quit
```

- **Top line**: the mode's pill (`FIND`, `NORMAL`, `SEARCH`, a prompt's
  label, `CONFIRM`, `ADD HOST`), then what the mode edits: the query, the
  focused column's query, a prompt's input with what an empty ⏎ takes or
  why the last one was refused (`✗ banana is taken on B`), the question.
  On the right the view's note (`home A not connected (2m)`) and matches
  over total.
- **Breadcrumb**: host › session (or dir) › window under the cursor, then
  the branch (`*` when dirty), `grouped with <session>`, `also on N other
  clients`; the host's link on the right (`local server`, `connected ·
  14ms`, `connecting…`, `installing tower…`, `not responding`,
  `unreachable · last seen 2h ago · <reason>`, a running or failed host
  check). When it does not fit, the link shrinks to its icon, then the
  clients, group and branch notes go, then the longest name loses its
  middle. Without the preview it shows the pane picked with `J` `K`.
- **The columns** (`^l`, or the screen `f` returns to): `[1] hosts`,
  `[2] sessions` with the host's zoxide dirs under them, `[3] windows`,
  then the layout preview.
- **Footer**: the keys of the mode and the selection, leaving out keys
  that would only be refused (no `x remove` on the local server, `⏎
  reconnect` on a host that is down, `⏎ new session` on a dir); hints that
  do not fit drop whole from the right, keeping `? help`. On the right the
  latest message, a pending `g`, or `q quit`.

### Opening

The popup and the loop's picker open in the **finder**, so typing a name
and ⏎ moves there as the picker did. There `esc` closes the dashboard (in
`tower dash` outside tmux: attach to the last target), `^c` clears the
query and then closes, and `^l` shows the cursor's row in the columns.
`f` from the columns opens the finder afresh, and `esc` then goes back to
the columns. With an empty query, `-` and `.` select the previous and
the current session (⏎ then goes there) and `?` shows the keys.

### The finder

Every host's sessions in one list, each once, most recently attached
first: reachable hosts' sessions, then their zoxide dirs (git roots; `^g`
every entry), then unreachable hosts' cached sessions and dirs, dimmed.

- **Words.** The query splits at spaces and `:`; each word goes to the
  host, session or window it matches best, in any order, and every word
  must match something. A substring scores more than a scattered match
  (the tightest span, at most three times the word); among substrings the
  whole name, then its start, then a word start, each nearer the front.
  A number next to `:` names a window by its number only
  (`gb200:train-llm:2`); a plain number also may, below a name holding it.
  Ties keep recency.
- **Windows** a word is a substring of are listed under their session
  (`└ 2:train`), best first, three and then `└ +N more`; a scattered
  match lists a window only when no name has that word as a substring. A
  session whose own name matched no word and that has one listed window
  folds into `session › 2:window`. A grouped session's windows are listed
  under the session the group is named after.
- **The cursor** goes, when the query changes, to the best row of the top
  session: a window row scores its own words plus the session's, and wins
  only when strictly better (`tens` lands on `tensorboard`). Then it stays
  on its row by identity while rows come and go.
- `tab` shows every window of the session (the ones the query does not
  match dimmed) or folds them back; it resets when the query changes. ⏎ on
  `+N more` is `tab`. ⏎ on a session attaches it at its remembered window.

### The columns

- **hosts**: OS logo (rose for this machine, foam for others), label, `󰧟`
  for the client's host, a bell or activity marker rolled up, and on the
  right the session count, or how many entries the sessions column's
  query matches (gold; hosts with none dimmed), a spinner while it
  connects, installs or is being checked, its last-seen age in red when
  down, `off`, or a red `!` when a check failed. With only this machine
  listed it offers `a add a host`. Hosts are ordered as the dashboard
  opens (reachable, then down, then turned off, each by most recent
  attach) and the order is kept while it is open; hosts that appear later
  join the bottom.
- **sessions**: most recently attached first, `󰌹` for a grouped one, `󰧟`
  for the client's; on the right, each in a slot as wide as the widest in
  the list, `󰍺 N` other clients, a bell or activity, and the age (`new`
  for a session this dashboard made and nobody attached yet). A linked
  worktree's session named `<repo><sep><branch>` dims the repo part, which
  shrinks first. Then `dirs · git roots ^g` and the dirs with their branch.
  A host loading says so; a host with none says `no sessions · n makes
  one`; an unreachable host's header says `cached 2h ago`.
- **windows**: the number, `󰓩`, the name, `󰧟`; a bell or activity, and the
  pane count. A dir has `no session yet`.
- **layout**: the selected window's panes at their real proportions
  (capture's `Panes`), with command, path and size, the picked pane in
  iris; under it the picked pane's capture, its last lines. A dir shows its
  branch and the session ⏎ would make; a host its OS, tmux, tower and
  status.

### Width and long names

Column widths come from everything the view holds, not what is on screen:
each column takes the 90th percentile of its rows' widths, within hosts
16–30, sessions 24–60, windows 18–44; they are taken as the dashboard
opens and only grow. The preview takes the rest, at least 40; when the
columns want more, the wider of sessions and windows gives way first.
Below 120 columns the preview goes: sessions keep their width and windows
take the rest, or both shrink in proportion. The focused column borrows up
to 8 cells from the preview while its visible rows are cut. The finder
gives its list its widest row (at least 44) and the preview the rest when
that leaves 40. Under 40×8 the dashboard says it needs more room.

Every cut counts terminal cells: names lose their middle (sliding to show
the first matched character when the cut would hide it), worktree names
their repo part first, paths shorten as fish's prompt does
(`~/w/c/a/backend-api`, then `…/backend-api-gateway`), inputs scroll
sideways behind a `…`. Match highlights carry over to what remains.

### Look

Rosé Pine (main). The focused column's cursor is an iris `▌` on the
overlay colour; the other columns' remembered rows a grey `▌` on the
surface colour. Matches rose, bold, underlined; love (red) only for
problems. Glyphs are JetBrains Mono Nerd Font's: tmux session `` and dir
`` (as `session-picker.sh`), window `󰓩`, OS logos fa-apple, fa-linux,
fa-windows, and the markers listed under `?`. All borders are square; a
`┃` thumb on a divider marks a column that overflows. In tmux the program
draws in true colour (tmux maps it to what the terminal outside supports).

## Keys

| Mode | Keys |
| --- | --- |
| finder | type, `⌫`, `^u`; `^j ^k ↓ ↑ ^n ^p` move; `⏎` attach (a dir: a new session there; `+N more`: `tab`); `tab` windows; `^l` the columns; `^x` kill; `^r` rename; `^g` dirs; `^c` clear, then back (or close); `esc` back (or close) |
| columns | `h l ^h ^l tab shift-tab` column (tab wraps; windows skipped for a dir); `1 2 3`; `j k ^j ^k`; `gg G`; `^d ^u pgup pgdn`; `/ i` search; `f` finder; `⏎` (host: open, reconnect, check again; session: attach at its remembered window; window: attach with the picked pane; dir: new session); `J K` pick a pane; `D` duplicate; `- .` select previous / current; `n` new session (or window in the windows column); `r ^r` rename (dir: a named session; host: its label); `x ^x` kill (host: remove); `a` add a host; `space` host off / on; `^g` dirs; `esc ^c` clear the column's query, then every query, then quit; `q` quit; `?` keys |
| search | typing filters the focused column (in order, case and spaces ignored); `^j ^k ↓ ↑ ^n ^p`; `^h ^l tab shift-tab` column with its own query; `← → ^b ^f ^a ^e`; `⌫ ^d ^u`; `⏎ ^r ^x ^g` as in the columns; `^c` clear; `esc` back, keeping queries |
| prompt | `⏎` apply (refused names keep it open with why); `esc ^c` cancel; editing as in search; in a session name `.` and `:` become `_`; input is trimmed |
| confirm | `y` yes; any other key no |
| add host | type; `^j ^k`; `⏎` pick; `esc ^c` cancel |
| help | any key closes |

Pasted text goes into whatever takes text, line breaks as spaces (in the
columns it starts a search). Errors stay in the footer until the next key;
outcomes for 2.4s; what is in flight until its outcome.

**Mouse**: a click selects a row and focuses its column; a second click on
it within 400ms is ⏎; the wheel moves the column under the pointer without
moving focus. Prompts, confirm, the picker and help ignore it.

## Actions

- **⏎** reads the view afresh and resolves the row's ids in it: a renamed
  session is still reached; a row gone since it was drawn says
  `selection is gone`; a server restarted since says `<host> restarted
  since it was listed; pick again`; a stalled host `<host> is not
  responding`. On a host that is down (or failed) ⏎ retries that host
  now (`retry`, its backoff afresh) and says so.
- **Same server**: `switch-client -c <client> -t <id>` (`; select-window`,
  `; select-pane` in the same tmux call), then quit.
- **Hand-off** (another server, popup): `act switch` with a nonce and the
  client, as in protocol.md. On `ended` the popup waits for its client's
  pid to go (at most 3s); without it, it writes the frame hold to its
  client's tty (`#{client_tty}`, read as the popup opens) unless
  `TOWER_SYNC=0`, `TOWER_TEST_NOTTY`, or the tty cannot be written, and
  runs `detach-client -t <client> -E 'exit 42'`. A detach that fails while
  the client is still there releases the hold and shows the error. A
  client no loop owns gets `⏎ on another host needs the attach loop (run
  tower outside tmux)`. Test hooks: `TOWER_TEST_GEN`,
  `TOWER_TEST_CRASH=after-switch`.
- **Kill** asks first, saying what is at stake: windows and panes, up to
  two commands other than a shell still running (`panes`, asked as the
  question opens and added when it answers), the other clients it
  detaches, that a grouped session's windows stay with the group, that a
  session's last window takes the session, that you are attached to it.
  `y` hides the row at once and sends `act kill` (a last window hides its
  session too); the hide survives every rebuild until the answer; a
  failure brings the row back with the error; a success keeps it until a
  view read started after the answer lands. The cursor moves to the
  nearest row.
- **New**: `n` asks a name (empty: tmux's), on the host in `~`, or a window
  in the session's directory; the new row is selected once a read has it.
  ⏎ on a dir makes a session named after it (`.` dropped at the start,
  `.` and `:` as `_`) in that dir and attaches it; a taken name opens the
  prompt with the first free `<name>_<n>` (the fzf picker's spelling);
  `r` on a dir asks the name first.
- **Duplicate** (`D`): `act dup` named `<name> 2` (or the next free
  `<name> <n>`), then ⏎ on what it made; a session of that name already
  in the session's group is the duplicate made before and is attached as
  it is, while one of that name outside the group is passed over.
- **Rename**: prefilled; a session name another session on the host has
  is refused with why.
- **Requests** carry a fresh id and a deadline of `TOWER_ACK_TIMEOUT` (5s;
  15s for a new session on a host with no server); the call waits a second
  longer so towerd's own refusal, which says why, is what the user sees.
  Every answer triggers a view read (read your writes).

In the loop's picker ⏎ on any reachable host returns the target, which the
loop prepares; a dir or a duplicate is made first, then returned.

## The host list

On the home's machine (`view.Self` is the view's home) and with a
`HostList`, the hosts column edits `hosts.toml` through `internal/hosts`,
the code `tower host` runs:

- `a` opens the picker: the ssh config's aliases not in the list (Host
  lines without patterns, following `Include`), filtered as typed, and a
  last row `+ ssh <query>` whenever the query is not exactly an alias. An
  alias is added under its own name unless taken; a raw target, or a taken
  alias, asks for a name with `⏎ for <default>` (the target's host part,
  `-2`, `-3`, … when taken).
- **Names** are unique regardless of case; a name equal to an ssh alias is
  only for the host that alias reaches; `:` and spaces become `-`.
- The checks of `tower host add` run in order (ssh with BatchMode, tmux
  3.2+, the OS, the tower binary, installed when missing), within 15s, a
  spinner and the current step in the host's row and the breadcrumb; the
  host is saved after them, failed or not, and the home reloads. A failed
  check marks the host `!` with the reason; ⏎ on it checks again. Closing
  the dashboard during the checks adds nothing.
- `x` removes a host after `y` (its sessions keep running), `r` renames
  its label, `space` turns it off or on. This machine cannot be removed or
  turned off.

## Model

```go
type Model struct {
    view   proto.Dash       // the last view read
    w      *world           // what it lists: hosts, sessions, dirs, windows
    order  []string         // hosts' order, by name, taken as it opens
    hidden map[rowKey]hide  // rows whose kill is in flight
    mode   mode             // normal, search, find, prompt, confirm, add host, help
    focus  col
    cs     [3]colState      // each column's query and scroll
    sel    selection        // memory: host, each host's entry, each session's window, each window's pane
    find   finder           // the finder's query, rows and cursor
    cap    capState         // captures in flight and kept
    ...
}
```

- **Identity, not position.** A row key is host id + server instance +
  session id [+ window id], or host + path for a dir. The columns remember
  the selected host, each host's entry, each session's window; a query
  that hides the remembered row puts the cursor on the first match
  without forgetting it; memory changes only when a cursor moves. After
  a rebuild a remembered row that went hands its place to the nearest row
  after it in the old list that is still there, else before it; the
  finder's cursor the same.
- **Live.** A `watch` command waits on towerd and returns when the
  generation moves; the model then reads `view` and rebuilds, at most once
  per 50ms in a burst, one read in flight at a time. Reads run as
  commands, so typing is never blocked. A failed watch retries after
  500ms. `TOWER_LIVE=0` turns it off.
- **Previews.** One capture in flight at a time, for the window (and
  picked pane) the preview shows; its answer is kept (64 kept), so moving
  back shows it at once, and the selected one is asked for next. A capture
  is asked again only when the selection changes. The breadcrumb shows the
  window from the view before the capture lands.

## The loop's side

```go
func Pick(ctx context.Context, c *client.Client, o PickOptions) (Choice, error)

type PickOptions struct {
    Loop string // the loop's id: its view marks current and previous
    Note string // why the picker is up
    Dash bool   // tower dash: esc means "attach to the last target"
    Input  io.Reader
    Output io.Writer
}

type Choice struct {
    Target proto.Ref
    Last   bool // esc in tower dash: the loop attaches to its last target
}
```

`Pick` opens in the finder and returns `ErrQuit` on `esc` (or `Last` in
`tower dash`) and on `^c` with an empty query, and the context's error
when the context ends. The note shows in the footer until the first key.
The loop releases its frame hold before calling `Pick`. A picked pane
travels in `Ref.Pane`.

## Scripted entry points

Each uses `TOWER_CLIENT` like the popup. Hosts are named by label or
towerd id, sessions by name or id.

| Command | Does |
| --- | --- |
| `tower _ui rows` | every session, one per line: host, name, windows, age, marks |
| `tower _ui find <query…>` | the finder's rows for the query, `>` on the cursor's |
| `tower _ui goto <host> <session> [<window index>]` | ⏎ on that row |
| `tower _ui kill <host> <session> [<window index>]` | `x`, `y` |
| `tower _ui ask-kill <host> <session> [<window index>]` | the question `x` asks, panes included |
| `tower _ui rename <host> <session> <new>` | `r` |
| `tower _ui new <host> [<name>]` | `n`; first prints `starting tmux on <host>…` for a host with no server |
| `tower _ui open <host> <dir>` | ⏎ on a dir: a session named after it there (first free name), attached |
| `tower _ui dup <host> <session>` | `D` |
| `tower _ui preview <host> <session>` | the header line from the view, then the capture |

Each action prints `<op> on <host>: done` (a note after it in
parentheses); on an error it prints `tower: <op> on <host>: <error>` on
stderr and exits 1.

## Start-up

The popup is on the hand-off's critical path, so its start is kept short:

- `Dial` takes towerd's resolved tmux binary from the ensure `status`, and
  the opener passes it (`TOWER_TMUX_BIN`), the machine key, the server and
  every other `TOWER_*` variable in the popup's shell command, which runs
  the tower binary by path.
- The first view is read before the program starts, so the first frame
  has the rows; the client's tty and session are read after it.
- No colour detection runs `tmux info`: Bubble Tea is given the true
  colour profile inside tmux, and the UI draws plain SGR sequences, not
  Lip Gloss (whose package-level writer detects a profile at init; LC07).
- Bubble Tea flushes frames on a ticker; at its maximum of 120 fps a frame
  (the first, and each key's echo) waits at most 8.3ms.
- Building the lists, the finder and the first frame is pure work on the
  view (a few hundred microseconds for the suite's views).

Measured by U08 (a pty of 120×35 against the home's towerd, 15 runs,
macOS, M-series): process start to the first frame about 23–26ms median,
a key to its echo 8ms. The step-2 picker measured the same way gives
24ms and 8ms: Atlas costs nothing measurable at start. (The 14ms quoted
for the picker before was a stand-in towerd with no tmux behind it.)
Timing marks: `dash: open`, `dash: start`, `dash: connected`, `dash:
view`, `dash: frame`, `dash: enter`, `dash: kill`, `dash: <op> answered`.

## Concurrency

Bubble Tea's update loop owns the model. Calls to towerd, tmux and the
host list run as commands (goroutines) and come back as messages; a host's
checks report each step on a channel the model reads one message at a
time. The `Conn` caches the client's tty under a mutex.

## Open

- A pane picked with `J` `K` is selected by a local switch only: the
  hand-off and the loop's attach carry `Ref.Pane`, but the loop's attach
  command selects the window, not the pane.
- ⏎ on a host that is down retries every down host (`netchange`); there is
  no call to retry one.
- `?` and `-`/`.` type into a finder query that is not empty.
