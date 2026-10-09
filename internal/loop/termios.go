package loop

import "golang.org/x/sys/unix"

func getTermios(fd int) (*unix.Termios, error) { return unix.IoctlGetTermios(fd, ioctlGet) }

func setTermios(fd int, t *unix.Termios) error { return unix.IoctlSetTermios(fd, ioctlSet, t) }

// setTermiosFlush sets the modes and discards the input not read yet, in
// one step.
func setTermiosFlush(fd int, t *unix.Termios) error {
	return unix.IoctlSetTermios(fd, ioctlSetFlush, t)
}

// rawMode turns echo, line editing and the signal keys off, keeping
// output processing, so the go line arrives byte by byte and is not
// echoed, and a key typed meanwhile is only a byte.
func rawMode(t *unix.Termios) {
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.IEXTEN | unix.ISIG
	t.Iflag &^= unix.ICRNL | unix.INLCR | unix.IXON
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
}
