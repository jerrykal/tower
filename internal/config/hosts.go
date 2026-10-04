package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Host is one entry of hosts.toml.
type Host struct {
	Name              string `toml:"name"`
	SSH               string `toml:"ssh"`
	Tmux              string `toml:"tmux,omitempty"`
	Tower             string `toml:"tower,omitempty"`
	Enabled           *bool  `toml:"enabled,omitempty"`
	Standby           *bool  `toml:"standby,omitempty"`
	ObscureKeystrokes bool   `toml:"obscure_keystrokes,omitempty"`
	// Home is TOWER_HOME for tower on that host: the real-host tests and
	// demos keep its state apart from the user's own tower there.
	Home string `toml:"home,omitempty"`
}

// On reports whether the host is enabled (the default).
func (h Host) On() bool { return h.Enabled == nil || *h.Enabled }

// StandbyOn reports whether standby sessions are allowed (the default).
func (h Host) StandbyOn() bool { return h.Standby == nil || *h.Standby }

// Same reports whether h and o say the same: the optional settings are
// compared by value (two loads of one file give different pointers).
func (h Host) Same(o Host) bool {
	a, b := h, o
	a.Enabled, b.Enabled = nil, nil
	a.Standby, b.Standby = nil, nil
	return a == b && h.On() == o.On() && h.StandbyOn() == o.StandbyOn()
}

// Target is what ssh is given: SSH, or the name when SSH is empty.
func (h Host) Target() string {
	if h.SSH != "" {
		return h.SSH
	}
	return h.Name
}

type hostsFile struct {
	Host []Host `toml:"host"`
}

// LoadHosts reads the host list; a missing file is an empty list.
func LoadHosts(path string) ([]Host, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f hostsFile
	if _, err := toml.Decode(string(b), &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for i, h := range f.Host {
		if h.Name == "" {
			return nil, fmt.Errorf("%s: host %d has no name", path, i+1)
		}
		k := strings.ToLower(h.Name)
		if seen[k] {
			return nil, fmt.Errorf("%s: two hosts are named %q", path, h.Name)
		}
		seen[k] = true
	}
	return f.Host, nil
}

// SaveHosts writes the host list atomically.
func SaveHosts(path string, hosts []Host) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(hostsFile{Host: hosts}); err != nil {
		return err
	}
	return WriteFile(path, buf.Bytes())
}

// DefaultName derives a host label from an ssh target: the host part
// without user, port or domain (me@box.lan:2222 → box); an IP address
// stays whole.
func DefaultName(target string) string {
	t := target
	if u, err := url.Parse(t); err == nil && u.Scheme == "ssh" {
		t = u.Host
	}
	if i := strings.LastIndexByte(t, '@'); i >= 0 {
		t = t[i+1:]
	}
	if h, _, err := net.SplitHostPort(t); err == nil {
		t = h
	} else if strings.Count(t, ":") == 1 {
		t = t[:strings.IndexByte(t, ':')]
	}
	t = strings.Trim(t, "[]")
	if net.ParseIP(t) != nil {
		return t
	}
	if i := strings.IndexByte(t, '.'); i > 0 {
		t = t[:i]
	}
	return t
}
