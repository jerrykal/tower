package scenario

import (
	"bytes"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// proc is a process of this user: its argv and environment.
type proc struct {
	pid  int
	argv []string
	env  []string
}

// pnode is a process in the process tree: procTree lists them per
// platform.
type pnode struct {
	pid, ppid, pgid int
	comm            string
}

// descendants are the processes under pid in tree, nearest first.
func descendants(tree []pnode, pid int) []pnode {
	kids := map[int][]pnode{}
	for _, p := range tree {
		kids[p.ppid] = append(kids[p.ppid], p)
	}
	var out []pnode
	next := kids[pid]
	for len(next) > 0 {
		p := next[0]
		next = append(next[1:], kids[p.pid]...)
		out = append(out, p)
	}
	return out
}

// getenv is the value of k in the process's environment.
func (p proc) getenv(k string) (string, bool) {
	for _, kv := range p.env {
		if v, ok := strings.CutPrefix(kv, k+"="); ok {
			return v, true
		}
	}
	return "", false
}

// procsWhere lists the user's processes (this one left out) that match.
// listProcs is per platform: /proc on Linux, kern.procargs2 on macOS.
func procsWhere(match func(proc) bool) []int {
	var out []int
	for _, p := range listProcs() {
		if p.pid != os.Getpid() && match(p) {
			out = append(out, p.pid)
		}
	}
	return out
}

// pidsWithEnvPrefix lists the processes whose variable k starts with
// prefix.
func pidsWithEnvPrefix(k, prefix string) []int {
	return procsWhere(func(p proc) bool {
		v, ok := p.getenv(k)
		return ok && strings.HasPrefix(v, prefix)
	})
}

// splitNUL splits NUL-terminated strings, dropping empty ones.
func splitNUL(b []byte) []string {
	var out []string
	for _, f := range bytes.Split(b, []byte{0}) {
		if len(f) > 0 {
			out = append(out, string(f))
		}
	}
	return out
}

// hasArgs reports whether argv holds want as consecutive arguments, or
// (a remote command line) as one argument's words.
func hasArgs(argv []string, want ...string) bool {
	for i := range argv {
		if i+len(want) <= len(argv) && slices.Equal(argv[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// TestProcLookup: processes are found by their environment and arguments,
// on Linux as on macOS: a towerd of the host's home and server, not its
// bridge, not another home's whose path starts the same.
func TestProcLookup(t *testing.T) {
	w := NewWorld(t, "procs")
	a := w.Host("A", nil)
	exe, _ := os.Executable()
	spawn := func(home string, argv ...string) int {
		t.Helper()
		cmd := exec.Command(exe)
		cmd.Args = argv
		cmd.Env = append(os.Environ(), helperVar+"=sleep", "TOWER_HOME="+home)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd.Process.Pid
	}
	towerd := spawn(a.HomeDir, "tower", "towerd", "--tmux", "-L "+a.Sock)
	spawn(a.HomeDir, "tower", "towerd", "--stdio", "--tmux", "-L "+a.Sock)
	other := spawn(a.HomeDir+"2", "tower", "towerd", "--tmux", "-L "+a.Sock)
	if got := a.TowerdProcs(); !slices.Equal(got, []int{towerd}) {
		t.Fatalf("A's towerds: %v, want [%d]", got, towerd)
	}
	// Teardown's sweep: every process with a home under the world's dir.
	if got := pidsWithEnvPrefix("TOWER_HOME", w.Dir+"/"); len(got) != 3 || !slices.Contains(got, other) {
		t.Fatalf("processes under %s: %v", w.Dir, got)
	}
	if got := pidsWithEnvPrefix("TOWER_HOME", a.HomeDir+"/"); len(got) != 0 {
		t.Fatalf("processes under %s/: %v", a.HomeDir, got)
	}
}
