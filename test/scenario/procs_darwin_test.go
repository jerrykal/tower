package scenario

import (
	"bytes"
	"encoding/binary"
	"os"

	"golang.org/x/sys/unix"
)

// listProcs reads every process of this user through kern.procargs2:
// argc, the executable's path, then argv and the environment, each
// NUL-terminated.
func listProcs() []proc {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil
	}
	var out []proc
	for _, kp := range kps {
		pid := int(kp.Proc.P_pid)
		b, err := unix.SysctlRaw("kern.procargs2", pid)
		if err != nil || len(b) < 4 {
			continue
		}
		if p, ok := parseProcArgs(pid, b); ok {
			out = append(out, p)
		}
	}
	return out
}

func parseProcArgs(pid int, b []byte) (proc, bool) {
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	rest := b[4:]
	// The executable's path, then NUL padding.
	i := 0
	for i < len(rest) && rest[i] != 0 {
		i++
	}
	for i < len(rest) && rest[i] == 0 {
		i++
	}
	p := proc{pid: pid}
	for _, tok := range bytes.Split(rest[i:], []byte{0}) {
		switch {
		case len(p.argv) < argc:
			p.argv = append(p.argv, string(tok))
		case len(tok) == 0:
			return p, true // the end of the environment
		default:
			p.env = append(p.env, string(tok))
		}
	}
	return p, len(p.argv) == argc
}

// procTree lists every process of this user with its parent and command
// name, from one kern.proc.uid read.
func procTree() []pnode {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil
	}
	out := make([]pnode, 0, len(kps))
	for _, kp := range kps {
		out = append(out, pnode{pid: int(kp.Proc.P_pid), ppid: int(kp.Eproc.Ppid), comm: unix.ByteSliceToString(kp.Proc.P_comm[:])})
	}
	return out
}
