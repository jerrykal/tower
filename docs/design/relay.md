# relay

The terminal side of a remote attach: terminal modes, ptys, running ssh on
a pty of the loop's own, and relaying the terminal to it byte-exact, with
the loop's own writes placed between escape sequences.

## Terminal modes and sizes

```go
type Modes struct{ t unix.Termios }

func GetModes(fd int) (Modes, error)
func SetModes(fd int, m Modes) error
func (m Modes) Raw() Modes                  // cfmakeraw, as ssh sets it
func (m Modes) Same(o Modes) bool           // ignoring raw-mode flags and kernel state bits (PENDIN, …)
func GetSize(fd int) (rows, cols int, err error)
func SetSize(fd int, rows, cols int) error
```

`term_darwin.go` and `term_linux.go` hold the ioctl numbers.

## Ptys

```go
type Pty struct{ Master *os.File; Slave *os.File; Name string }
func OpenPty() (*Pty, error) // /dev/ptmx, grant, unlock, name
```

## Sessions

```go
type Session struct {
    Cmd    *exec.Cmd
    Pty    *Pty
    Out    *os.File // read end of ssh's stdout pipe
    ...
}

func Start(argv []string, env []string, modes Modes, rows, cols int) (*Session, error)
```

ssh runs with the pty's slave as stdin, stderr and controlling terminal
(`Setsid`, `Setctty`), and a pipe as stdout. The pty gets the terminal's
modes and size before ssh starts, so ssh's pty request carries them.

```go
func (s *Session) ReadUntil(marker []byte, timeout time.Duration) error // standby: wait for the shim's marker, stripped
func (s *Session) Send(line []byte) error                               // the go line
func (s *Session) Relay(t *Terminal) (exit int, err error)              // until ssh exits
func (s *Session) Terminate()                                           // SIGTERM to ssh
func (s *Session) Kill()                                                // SIGKILL (a stopped ssh loses SIGTERM)
```

## The terminal

```go
type Terminal struct {
    In, Out *os.File
    ...
}

func (t *Terminal) Write(b []byte)   // the loop's own writes: between sequences when relaying
func (t *Terminal) Hold() uint64      // ESC[?2026h; returns the hold's number
func (t *Terminal) Release(n uint64)  // ESC[?2026l unless a newer hold was made
```

**Relay.** Three copies while a session has the terminal:

1. terminal → pty master, using `poll` on the terminal's fd so a byte is
   read only when there is one, and the copy stops without having taken a
   byte meant for the next client;
2. stdout pipe → terminal, and pty master → terminal (ssh's own messages);
3. SIGWINCH → `SetSize` on the pty.

Output passes through a **sequence tracker**, a byte-at-a-time state
machine (ground, ESC, CSI, OSC/DCS/APC/PM/SOS strings ending in BEL or ST,
UTF-8 continuation) that knows whether the bytes written so far end at a
boundary. The loop's own writes wait in a small queue and go out at the
first boundary; one that has waited 50ms on an unterminated sequence goes
anyway. With no relay running (a local attach), `Write` writes at once.

**Throughput.** One goroutine per direction, 32 KB buffers, no extra copy
on the output path: the tracker scans the buffer and the writer cuts it
only to insert a queued write. A terminal that stops reading blocks the
output goroutine, which stops reading the pipe, so ssh blocks; memory stays
flat.

## Concurrency

The output goroutine owns the tracker and the terminal's writes during a
relay; `Terminal.Write` hands it the bytes through a channel. Outside a
relay `Terminal.Write` takes a mutex and writes directly. The input
goroutine is stopped through a pipe that `poll` also watches.
