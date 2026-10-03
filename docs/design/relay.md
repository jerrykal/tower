# relay

The terminal side of a remote attach: terminal modes, ptys, running ssh on
a pty of the loop's own, and relaying the terminal to it byte-exact, with
the loop's own writes placed between escape sequences.

## Terminal modes and sizes

```go
type Modes struct{ t unix.Termios }

func GetModes(fd int) (Modes, error)
func SetModes(fd int, m Modes) error
func (m Modes) Raw() Modes                  // raw mode as ssh sets it for a session with a pty
func (m Modes) Same(o Modes) bool           // ignoring raw-mode flags and kernel state bits
func GetSize(fd int) (rows, cols int, err error)
func SetSize(fd int, rows, cols int) error  // either side of a pty
```

- **Raw** is ssh's raw mode, not `cfmakeraw`: `IGNPAR` on; `ISTRIP INLCR
  IGNCR ICRNL IXON IXANY IXOFF` (and `IUCLC` on Linux) off; `ISIG ICANON
  ECHO ECHOE ECHOK ECHONL IEXTEN` off; `OPOST` off; `VMIN` 1, `VTIME` 0.
  The character size and parity stay as they were, as ssh leaves them.
- **Same** compares both sides made raw, with the kernel's own state bits
  (`PENDIN`, `FLUSHO`) left out, so a terminal's modes taken before a
  client made it raw match the same terminal raw, and a bit the kernel
  flips does not make a standby look made for another terminal.

`term_darwin.go` and `term_linux.go` hold the ioctl numbers and the pty
calls (`TIOCPTYGRANT`/`TIOCPTYUNLK`/`TIOCPTYGNAME` on macOS,
`TIOCSPTLCK`/`TIOCGPTN` on Linux). No cgo.

## Ptys

```go
type Pty struct{ Master, Slave *os.File; Name string }
func OpenPty() (*Pty, error) // /dev/ptmx, grant, unlock, name; both ends close-on-exec, blocking
func (p *Pty) Close() error  // slave first: on macOS closing a master blocks while a read on it is blocked
```

## Sessions

```go
const (
    MarkerReady = "\x1b]7193;tower-standby-ready\a" // the shim waits for its go line
    MarkerGo    = "\x1b]7193;tower-standby-go\a"    // the shim read it and starts tmux
)

func Start(argv, env []string, modes Modes, rows, cols int) (*Session, error)

func (s *Session) ReadUntil(marker []byte, timeout time.Duration) (before []byte, err error)
func (s *Session) Send(line []byte) error           // line + "\n" to the pty: the go line
func (s *Session) Relay(t *Terminal) (exit int, err error)
func (s *Session) Terminate()                       // SIGTERM to ssh; the relay stops taking input
func (s *Session) Kill()                            // SIGKILL (a stopped ssh loses SIGTERM); same
func (s *Session) SetSize(rows, cols int) error
func (s *Session) Done() <-chan struct{}            // ssh has exited (a standby that died)
func (s *Session) ExitCode() int                    // exit code, or 128 + signal
func (s *Session) Pid() int
func (s *Session) PtyName() string
func (s *Session) Close()                           // kill if running, wait, release the fds
```

ssh runs with the pty's slave as stdin, stderr and controlling terminal
(`Setsid`, `Setctty`), and a pipe as stdout. The pty gets the given modes
and size before ssh starts, so ssh's pty request carries them. The
session keeps the pty's master, the pipe's read end, and two pipes of its
own: one whose write end closes when ssh exits (so a `poll` sees the exit)
and one closed by `Terminate` or `Kill` (so the relay's input copy stops at
once: keys typed between a switch and ssh's exit belong to the next
client).

- **ReadUntil** reads ssh's stdout until the marker, strips it and returns
  what came before it; what came after is kept and relayed first. It ends
  early with `ErrExited` when ssh exits, else `ErrTimeout`.
- **Exit status.** The attach's exit is ssh's: its exit code, or 128 plus
  the signal that ended it, as a shell reports it.

## The terminal

```go
const (
    SyncBegin = "\x1b[?2026h"
    SyncEnd   = "\x1b[?2026l"
    ClientRestore = "…" // see below
    PlaceWait = 50 * time.Millisecond
)

func NewTerminal(in, out *os.File) (*Terminal, error)
func (t *Terminal) Write(b []byte)    // the loop's own writes: between sequences when relaying
func (t *Terminal) Hold() uint64      // SyncBegin; returns the hold's number
func (t *Terminal) Release(n uint64)  // SyncEnd unless a newer hold was made, or n was released
func (t *Terminal) RestoreClient()    // ClientRestore, for a client the loop hung up
```

`Write` never waits for a relayed terminal: during a relay it queues the
bytes; outside one it writes them before returning.

**ClientRestore** is what a tmux client writes as it exits, for a remote
client the loop hung up (its own never arrives), less synchronized output
since the loop writes it inside a hold: `ESC[r` (scroll region),
`ESC[m` (attributes), `ESC(B` (G0 ASCII), `ESC[?1l ESC>` (cursor keys and
keypad normal), `ESC[H ESC[2J` (clear the alternate screen about to be
left), `ESC[0 q` (cursor shape), `ESC[?25h` (cursor shown), `ESC[?1000l
ESC[?1002l ESC[?1003l ESC[?1006l ESC[?1005l` (mouse), `ESC[?2004l`
(bracketed paste), `ESC[?1004l` (focus events), `ESC[>4m` (extended keys),
`ESC[?1049l` (out of the alternate screen, the shell's screen and cursor
back).

## The relay

`Relay` runs three copies while a session has the terminal, all on raw
descriptors with `poll` (it must know a byte is there before reading it,
and wait on several descriptors and a deadline at once):

1. **Input** (a goroutine): terminal → pty master. It reads only once
   `poll` says the terminal has input, and stops without reading when the
   relay ends, `Terminate`/`Kill` is called or ssh exits, so it never takes
   a byte meant for the next client.
2. **Output** (the calling goroutine): stdout pipe and pty master (ssh's own
   messages) → terminal, with the loop's queued writes put in. It polls
   the pipe, the pty, a wake pipe (`Write` wakes it) and the exit pipe, with
   the oldest queued write's deadline as the timeout. Once ssh has exited
   it drains the pipe and the pty, then ends.
3. **Size** (a goroutine): the terminal's size to the pty at the start and
   on every SIGWINCH.

The terminal's modes are the caller's: the loop makes the terminal raw
once and restores it at its end; `Relay` touches neither.

**Placement.** Output passes through a sequence tracker, a state machine
modelled on the DEC/ANSI parser terminals share (ground, ESC and its
intermediates, CSI, OSC strings ending in BEL or ST, DCS/SOS/PM/APC
strings ending in ST, CAN/SUB aborting any of them, UTF-8 characters; C1
bytes are parts of characters in UTF-8). A zero-width joiner glues the
next character to its cluster, so an emoji ZWJ sequence counts as one.
With no queued write the output goes out whole. With one, the output
goes out up to the end of the sequence or character it is in, then the
queued writes, then the rest; a write that has waited `PlaceWait` on an
unterminated sequence goes anyway (the tracker follows it too, so it
stays in step with the terminal).

The tracker follows only the bytes after the last ESC, CAN or SUB of each
read: those three leave a terminal's parser in the same state whatever
state it was in, so terminal output costs a short tail scan, and text
without any escape a pass at about 1.3 GB/s.

**Ending.** A relay cut off inside a sequence (ssh killed in the middle of
an OSC) would leave the terminal's parser inside it, swallowing what comes
next; the relay then writes CAN, which ends any sequence, before the
writes still queued.

**Throughput.** 32 KB reads, no copy on the output path: the tracker scans
the buffer and the writer cuts it only to insert a queued write. A
terminal that stops reading blocks the output copy, which stops reading
the pipe, so ssh blocks; memory stays flat.

Measured on an M5 MacBook against the same command given the terminal (a
pty read by another process), in the package's tests:

| | given the terminal | relayed |
| --- | --- | --- |
| keystroke echo, median (LS07) | 7–12µs | +4–8µs |
| 200 MiB coloured log `cat` (LS08) | 220 MB/s | 217 MB/s, 0.5s relay CPU |
| 200 MiB wide characters and binary | 221 MB/s | 217 MB/s, 0.5s relay CPU |
| build log, a write a line | 54 MB/s | 96 MB/s |
| key to host under a flood, median | – | 15µs |
| key echo under a 40 MB/s flood, median | – | 55µs (unpaced: 0.4ms, behind the buffered output) |
| a terminal not reading for 2.5s | – | the host's writes stop; relay heap flat (±2 KiB), 4ms CPU per 0.5s |

Through the fake ssh to a host with a pty (the LS08 scenario), relayed
against the fake ssh given the terminal: the 200 MiB cats at 142–154 MB/s
relayed and 125–165 plain, set by the fake ssh, which spends 2.6–3.4s of
CPU on them against the relay's 0.7–0.8s; the build log 53 MB/s relayed,
41 plain; keys reach the host in 31–37µs relayed, 22–25µs plain; echo at
40 MB/s 114–133µs relayed, 95–113µs plain. Unpaced, the echo comes back
in 2–3.5ms relayed against about 110µs plain: the fake ssh is then no
longer the slowest stage, so its 2 MiB window fills and the key waits
behind it, as it would behind a real ssh channel's window.

## Concurrency

The output goroutine owns the tracker and the terminal's writes during a
relay. `Terminal.Write` queues the bytes under the terminal's mutex and
wakes the output goroutine through a pipe (a goroutine blocked in `poll`
cannot wait on a channel); outside a relay it writes them directly under
the same mutex. The input goroutine is stopped through pipes `poll` also
watches. A session's `ReadUntil`, `Relay` and `Close` are for one
goroutine at a time; `Terminate`, `Kill`, `Done` and `SetSize` for any.
