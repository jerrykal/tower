package main

import (
	"os"

	"github.com/jerrykal/tower/internal/relay"
)

func isTerminal(f *os.File) bool {
	_, err := relay.GetModes(int(f.Fd()))
	return err == nil
}

// makeRaw puts fd in raw mode as ssh does and returns the old modes.
func makeRaw(fd int) (*relay.Modes, error) {
	old, err := relay.GetModes(fd)
	if err != nil {
		return nil, err
	}
	if err := relay.SetModes(fd, old.Raw()); err != nil {
		return nil, err
	}
	return &old, nil
}

func restore(fd int, m *relay.Modes) {
	if m != nil {
		relay.SetModes(fd, *m)
	}
}

// copyModes gives dst the modes and window size of src.
func copyModes(src, dst int) {
	if m, err := relay.GetModes(src); err == nil {
		relay.SetModes(dst, m)
	}
	if rows, cols, err := relay.GetSize(src); err == nil {
		relay.SetSize(dst, rows, cols)
	}
}

// openPty opens the host's pty for a session.
func openPty() (master, slave *os.File, err error) {
	p, err := relay.OpenPty()
	if err != nil {
		return nil, nil, err
	}
	return p.Master, p.Slave, nil
}
