package scenario

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// procTree lists every process with its parent, process group and
// command name, from /proc/<pid>/stat: pid (comm) state ppid pgrp …, comm
// perhaps with spaces.
func procTree() []pnode {
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	var out []pnode
	for _, d := range dirs {
		st, err := os.ReadFile(filepath.Join(d, "stat"))
		if err != nil {
			continue
		}
		open, end := bytes.IndexByte(st, '('), bytes.LastIndexByte(st, ')')
		if open < 0 || end < open {
			continue
		}
		f := strings.Fields(string(st[end+1:]))
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(st[:open])))
		ppid, _ := strconv.Atoi(f[1])
		pgid, _ := strconv.Atoi(f[2])
		out = append(out, pnode{pid: pid, ppid: ppid, pgid: pgid, comm: string(st[open+1 : end])})
	}
	return out
}
