package transport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/jerrykal/tower/internal/config"
)

// Check is one step of checking a host before it is added.
type Check struct {
	Name   string // ssh, tmux, os, tower
	OK     bool
	Detail string // what was found (a version, an OS), or why it failed with the fix
}

// MinTmux is the oldest tmux tower works with.
var MinTmux = [2]int{3, 2}

// CheckHost checks h in order: ssh with BatchMode (no prompt can hang
// it), tmux 3.2 or later, the OS (uname -s), and the tower binary. The
// steps after ssh share one ssh session. A failed step ends the list
// except tower's, which install on connect can mend.
func (s *SSH) CheckHost(ctx context.Context, h config.Host, tower string) []Check {
	script := strings.Join([]string{
		`echo "tmux $(tmux -V 2>/dev/null)"`,
		`echo "os $(uname -s 2>/dev/null)"`,
		`echo "tower $(` + RemoteCommand(tower, "version") + ` 2>/dev/null)"`,
	}, "; ")
	out, code, stderr, err := s.runWatched(ctx, h, script)
	if err != nil || code != 0 && !strings.HasPrefix(out, "tmux ") {
		reason := Classify(h.Target(), code, stderr).Reason
		if err != nil && ctx.Err() != nil {
			reason = "connection timed out"
		}
		return []Check{{Name: "ssh", Detail: reason}}
	}
	checks := []Check{{Name: "ssh", OK: true}}
	vals := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(l, " ")
		vals[k] = strings.TrimSpace(v)
	}
	tv := strings.TrimPrefix(vals["tmux"], "tmux ")
	switch {
	case tv == "":
		return append(checks, Check{Name: "tmux", Detail: "tmux is not installed on " + h.Name + ": install tmux 3.2 or later there"})
	case !tmuxAtLeast(tv, MinTmux):
		return append(checks, Check{Name: "tmux", Detail: "tmux " + tv + " on " + h.Name + " is too old: tower needs 3.2 or later"})
	}
	checks = append(checks, Check{Name: "tmux", OK: true, Detail: tv})
	if vals["os"] == "" {
		return append(checks, Check{Name: "os", Detail: "uname -s gave nothing"})
	}
	checks = append(checks, Check{Name: "os", OK: true, Detail: vals["os"]})
	if vals["tower"] == "" {
		return append(checks, Check{Name: "tower", Detail: "tower is not installed on " + h.Name + ": install it there, or set its path with --tower"})
	}
	return append(checks, Check{Name: "tower", OK: true, Detail: vals["tower"]})
}

// runWatched runs remote on h, reading stderr as it arrives: a Tailscale
// SSH check banner ends ssh at once (it would hold the connection until a
// browser login).
func (s *SSH) runWatched(ctx context.Context, h config.Host, remote string) (out string, code int, stderr string, err error) {
	cmd := s.Stream(h, remote)
	var outb bytes.Buffer
	cmd.Stdout = &outb
	errp, err := cmd.StderrPipe()
	if err != nil {
		return "", -1, "", err
	}
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return "", -1, "", err
	}
	var mu sync.Mutex
	var errb strings.Builder
	read := make(chan struct{})
	go func() {
		defer close(read)
		sc := bufio.NewScanner(errp)
		for sc.Scan() {
			mu.Lock()
			errb.WriteString(sc.Text() + "\n")
			ts := TailscaleCheck(errb.String())
			mu.Unlock()
			if ts {
				cmd.Process.Kill()
			}
		}
	}()
	stop := context.AfterFunc(ctx, func() { cmd.Process.Kill() })
	defer stop()
	<-read
	werr := cmd.Wait()
	code = 0
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if ctx.Err() != nil {
		return outb.String(), code, errb.String(), ctx.Err()
	}
	return outb.String(), code, errb.String(), nil
}

// tmuxAtLeast compares a tmux version ("3.4", "3.2a", "next-3.6") with
// want.
func tmuxAtLeast(v string, want [2]int) bool {
	v = strings.TrimPrefix(v, "next-")
	maj, rest, _ := strings.Cut(v, ".")
	a, err := strconv.Atoi(maj)
	if err != nil {
		return false
	}
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	b, _ := strconv.Atoi(rest[:i])
	return a > want[0] || a == want[0] && b >= want[1]
}
