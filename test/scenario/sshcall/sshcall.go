// Package sshcall reads an ssh command line as ssh does, and logs each
// call as a JSON line in $TOWER_TEST_SSH_LOG, for the scenarios that
// count or inspect the calls tower makes. The suite's wrapper around
// real ssh (sshwrap) uses it.
package sshcall

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Args is one ssh command line.
type Args struct {
	Opts   map[string]string // -o Key=Value, keys lower case, first value wins
	TTY    bool
	Op     string // -O
	Host   string
	Remote string
}

// Parse reads argv (without the program); nil for -V.
func Parse(argv []string) (*Args, error) {
	a := &Args{Opts: map[string]string{}}
	for i := 0; i < len(argv); i++ {
		s := argv[i]
		switch {
		case s == "-V":
			return nil, nil
		case s == "--":
			if a.Host == "" && i+1 < len(argv) {
				a.Host = argv[i+1]
				i++
			}
			a.Remote = strings.Join(argv[i+1:], " ")
			return a, nil
		case s == "-o" && i+1 < len(argv):
			k, v, _ := strings.Cut(argv[i+1], "=")
			k = strings.ToLower(k)
			if _, ok := a.Opts[k]; !ok {
				a.Opts[k] = v
			}
			i++
		case s == "-O" && i+1 < len(argv):
			a.Op = argv[i+1]
			i++
		case s == "-t" || s == "-tt":
			a.TTY = true
		case s == "-T" || s == "-n" || s == "-q":
		case strings.HasPrefix(s, "-"):
			// Options with a value we ignore.
			if strings.ContainsAny(s[1:2], "pliFJS") && len(s) == 2 {
				i++
			}
		case a.Host == "":
			a.Host = s
		default:
			a.Remote = strings.Join(argv[i:], " ")
			return a, nil
		}
	}
	if a.Host == "" {
		return nil, fmt.Errorf("no host")
	}
	return a, nil
}

// Opt is the value of -o k.
func (a *Args) Opt(k string) string { return a.Opts[strings.ToLower(k)] }

// Alive is how long a dead connection takes to give up:
// ServerAliveInterval × ServerAliveCountMax (0: never).
func (a *Args) Alive() time.Duration {
	iv, _ := strconv.Atoi(a.Opt("ServerAliveInterval"))
	n, err := strconv.Atoi(a.Opt("ServerAliveCountMax"))
	if err != nil {
		n = 3
	}
	return time.Duration(iv*n) * time.Second
}

// ConnectTimeout is -o ConnectTimeout (0: none).
func (a *Args) ConnectTimeout() time.Duration {
	s, _ := strconv.Atoi(a.Opt("ConnectTimeout"))
	return time.Duration(s) * time.Second
}

// Log appends the call to $TOWER_TEST_SSH_LOG.
func Log(a *Args) {
	rec := map[string]any{
		"ts": time.Now().UnixMilli(), "host": a.Host, "tty": a.TTY, "op": a.Op,
		"batch": a.Opt("BatchMode"), "cp": a.Opt("ControlPath"), "okt": a.Opt("ObscureKeystrokeTiming"),
		"cm": a.Opt("ControlMaster"), "cmd": a.Remote,
	}
	b, _ := json.Marshal(rec)
	f, err := os.OpenFile(os.Getenv("TOWER_TEST_SSH_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}
