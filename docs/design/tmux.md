# tmux

Everything tower says to tmux: which binary to run, how to quote, one-shot
commands, and the control-mode client towerd keeps on its server.

## Binaries

```go
func Resolve(name string) string // absolute path past version-manager shims, or name
```

For each match of `name` on `PATH`: a plain binary is the answer; a shim
(under a `shims` directory of mise or asdf) is asked which binary it runs
(`mise which <name>`, `asdf which <name>`), and passed over if it cannot
say. towerd resolves `tmux` once and hands the path to every other process
in its `status` answer and in `TOWER_TMUX_BIN`.

```go
func Bin() string // TOWER_TMUX_BIN if absolute and executable, else Resolve("tmux")
```

## Quoting

- `Quote(s)`: one tmux command-language argument in single quotes, `'`
  written as `'\''`… tmux's parser has no backslash escapes inside single
  quotes, so a single quote ends the string, an escaped one is written
  outside it, and the string reopens.
- `Literal(s)`: `#` doubled, for any string tmux expands as a format
  (`display-message`, `-F`, a session name given to `new -s` is not one).

Names are never interpolated into a shell command line: commands go to tmux
as an argument vector, or through the control client in tmux's own command
language.

## One-shot commands

```go
type Server struct { Bin string; Args []string } // Args: -L name / -S path
func (s Server) Command(args ...string) *exec.Cmd
func (s Server) Run(ctx context.Context, args ...string) (string, error)
```

## Formats

Listings use a tab between fields and put the free-text field (a name)
last, so a name with tabs survives; one record per line. Names cannot hold
a newline (tmux replaces control characters in names), and the parser
checks the field count.

## Control client

```go
type Control struct { ... }

func Attach(s Server, session string) (*Control, error) // tmux -C attach -f no-output,ignore-size -t session
func (c *Control) Do(cmd string) (Reply, error)         // one command line, matched FIFO
func (c *Control) DoMany(lines []string, d time.Duration) ([]Reply, error) // a batch written at once
func (c *Control) Bytes() int64                          // read from tmux so far
func (c *Control) Notes() <-chan Note                   // %session-changed, %message, %exit …
func (c *Control) Name() string                          // its client name
func (c *Control) Pid() int
func (c *Control) Close()                                // detach-client -t <own name>, then wait
```

- **FIFO matching.** tmux answers commands in the order it received them,
  each between `%begin <time> <number> <flags>` and `%end`/`%error`. `Do`
  writes the line and appends a waiter to a queue under one mutex; the
  reader pops the head for each `%begin`. Lines between are the reply.
  Notifications outside a block go to `Notes` (a buffered channel; when it
  is full the oldest is dropped and a `Lost` flag is set, which the watch
  takes as "re-read everything").
- **Batches.** `DoMany` writes several lines in one write and waits for
  each reply in order: towerd's re-read costs one round trip to the server.
- **No output.** `-f no-output` keeps pane output off the client;
  `ignore-size` keeps its size out of window sizing.
- **Ending.** `Close` detaches the client by name (a bare `detach-client`
  from a control client whose session is gone detaches the user's most
  recent client), then waits for the process. If tmux exits first (`%exit`),
  `Do` fails with `ErrClosed`.
