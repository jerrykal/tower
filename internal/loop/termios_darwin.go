package loop

import "golang.org/x/sys/unix"

const (
	ioctlGet = unix.TIOCGETA
	ioctlSet = unix.TIOCSETA
)

// flushInput discards the input not read yet (FREAD), without waiting for
// output to drain, as TIOCSETAF would.
func flushInput(fd int) error { return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, 1) }
