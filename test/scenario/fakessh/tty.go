package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), getTermios)
	return err == nil
}

// makeRaw puts fd in raw mode as ssh does and returns the old modes.
func makeRaw(fd int) (*unix.Termios, error) {
	old, err := unix.IoctlGetTermios(fd, getTermios)
	if err != nil {
		return nil, err
	}
	t := *old
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, setTermios, &t); err != nil {
		return nil, err
	}
	return old, nil
}

func restore(fd int, t *unix.Termios) {
	if t != nil {
		unix.IoctlSetTermios(fd, setTermios, t)
	}
}

// copyModes gives dst the modes and window size of src.
func copyModes(src, dst int) {
	if t, err := unix.IoctlGetTermios(src, getTermios); err == nil {
		unix.IoctlSetTermios(dst, setTermios, t)
	}
	if ws, err := unix.IoctlGetWinsize(src, unix.TIOCGWINSZ); err == nil {
		unix.IoctlSetWinsize(dst, unix.TIOCSWINSZ, ws)
	}
}
