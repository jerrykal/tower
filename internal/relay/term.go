package relay

import (
	"golang.org/x/sys/unix"
)

// Modes are a terminal's modes (termios).
type Modes struct{ t unix.Termios }

// GetModes reads the modes of the terminal open on fd.
func GetModes(fd int) (Modes, error) {
	t, err := unix.IoctlGetTermios(fd, ioctlGetModes)
	if err != nil {
		return Modes{}, err
	}
	return Modes{*t}, nil
}

// SetModes sets the modes of the terminal open on fd, at once.
func SetModes(fd int, m Modes) error {
	return unix.IoctlSetTermios(fd, ioctlSetModes, &m.t)
}

// Raw returns m in raw mode, the way ssh sets its terminal for a session
// with a pty: no input translation, flow control, signals, echo or line
// editing, no output processing, reads return as soon as a byte arrives.
// The character size and parity are left as they are, as ssh leaves them.
func (m Modes) Raw() Modes {
	t := m.t
	t.Iflag |= unix.IGNPAR
	t.Iflag &^= unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL |
		unix.IXON | unix.IXANY | unix.IXOFF | rawIflagExtra
	t.Lflag &^= unix.ISIG | unix.ICANON | unix.ECHO | unix.ECHOE |
		unix.ECHOK | unix.ECHONL | unix.IEXTEN
	t.Oflag &^= unix.OPOST
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return Modes{t}
}

// stateBits are the local-mode bits the kernel sets and clears by itself
// (pending re-print of input, output being flushed). Two terminals the
// user set up alike can differ in them at any moment.
const stateBits = unix.PENDIN | unix.FLUSHO

// Same reports whether m and o are the same modes, apart from what raw
// mode changes and the kernel's own state bits. A terminal whose modes
// were taken before a client made it raw is the same as itself raw.
func (m Modes) Same(o Modes) bool {
	a, b := m.Raw().t, o.Raw().t
	a.Lflag &^= stateBits
	b.Lflag &^= stateBits
	return a == b
}

// GetSize reads the window size of the terminal open on fd.
func GetSize(fd int) (rows, cols int, err error) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Row), int(ws.Col), nil
}

// SetSize sets the window size of the terminal open on fd (either side of
// a pty). The terminal's foreground process group gets SIGWINCH when the
// size changes.
func SetSize(fd int, rows, cols int) error {
	return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
}
