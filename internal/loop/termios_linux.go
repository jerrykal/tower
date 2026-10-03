package loop

import "golang.org/x/sys/unix"

const (
	ioctlGet = unix.TCGETS
	ioctlSet = unix.TCSETS
)
