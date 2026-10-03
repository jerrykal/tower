package transport

import (
	"strconv"
	"strings"
	"time"
)

// waitDelay bounds how long a finished ssh's pipes may stay open (a
// half-open connection can keep a child holding them).
const waitDelay = 2 * time.Second

// Failure classes.
const (
	Down   = "down"   // may come back by itself: retried with backoff
	Failed = "failed" // needs the user: retried at the cap
)

// Failure says why a host cannot be reached, and the fix.
type Failure struct {
	Class  string
	Reason string
}

// Classify turns ssh's exit code and stderr into a Failure.
func Classify(host string, exit int, stderr string) Failure {
	s := stderr
	switch {
	case TailscaleCheck(s):
		return Failure{Down, "Tailscale SSH wants a check: run `ssh " + host + "` once"}
	case exit == 127 || strings.Contains(s, "command not found") && strings.Contains(s, "tower") ||
		strings.Contains(s, "tower: No such file or directory"):
		return Failure{Failed, "tower is not installed on " + host}
	case strings.Contains(s, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return Failure{Down, "host key changed: check it, then `ssh " + host + "` once"}
	case strings.Contains(s, "Host key verification failed") || strings.Contains(s, "host key is known"):
		return Failure{Down, "host key not trusted: run `ssh " + host + "` once to check it"}
	case strings.Contains(s, "Permission denied") || strings.Contains(s, "password:") ||
		strings.Contains(s, "passphrase"):
		return Failure{Down, "authentication failed: load the key with `ssh-add`, or set one up with `ssh-copy-id`"}
	case strings.Contains(s, "Could not resolve hostname"):
		return Failure{Down, "cannot resolve host name"}
	case strings.Contains(s, "Connection refused"):
		return Failure{Down, "connection refused"}
	case strings.Contains(s, "timed out") || strings.Contains(s, "not responding"):
		return Failure{Down, "connection timed out"}
	case strings.Contains(s, "closed by remote host") || strings.Contains(s, "Connection closed") ||
		strings.Contains(s, "Shared connection") || strings.Contains(s, "Broken pipe"):
		return Failure{Down, "connection lost"}
	}
	if line := lastLine(s); line != "" {
		return Failure{Down, line}
	}
	return Failure{Down, "ssh exited " + strconv.Itoa(exit)}
}

// TailscaleCheck reports whether s is Tailscale SSH asking for a check in
// a browser, after which it holds the connection.
func TailscaleCheck(s string) bool {
	return strings.Contains(s, "Tailscale SSH") && strings.Contains(s, "check")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
