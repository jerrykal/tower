# transport

How tower uses ssh: the options on every call, the commands for the
stream, attaches, standbys, probes and master resets, the classification of
failures, the control-socket sweep and the network watch. No stream logic
and no state about hosts beyond what ssh needs.

## API

```go
type SSH struct {
    Bin   string // TOWER_SSH or "ssh"
    CMDir string // control sockets
}

func (s *SSH) Options(h config.Host) []string // -o … for every call
func (s *SSH) Stream(h config.Host, remote string) *exec.Cmd   // ssh -T … host -- remote
func (s *SSH) Attach(h config.Host, remote string) []string    // argv: ssh -t … host -- remote
func (s *SSH) Probe(ctx context.Context, h config.Host) error  // ControlMaster=no … true
func (s *SSH) Exit(ctx context.Context, h config.Host) error   // -O exit
func (s *SSH) Run(ctx context.Context, h config.Host, remote string, stdin io.Reader) (string, error)

func RemoteCommand(tower string, args ...string) string // a shell line, every arg quoted

type Failure struct { Class, Reason string } // "down" or "failed", and the text with its fix
func Classify(host string, exit int, stderr string) Failure
func TailscaleCheck(line string) bool

func (s *SSH) CheckHost(ctx context.Context, h config.Host, tower string) []Check

func Sweep(dir string)                                  // remove control sockets nobody listens on
func WatchNet(ctx context.Context, every time.Duration, changed func())
```

## Options

`BatchMode=yes`, `ConnectTimeout=5`, `ServerAliveInterval=5`,
`ServerAliveCountMax=3`, `ControlMaster=auto`, `ControlPersist=10m`,
`ControlPath=<cm>/%C`, and `ObscureKeystrokeTiming=no` when the ssh binary
is 9.5 or later (asked once per binary with `ssh -V`) and the host does not
keep obscuring. Options come before the host, which comes before `--`; the
remote side is one shell line made by `RemoteCommand`, so no name or path
reaches ssh's own argument parsing.

## Failures

| Exit, stderr | Class | Reason |
| --- | --- | --- |
| 127, or `command not found` / `No such file` for the tower binary | failed | `tower is not installed on <host>` |
| `Host key verification failed`, `REMOTE HOST IDENTIFICATION HAS CHANGED` | down | host key: `ssh <host>` once to check it |
| `Permission denied` | down | authentication failed: load the key with `ssh-add`, or set one up with `ssh-copy-id` |
| `Could not resolve hostname` | down | cannot resolve host name |
| `Connection refused` | down | connection refused |
| `timed out` | down | connection timed out |
| a Tailscale SSH check banner | down | Tailscale SSH wants a check: run `ssh <host>` once |
| anything else | down | the last stderr line |

`Stream` callers read stderr line by line as it arrives and end the ssh on
a Tailscale check banner, which otherwise holds the connection.

## Checking a host

`CheckHost` is what `tower host add` (and the dashboard's add) runs, in
order: ssh with `BatchMode=yes`, tmux 3.2 or later, `uname -s`, and the
tower binary (`<tower> version`). The steps after ssh go in one remote
command on the one session, so a check costs a single connection. Each
`Check` carries what was found or the reason with its fix. stderr is read
as it arrives: a Tailscale check banner ends ssh at once instead of after
its 30s hold.

## Control sockets

`Sweep` dials each socket in the directory with a short timeout; it
removes a socket only if the dial is refused (`ECONNREFUSED`: nobody
listens). A timeout means a busy master and keeps it.

## Network watch

`WatchNet` lists the interface addresses every `every`, leaving out
link-local ones, and calls `changed` when the set differs from the last.
