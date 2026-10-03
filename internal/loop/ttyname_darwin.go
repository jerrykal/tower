package loop

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ttyName is the path of the terminal open on fd: the /dev entry with its
// device number.
func ttyName(fd int) (string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	ents, err := os.ReadDir("/dev")
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "tty") {
			continue
		}
		p := filepath.Join("/dev", e.Name())
		var s unix.Stat_t
		if unix.Stat(p, &s) == nil && s.Mode&unix.S_IFMT == unix.S_IFCHR && s.Rdev == st.Rdev {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}
