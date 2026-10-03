package loop

import (
	"os"
	"strconv"
)

// ttyName is the path of the terminal open on fd.
func ttyName(fd int) (string, error) {
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
}
