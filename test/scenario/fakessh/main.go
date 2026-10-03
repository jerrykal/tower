// Command fakessh stands in for ssh in tower's scenario suite. Each
// simulated host is a tmux server on this machine with an environment of
// its own; fakessh runs the remote command in that environment, through a
// link shaped by the host's knobs (see Knobs): delay, jitter, bandwidth,
// windows, stalls, half-open connections, a shared control master,
// network changes and ssh's failure messages.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type args struct {
	opts   map[string]string // -o Key=Value, first value wins
	tty    bool
	op     string // -O
	host   string
	remote string
}

func main() {
	a, err := parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakessh:", err)
		os.Exit(255)
	}
	if a == nil { // -V
		fmt.Fprintln(os.Stderr, "OpenSSH_10.0p2, LibreSSL 3.3.6 (tower's fake ssh)")
		return
	}
	logCall(a)
	k, err := loadKnobs(a.host)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh: Could not resolve hostname %s: nodename nor servname provided, or not known\n", a.host)
		os.Exit(255)
	}
	if a.op != "" {
		os.Exit(control(a, k))
	}
	os.Exit(session(a, k))
}

func parse(argv []string) (*args, error) {
	a := &args{opts: map[string]string{}}
	for i := 0; i < len(argv); i++ {
		s := argv[i]
		switch {
		case s == "-V":
			return nil, nil
		case s == "--":
			if a.host == "" && i+1 < len(argv) {
				a.host = argv[i+1]
				i++
			}
			a.remote = strings.Join(argv[i+1:], " ")
			return a, nil
		case s == "-o" && i+1 < len(argv):
			k, v, _ := strings.Cut(argv[i+1], "=")
			k = strings.ToLower(k)
			if _, ok := a.opts[k]; !ok {
				a.opts[k] = v
			}
			i++
		case s == "-O" && i+1 < len(argv):
			a.op = argv[i+1]
			i++
		case s == "-t" || s == "-tt":
			a.tty = true
		case s == "-T" || s == "-n" || s == "-q":
		case strings.HasPrefix(s, "-"):
			// Options with a value we ignore.
			if strings.ContainsAny(s[1:2], "pliFJS") && len(s) == 2 {
				i++
			}
		case a.host == "":
			a.host = s
		default:
			a.remote = strings.Join(argv[i:], " ")
			return a, nil
		}
	}
	if a.host == "" {
		return nil, fmt.Errorf("no host")
	}
	return a, nil
}

func (a *args) opt(k string) string { return a.opts[strings.ToLower(k)] }

// alive is how long a dead connection takes to give up:
// ServerAliveInterval × ServerAliveCountMax (0: never).
func (a *args) alive() time.Duration {
	iv, _ := strconv.Atoi(a.opt("ServerAliveInterval"))
	n, err := strconv.Atoi(a.opt("ServerAliveCountMax"))
	if err != nil {
		n = 3
	}
	return time.Duration(iv*n) * time.Second
}

func (a *args) connectTimeout() time.Duration {
	s, _ := strconv.Atoi(a.opt("ConnectTimeout"))
	return time.Duration(s) * time.Second
}

func fakeDir() string { return os.Getenv("TOWER_FAKE_DIR") }

func logCall(a *args) {
	rec := map[string]any{
		"ts": time.Now().UnixMilli(), "host": a.host, "tty": a.tty, "op": a.op,
		"batch": a.opt("BatchMode"), "cp": a.opt("ControlPath"), "okt": a.opt("ObscureKeystrokeTiming"),
		"cm": a.opt("ControlMaster"), "cmd": a.remote,
	}
	b, _ := json.Marshal(rec)
	f, err := os.OpenFile(filepath.Join(fakeDir(), "ssh.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

// control handles ssh -O: each takes o_delay_ms (a wedged master), and
// exit ends the master and every session on it.
func control(a *args, k *Knobs) int {
	time.Sleep(time.Duration(k.ODelayMs) * time.Millisecond)
	switch a.op {
	case "exit", "stop":
		if k.Mux {
			os.Remove(masterPath(a.host))
		}
		return 0
	case "check":
		if k.Mux {
			if _, err := os.Stat(masterPath(a.host)); err == nil {
				fmt.Fprintf(os.Stderr, "Master running (pid=%d)\n", os.Getpid())
				return 0
			}
		}
		fmt.Fprintln(os.Stderr, "Control socket connect: No such file or directory")
		return 255
	}
	return 0
}

// refuse prints what ssh prints for a host that cannot be reached, and
// returns the exit status, or hangs as ssh would.
func refuse(a *args, k *Knobs) int {
	h := a.host
	batch := strings.EqualFold(a.opt("BatchMode"), "yes")
	switch k.Down {
	case "refused":
		fmt.Fprintf(os.Stderr, "ssh: connect to host %s port 22: Connection refused\n", h)
	case "hostkey":
		fmt.Fprintf(os.Stderr, "No ED25519 host key is known for %s and you have requested strict checking.\nHost key verification failed.\n", h)
	case "auth", "password":
		if !batch {
			fmt.Fprintf(os.Stderr, "%s's password: ", h)
			select {}
		}
		fmt.Fprintf(os.Stderr, "%s: Permission denied (publickey,password).\n", h)
	case "resolve":
		fmt.Fprintf(os.Stderr, "ssh: Could not resolve hostname %s: nodename nor servname provided, or not known\n", h)
	case "timeout":
		t := a.connectTimeout()
		if t == 0 {
			select {}
		}
		time.Sleep(t)
		fmt.Fprintf(os.Stderr, "ssh: connect to host %s port 22: Operation timed out\n", h)
	case "tscheck":
		fmt.Fprintf(os.Stderr, "# Tailscale SSH requires an additional check.\n# To authenticate, visit: https://login.tailscale.com/a/0123456789abcd\n")
		time.Sleep(30 * time.Second)
		fmt.Fprintf(os.Stderr, "Connection closed by %s port 22\n", h)
	default:
		fmt.Fprintf(os.Stderr, "ssh: %s: %s\n", h, k.Down)
	}
	return 255
}

// recordHolder notes a process that keeps a half-open connection's remote
// side alive, for the harness to end at teardown.
func recordHolder(pid int) {
	f, err := os.OpenFile(filepath.Join(fakeDir(), "holders"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintln(f, pid)
	f.Close()
}
