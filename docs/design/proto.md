# proto

The wire types every part of tower shares: the model (hosts, sessions,
windows, targets), the home ↔ remote stream messages, and the local calls
over towerd's unix socket. `proto` holds data and pure helpers only: no I/O,
no goroutines, no dependencies on other tower packages.

## Versions

- `proto.Min`, `proto.Max`: the protocol range this build speaks (1–1).
  `Negotiate(a, b Range) (int, error)` picks the highest common version or
  fails with both ranges in the message.
- `proto.Newer(a, b string) bool` compares tower versions (`0.0.1`,
  `0.0.1-dev+abc`): a dev build sorts after its base tag; two different dev
  builds of one base compare unequal by commit, and either replaces the
  other (a towerd of another build is replaced, never trusted to match).

## The model

```go
type Ref struct {        // a target: what a loop attaches to
    Host    string // towerd id
    Name    string // host label at the home (display only)
    Inst    string // tmux server instance "pid:start_time"
    Session string // "$3"
    Window  string // "@7", optional
    Pane    string // "%1", optional
    Label   string // session name when the ref was made (display, notes)
}

type Session struct {
    ID, Name, Path string
    Ago      int64  // ms since session_last_attached, on the owner's clock
    Attached int    // clients attached, towerd's control client excluded
    Group    string // session_group when grouped
    Windows  []Window
    Git      *Git   // the session's directory, when it is in a git repo
}

type Git struct {
    Branch string // the branch, or a short commit (7) when detached
    Dirty  bool
    Repo   string // a linked worktree: the main worktree's directory name
}

type Dir struct {        // a zoxide directory with no session yet
    Path string // as the host spells it, ~ for its home
    Root bool   // a git repo's root (a .git file or directory)
    Git  *Git   // for a root
    Net  bool   // on a network mount: listed, never checked
}

type Pane struct {       // for the layout preview and a kill's question
    ID, Window          string // "%3", "@1"
    Left, Top           int    // cells, as tmux lays the window out
    Width, Height       int
    Command, Path       string // pane_current_command; the directory, ~ for the host's home
    Active              bool
}

type Window struct {
    ID     string
    Index  int
    Name   string
    Panes  int
    Active bool
    Bell, Activity, Silence bool
}

type Client struct {     // a registered client seen on a server
    Name    string // tmux client name (its tty)
    Pid     int
    Session string
    Window  string
    Loop    string
    Gen     int
    Home    string // towerd id of the loop's home
}
```

A host as the home sees it:

```go
type Host struct {
    ID, Name  string
    Status    string // local, up, connecting, stalled, down, failed, dup, off
    Reason    string // why down/failed/dup, with the fix
    OS, Tmux, Version, MKey string
    Inst      string
    NoServer  bool
    Sessions  []Session
    Dirs      []Dir // zoxide directories with no session, most frecent first
    Seen      int64 // ms since last heard (cached hosts)
}

type Loop struct {
    ID        string
    Gen       int
    Cur, Prev Ref
    Seen      bool // the home has seen this attach's client: a loop's fallback frame release
}

type View struct {
    Seq   uint64
    Home  string // towerd id of the home
    Hosts []Host
    Loops []Loop
    Pad   string // TOWER_TEST_PAD only
}

type State struct {
    Seq      uint64
    Inst     string
    NoServer bool
    Sessions []Session
    Dirs     []Dir    // zoxide directories with no session
    Clients  []Client // only clients of the receiving home's loops
    Pad      string   // TOWER_TEST_PAD only
}
```

`Ago` fields are relative so no timestamp crosses machines; the receiver
notes when a message arrived and adds the time since when it shows an age.

## Stream messages

One JSON object per line; `t` names the type, the one matching field is
set, and unknown types are ignored by the receiver.

```go
type Msg struct {
    T     string    `json:"t"` // hello, state, view, exec, relay, ack, ping, pong, look
    Hello *Hello    `json:"hello,omitempty"`
    State *State    `json:"state,omitempty"`
    View  *View     `json:"view,omitempty"`
    Req   *Request  `json:"req,omitempty"`  // exec and relay
    Ack   *Ack      `json:"ack,omitempty"`
    Ping  *Ping     `json:"ping,omitempty"` // ping and pong
}

type Hello struct {
    Min, Max int     // the sender's protocol range
    Proto    int     // the remote's choice (answer only)
    ID, Name string  // sender's towerd id and label
    As       string  // the name the home uses for the remote
    Version  string
    MKey, OS, Tmux string // remote's answer
    Err      string  // the remote's refusal
}

type Ping struct { N uint64; Clock int64 } // Clock: sender's unix ms (pong)
```

`look` has no body: a dashboard opened, so the receiver refreshes its git
state and zoxide directories (see [towerd.md](towerd.md), Looks).

## Requests

Requests travel as `exec` (home → remote), `relay` (remote → home) and the
local `act` call; one shape serves all three.

```go
type Request struct {
    ID       string // unique per request; repeats are answered from memory
    Op       string // kill, rename, new, capture, panes, has, switch, dup
    Deadline int64  // unix ms in the clock of whoever holds the request now
    Target   Ref
    Kind     string // "session" or "window" (kill, rename, new, panes)
    Name     string // rename, new, dup
    Dir      string // new: start directory (~ is the target host's home)
    Client   string // switch: "pid:created:name" of the pressing client
    Loop     string // switch: resolved from the client's registration
    Gen      int
    Nonce    string
    From     string // towerd id of the asking machine
}

type Ack struct {
    ID     string
    OK     bool
    Err    string
    Note   string // e.g. "already gone", "starting tmux on alpha…"
    Ended  bool   // switch: the loop ended the old client
    Gone   bool   // has: the session does not exist (or no server)
    Text   string // capture
    Panes  []Pane // capture: the target window's; panes
    Ref    *Ref   // new, dup: what was created
}
```

## Local calls

towerd serves one call per connection on its unix socket: a request line,
a reply line. Long calls (`watch`, `wait-switch`, `prepare`) hold their
connection until they answer; a caller that goes away cancels them.

```go
type Call struct {
    Op      string          `json:"op"`
    Version string          `json:"v"`  // caller's tower version
    Args    json.RawMessage `json:"a,omitempty"`
}
type Reply struct {
    Err    string          `json:"err,omitempty"`
    Result json.RawMessage `json:"r,omitempty"`
}
```

| Op | Args → Result | Who calls |
| --- | --- | --- |
| `status` | `{Full}` → `Status` (id, version, pid, mkey, tag, tmux binary, roles; with `Full` the links, homes (id, name, as, live), loops, pending switches, clients, the watch, the dirs refresher (binaries, counts, the last refreshes' cost) and towerd's CPU time, its finished children included) | ensure, `tower status` |
| `stop` | `{IfOlderThan}` → `{}` | `tower stop`, an upgrading caller |
| `stream` | – → the connection becomes a stream | the bridge |
| `register` | `{Pid, Loop, Gen, Home, Inst}` → `{MKey, TmuxBin}` | the attach shim |
| `view` | `{Client}` → `Dash` (the view to show, the host it runs on, the client's loop and its home, notes) | dashboards |
| `watch` | `{Gen}` → `{Gen}` | dashboards, loops |
| `act` | `Request` → `Ack` | dashboards, `tower last` |
| `loop` | `LoopBeat{ID, Gen, Cur, Prev}` → `LoopAck{Last}` (also activates the home role) | the loop, every 5s |
| `loop-bye` | `{ID}` → `{}` | the loop on exit |
| `prepare` | `{Loop, Target}` → `Prepared{Gen, Local, Argv, Go, Key, Host}` | the loop |
| `after` | `{Loop, Gen, Code, Ended}` → `Next{Do, Target, Note}` with `Do` one of `handoff`, `picker`, `reconnect`, `exit` | the loop |
| `wait-switch` | `{Loop, Gen}` → `{Switch}` | the loop, while attached |
| `held` | `{Loop, Gen}` → `{End}` | the loop |
| `standby` | `{Loop}` → `[]Offer{Host, Key, Argv}` | the loop |
| `last` | `{Client}` → `LastResult{Local, Stored, Ended, Asker, Target, Note}` | `tower last` |
| `wake`, `netchange`, `reload` | – → `{}` (`reload` also activates the home role) | tests, `tower netchange`, `tower host` |
| `plant` | `Request{Loop, Gen, Nonce, Target}` → `{}`: a stored switch, only with `TOWER_TEST_HOOKS` | the scenario suite |

## Quoting

- `ShellQuote(s)`: single-quoted for a POSIX shell (the remote command line
  of ssh).
- tmux quoting lives in `tmux` (see [tmux.md](tmux.md)).
