// Command hostagent runs as root in each host container of the scenario
// suite and applies what the harness asks over a unix socket in the
// run's directory, one JSON request and reply per connection, without a
// docker exec per change: a world's assignment (its environment file,
// everything reset), commands as the test user or root, the link's
// shape, and the faults. It sees only the container's processes (its
// own pid namespace), so whatever it signals is the container's.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Request is one call.
type Request struct {
	Op     string `json:"op"`               // assign, reset, exec, netem, freeze, halfopen, drop, stall
	Env    string `json:"env,omitempty"`    // assign: the world's environment file for tt-run
	Root   bool   `json:"root,omitempty"`   // exec: as root, not tt
	Cmd    string `json:"cmd,omitempty"`    // exec: a shell command
	On     bool   `json:"on,omitempty"`     // freeze, stall
	Delay  int    `json:"delay,omitempty"`  // netem: one-way ms
	Jitter int    `json:"jitter,omitempty"` // netem: ± ms
	BwKBps int    `json:"bw,omitempty"`     // netem: KB/s
}

// Reply is a call's result.
type Reply struct {
	Out  string `json:"out,omitempty"`
	Code int    `json:"code"`
	Err  string `json:"err,omitempty"`
}

var (
	tt      *user.User
	ttUID   int
	mu      sync.Mutex // one change at a time
	stallMu sync.Mutex
	stallCh chan struct{}
	stallWg sync.WaitGroup
)

func main() {
	sock := flag.String("sock", "", "the socket to serve")
	flag.Parse()
	var err error
	if tt, err = user.Lookup("tt"); err != nil {
		log.Fatal(err)
	}
	ttUID, _ = strconv.Atoi(tt.Uid)
	gid, _ := strconv.Atoi(tt.Gid)
	os.Remove(*sock)
	l, err := net.Listen("unix", *sock)
	if err != nil {
		log.Fatal(err)
	}
	// The harness runs as the test user, whose uid tt has.
	if err := os.Chown(*sock, ttUID, gid); err != nil {
		log.Fatal(err)
	}
	os.Chmod(*sock, 0o600)
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go serve(c)
	}
}

func serve(c net.Conn) {
	defer c.Close()
	var req Request
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, &req)
	}
	var rep Reply
	if err != nil {
		rep.Err = err.Error()
	} else {
		rep = handle(req)
	}
	b, _ := json.Marshal(rep)
	c.Write(append(b, '\n'))
}

func handle(r Request) Reply {
	if r.Op != "exec" {
		mu.Lock()
		defer mu.Unlock()
	}
	switch r.Op {
	case "assign":
		if err := os.WriteFile("/etc/tt-env-path", []byte(r.Env+"\n"), 0o644); err != nil {
			return Reply{Err: err.Error()}
		}
		return reset()
	case "reset":
		return reset()
	case "exec":
		return run(r.Cmd, r.Root)
	case "netem":
		return netem(r.Delay, r.Jitter, r.BwKBps)
	case "freeze":
		if r.On {
			return sh("iptables -A TT-FREEZE -p tcp -j DROP")
		}
		return sh("iptables -F TT-FREEZE")
	case "halfopen":
		// Every connection established now, both ways and for good: what
		// a network change does to them.
		return sh(`ss -Htn state established | while read -r _ _ local peer; do ` +
			`lport=${local##*:}; ip=${peer%:*}; port=${peer##*:}; ` +
			`iptables -A TT-HALFOPEN -p tcp -s "$ip" --sport "$port" --dport "$lport" -j DROP; ` +
			`iptables -A TT-HALFOPEN -p tcp -d "$ip" --dport "$port" --sport "$lport" -j DROP; done`)
	case "drop":
		// The per-connection sshd processes: their connections close.
		for _, p := range procs() {
			if strings.HasPrefix(p.argv0, "sshd: tt") {
				syscall.Kill(p.pid, syscall.SIGKILL)
			}
		}
		return Reply{}
	case "stall":
		if r.On {
			stallOn()
		} else {
			stallOff()
		}
		return Reply{}
	}
	return Reply{Err: "unknown op " + r.Op}
}

// reset puts the container back as it started: no stall, no shaping, no
// faults, none of the test user's processes (tmux servers, towerds,
// sessions).
func reset() Reply {
	stallOff()
	if rep := netem(0, 0, 0); rep.Err != "" {
		return rep
	}
	if rep := sh("iptables -F TT-FREEZE && iptables -F TT-HALFOPEN"); rep.Err != "" {
		return rep
	}
	for range 50 {
		left := 0
		for _, p := range procs() {
			if p.uid == ttUID {
				syscall.Kill(p.pid, syscall.SIGKILL)
				left++
			}
		}
		if left == 0 {
			return Reply{}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return Reply{Err: "the test user's processes outlived SIGKILL for 1s"}
}

// netem shapes eth0's egress and, through ifb0, its ingress: each
// direction gets the one-way delay.
func netem(delay, jitter, bw int) Reply {
	if delay == 0 && jitter == 0 && bw == 0 {
		return sh("tc qdisc del dev eth0 root 2>/dev/null; tc qdisc del dev ifb0 root 2>/dev/null; true")
	}
	q := fmt.Sprintf("netem delay %dms %dms limit 100000", delay, jitter)
	if bw > 0 {
		q += fmt.Sprintf(" rate %dkbit", bw*8)
	}
	return sh("tc qdisc replace dev eth0 root " + q + " && tc qdisc replace dev ifb0 root " + q)
}

// run runs a shell command: as tt through tt-run (the world's
// environment), or as root.
func run(cmdline string, root bool) Reply {
	var cmd *exec.Cmd
	if root {
		cmd = exec.Command("/bin/sh", "-c", cmdline)
	} else {
		cmd = exec.Command("/usr/local/bin/tt-run")
		gid, _ := strconv.Atoi(tt.Gid)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(ttUID), Gid: uint32(gid)}}
		cmd.Env = []string{"HOME=" + tt.HomeDir, "USER=tt", "LOGNAME=tt", "PATH=/usr/local/bin:/usr/bin:/bin",
			"SSH_ORIGINAL_COMMAND=" + cmdline}
		cmd.Dir = tt.HomeDir
	}
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	rep := Reply{Out: string(out)}
	if cmd.ProcessState != nil {
		rep.Code = cmd.ProcessState.ExitCode()
	}
	if err != nil && rep.Code == 0 {
		rep.Err = err.Error()
	}
	return rep
}

// sh runs a root script whose failure is an error.
func sh(script string) Reply {
	rep := run(script, true)
	if rep.Code != 0 && rep.Err == "" {
		rep.Err = fmt.Sprintf("%s: exit %d: %s", script, rep.Code, rep.Out)
	}
	return rep
}

type proc struct {
	pid, ppid, uid int
	argv0          string
}

// procs lists the container's processes.
func procs() []proc {
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	var out []proc
	for _, d := range dirs {
		pid, _ := strconv.Atoi(filepath.Base(d))
		st, err1 := os.ReadFile(filepath.Join(d, "stat"))
		cmd, err2 := os.ReadFile(filepath.Join(d, "cmdline"))
		var fi syscall.Stat_t
		if err1 != nil || err2 != nil || syscall.Stat(d, &fi) != nil {
			continue
		}
		// pid (comm) state ppid …: comm may hold spaces.
		i := strings.LastIndexByte(string(st), ')')
		f := strings.Fields(string(st[i+1:]))
		if len(f) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		argv0, _, _ := strings.Cut(string(cmd), "\x00")
		out = append(out, proc{pid: pid, ppid: ppid, uid: int(fi.Uid), argv0: argv0})
	}
	return out
}

// sessionProcs are what ssh sessions run: the test user's descendants of
// the "sshd: tt@…" processes, not those (sshd answers keepalives) and not
// what has left them (a towerd, a tmux server).
func sessionProcs() []int {
	ps := procs()
	parent := map[int]int{}
	sess := map[int]bool{}
	for _, p := range ps {
		parent[p.pid] = p.ppid
		if strings.HasPrefix(p.argv0, "sshd: tt@") {
			sess[p.pid] = true
		}
	}
	var out []int
	for _, p := range ps {
		if sess[p.pid] || p.uid != ttUID {
			continue
		}
		for q, n := parent[p.pid], 0; q > 1 && n < 64; q, n = parent[q], n+1 {
			if sess[q] {
				out = append(out, p.pid)
				break
			}
		}
	}
	return out
}

// stallOn stops the session processes, and new ones as they come, until
// stallOff continues them.
func stallOn() {
	stallMu.Lock()
	defer stallMu.Unlock()
	if stallCh != nil {
		return
	}
	stop := make(chan struct{})
	stallCh = stop
	stopped := map[int]bool{}
	sweep := func() {
		for _, pid := range sessionProcs() {
			if !stopped[pid] && syscall.Kill(pid, syscall.SIGSTOP) == nil {
				stopped[pid] = true
			}
		}
	}
	sweep()
	stallWg.Add(1)
	go func() {
		defer stallWg.Done()
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				for pid := range stopped {
					syscall.Kill(pid, syscall.SIGCONT)
				}
				return
			case <-t.C:
				sweep()
			}
		}
	}()
}

func stallOff() {
	stallMu.Lock()
	defer stallMu.Unlock()
	if stallCh == nil {
		return
	}
	close(stallCh)
	stallWg.Wait()
	stallCh = nil
}
