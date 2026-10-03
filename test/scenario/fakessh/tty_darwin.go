package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	getTermios = unix.TIOCGETA
	setTermios = unix.TIOCSETA
)

func openPty() (master, slave *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := ioctl(fd, unix.TIOCPTYGRANT, 0); err != nil {
		unix.Close(fd)
		return nil, nil, err
	}
	if err := ioctl(fd, unix.TIOCPTYUNLK, 0); err != nil {
		unix.Close(fd)
		return nil, nil, err
	}
	var name [128]byte
	if err := ioctl(fd, unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		unix.Close(fd)
		return nil, nil, err
	}
	n := 0
	for n < len(name) && name[n] != 0 {
		n++
	}
	m := os.NewFile(uintptr(fd), "ptmx")
	s, err := os.OpenFile(string(name[:n]), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}

func ioctl(fd int, req uint, arg uintptr) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), arg)
	if e != 0 {
		return e
	}
	return nil
}
