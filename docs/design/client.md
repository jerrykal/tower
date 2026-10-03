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
   - older: call `stop` with `IfOlderThan` = our version, wait for the
     socket to go quiet, then start ours (an older binary never downgrades
     a newer towerd).
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
