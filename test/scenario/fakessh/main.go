// Command fakessh stands in for ssh in tower's scenario suite. Each
// simulated host is a tmux server on this machine with an environment of
// its own; fakessh runs the remote command in that environment, through a
// link shaped by the host's knobs (see Knobs): delay, jitter, bandwidth,
// windows, stalls, half-open connections, a shared control master,
// network changes and ssh's failure messages.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jerrykal/tower/test/scenario/sshcall"
)

// args is one ssh command line.
type args = sshcall.Args

func fakeDir() string { return os.Getenv("TOWER_FAKE_DIR") }

func main() {
	a, err := sshcall.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakessh:", err)
		os.Exit(255)
	}
	if a == nil { // -V
		fmt.Fprintln(os.Stderr, "OpenSSH_10.0p2, LibreSSL 3.3.6 (tower's fake ssh)")
		return
	}
	sshcall.Log(a)
	k, err := loadKnobs(a.Host)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh: Could not resolve hostname %s: nodename nor servname provided, or not known\n", a.Host)
		os.Exit(255)
	}
	if a.Op != "" {
		os.Exit(control(a, k))
	}
	os.Exit(session(a, k))
}

// control handles ssh -O: each takes o_delay_ms (a wedged master), and
// exit ends the master and every session on it.
func control(a *args, k *Knobs) int {
	time.Sleep(time.Duration(k.ODelayMs) * time.Millisecond)
	switch a.Op {
	case "exit", "stop":
		if k.Mux {
			os.Remove(masterPath(a.Host))
		}
		return 0
	case "check":
		if k.Mux {
			if _, err := os.Stat(masterPath(a.Host)); err == nil {
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
	h := a.Host
	batch := strings.EqualFold(a.Opt("BatchMode"), "yes")
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
		t := a.ConnectTimeout()
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
