package relay

import (
	"strconv"

	"golang.org/x/sys/unix"
)

const (
	ioctlGetModes = unix.TCGETS
	ioctlSetModes = unix.TCSETS
	rawIflagExtra = unix.IUCLC
)

// grantUnlock unlocks the slave of the pty master fd. Linux's devpts
// grants the slave to the opener already.
func grantUnlock(fd int) error {
	return unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0)
}

// ptsName returns the path of the slave of the pty master fd.
func ptsName(fd int) (string, error) {
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		return "", err
	}
	return "/dev/pts/" + strconv.FormatUint(uint64(n), 10), nil
}
