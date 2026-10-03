package loop

import "golang.org/x/sys/unix"

func getTermios(fd int) (*unix.Termios, error) { return unix.IoctlGetTermios(fd, ioctlGet) }

func setTermios(fd int, t *unix.Termios) error { return unix.IoctlSetTermios(fd, ioctlSet, t) }

// rawMode turns echo and line editing off, keeping output processing, so
// the go line arrives byte by byte and is not echoed.
func rawMode(t *unix.Termios) {
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.IEXTEN
	t.Iflag &^= unix.ICRNL | unix.INLCR | unix.IXON
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
}
