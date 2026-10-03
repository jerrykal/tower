package loop

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// testPty opens a pseudo-terminal pair for a test.
func testPty() (master, slave *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	ctl := func(req uint, arg uintptr) error {
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), arg); e != 0 {
			return e
		}
		return nil
	}
	var name [128]byte
	for _, step := range []struct {
		req uint
		arg uintptr
	}{{unix.TIOCPTYGRANT, 0}, {unix.TIOCPTYUNLK, 0}, {unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))}} {
		if err := ctl(step.req, step.arg); err != nil {
			unix.Close(fd)
			return nil, nil, err
		}
	}
	// Non-blocking, so the runtime polls it and read deadlines work.
	unix.SetNonblock(fd, true)
	m := os.NewFile(uintptr(fd), "ptmx")
	s, err := os.OpenFile(unix.ByteSliceToString(name[:]), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}
