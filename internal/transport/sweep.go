package transport

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Sweep removes the control sockets in dir that nobody listens on (a
// refused dial: a master that died). A socket that is only slow to accept
// is another host's busy master and stays.
func Sweep(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.Type()&os.ModeSocket == 0 {
			continue
		}
		p := filepath.Join(dir, e.Name())
		c, err := net.DialTimeout("unix", p, 200*time.Millisecond)
		if err == nil {
			c.Close()
			continue
		}
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
			os.Remove(p)
		}
	}
}

// WatchNet calls changed whenever the set of this machine's interface
// addresses (link-local ones left out) differs from the last look, until
// ctx ends.
func WatchNet(ctx context.Context, every time.Duration, changed func()) {
	last := addrs()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := addrs()
			if now != last {
				last = now
				changed()
			}
		}
	}
}

func addrs() string {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	var out []string
	for _, a := range as {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLinkLocalUnicast() || ipn.IP.IsLinkLocalMulticast() {
			continue
		}
		out = append(out, ipn.String())
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}
