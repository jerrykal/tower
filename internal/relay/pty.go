package relay

import (
	"os"

	"golang.org/x/sys/unix"
)

// Pty is a pseudo-terminal pair. Both ends are in blocking mode.
type Pty struct {
	Master *os.File
	Slave  *os.File
	Name   string // the slave's path
}

// OpenPty opens a new pty: /dev/ptmx, grant, unlock, then the slave by
// name. Neither end becomes anyone's controlling terminal, and both are
// closed on exec.
func OpenPty() (*Pty, error) {
	m, s, name, err := openPty()
	if err != nil {
		return nil, err
	}
	return &Pty{Master: os.NewFile(uintptr(m), "/dev/ptmx"), Slave: os.NewFile(uintptr(s), name), Name: name}, nil
}

// openPty is OpenPty on bare descriptors.
func openPty() (master, slave int, name string, err error) {
	m, err := openCloexec("/dev/ptmx")
	if err != nil {
		return -1, -1, "", err
	}
	if err := grantUnlock(m); err != nil {
		unix.Close(m)
		return -1, -1, "", os.NewSyscallError("unlockpt", err)
	}
	name, err = ptsName(m)
	if err != nil {
		unix.Close(m)
		return -1, -1, "", os.NewSyscallError("ptsname", err)
	}
	s, err := openCloexec(name)
	if err != nil {
		unix.Close(m)
		return -1, -1, "", err
	}
	return m, s, name, nil
}

// Close closes both ends, the slave first: a read blocked on the master
// then ends (on macOS closing a master blocks while a read on it is
// blocked).
func (p *Pty) Close() error {
	e1 := p.Slave.Close()
	e2 := p.Master.Close()
	if e1 != nil {
		return e1
	}
	return e2
}

func openCloexec(path string) (int, error) {
	for {
		fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return -1, &os.PathError{Op: "open", Path: path, Err: err}
		}
		return fd, nil
	}
}
