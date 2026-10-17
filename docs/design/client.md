# client

Every tower process except towerd itself reaches towerd through this
package: one call per connection, and `Ensure`, which makes sure a current
towerd answers.

## API

```go
type Client struct { Env *config.Env; Version string }

func (c *Client) Call(ctx context.Context, op string, args, result any) error
func (c *Client) Ensure(ctx context.Context) (*proto.Status, error)
func (c *Client) Dial(ctx context.Context) (net.Conn, error) // the bridge's raw connection
```

`Call` dials the socket, writes one `proto.Call` line, reads one
`proto.Reply` line, and closes. A dial error is `ErrNoTowerd`; a reply
error is returned as an error carrying its text.

## Ensure

1. Ask `status`. If a towerd answers:
   - same version or newer: use it;
   - older, or another build of our version (`proto.Replaces`: a rebuilt
     development binary): call `stop` with `IfOlderThan` = our version
     (and `Bridge` from a bridge), wait for the socket to go quiet, then
     start ours (an older binary never downgrades a newer towerd);
   - but a remote-only towerd that is `served` runs its home's build,
     which the home's next bridge would only put back: anything but a
     bridge fails at once (`towerd <v> here runs a home's build (<exe>),
     not tower <v>: run that build, or tower stop first`), and towerd
     refuses such a `stop` itself, for a caller from before this rule.
2. If nobody answers: start `tower towerd --tmux …` detached (`Setsid`,
   stdio to `/dev/null`, environment as ours minus nothing), ask again every
   10ms, start another every 100ms. The lock decides between starters.
3. **Wedged.** If the lock is held and the socket accepts but `status` has
   not answered for 1s more, read `towerd.pid`, check the process is a
   towerd (its command line) and holds the lock, SIGKILL it, and start
   another.
4. Give up after 5s with the last error.

`Ensure` returns the `status` answer: the towerd's id, version, machine key
and tmux binary, which the caller uses instead of resolving them again.

## Following a home's build

`cmd/tower/follow.go`, before every command but those that never follow
(`towerd`, `_keep`, `_ensure`, `_ui`, `attach`, which a towerd or a home
runs by exact path, and `version` and `help`): when the towerd the
command acts on is remote-only and `served` (the stream of a home of its
build is connected), and its version is not this build's, tower execs
the towerd's binary (`exe`, its own path with links resolved, so it
names one build) with the same arguments. That is the home's build,
which the home installed and started there, so tower typed on a remote
behaves as the home's own, whatever tower PATH finds first. A command a
later build added follows too.

- **Which towerd.** The one the command acts on: the pane's server
  (`$TMUX`) for `tower` and `dash`, else `--tmux`, else `TOWER_TMUX`,
  else the default server.
- **Not followed:** a towerd that plays home (this machine is one you work
  from: Ensure's rules apply, so a newer build typed there upgrades it);
  one not served, or of this version; a key binding, which runs towerd's
  binary already (`TOWER_CLIENT` set); `TOWER_FOLLOW=0`. If the follow
  fails (status slower than 300ms, the binary gone), the command runs as
  itself and Ensure refuses to replace the towerd.
- **Once.** The exec'd tower gets `TOWER_FOLLOWED=<the version it came
  from>`, never follows again, and takes the variable out of its
  environment at once (an empty one too), so a tmux server it starts does
  not hand it to every pane. `tower status` says it: `tower <v> on PATH
  followed towerd's build <path>`.
- **The machine key** loaded for the check goes into `TOWER_MKEY`, so the
  command's own load and the exec'd build do not compute it again (a
  subprocess on macOS).
- **Cost:** one `status` round trip per typed command, about 0.15ms on
  Linux; none on a binding.
- A towerd from before `exe` answers without it: nothing to follow. The
  tower first on PATH must be new enough to follow; the `v0.0.0`
  placeholder release is not.
- `tower host …` followed into an older build edits `hosts.toml` with
  that build's fields; keys only a newer build knows would be dropped.
  Every build that follows has the same fields today.
