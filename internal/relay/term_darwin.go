package relay

import (
	"bytes"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ioctlGetModes = unix.TIOCGETA
	ioctlSetModes = unix.TIOCSETA
	rawIflagExtra = 0
)

// grantUnlock grants and unlocks the slave of the pty master fd.
func grantUnlock(fd int) error {
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		return err
	}
	return unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0)
}

// ptsName returns the path of the slave of the pty master fd.
func ptsName(fd int) (string, error) {
	var buf [128]byte // TIOCPTYGNAME fills a buffer of this size
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", errno
	}
	if i := bytes.IndexByte(buf[:], 0); i >= 0 {
		return string(buf[:i]), nil
	}
	return string(buf[:]), nil
}
