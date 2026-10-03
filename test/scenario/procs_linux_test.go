package scenario

import (
	"os"
	"path/filepath"
	"strconv"
)

// listProcs reads every readable process from /proc: cmdline and environ
// are NUL-separated.
func listProcs() []proc {
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	var out []proc
	for _, d := range dirs {
		pid, err := strconv.Atoi(filepath.Base(d))
		if err != nil {
			continue
		}
		cmd, err1 := os.ReadFile(filepath.Join(d, "cmdline"))
		env, err2 := os.ReadFile(filepath.Join(d, "environ"))
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, proc{pid: pid, argv: splitNUL(cmd), env: splitNUL(env)})
	}
	return out
}
