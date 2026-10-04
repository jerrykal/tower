// Package hosts edits one machine's host list (hosts.toml) the way both
// `tower host` and the dashboard do: the naming rules, the candidates the
// ssh config offers, and the checks a host goes through before it joins
// the list. Every edit tells the home to read the list again.
package hosts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/install"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/transport"
	"github.com/jerrykal/tower/internal/version"
)

// List is the host list of the machine Env describes.
type List struct {
	Env  *config.Env
	SSH  *transport.SSH
	Self string // this tower binary: what a missing tower check installs

	// SSHConfig is the ssh config whose aliases are offered; empty means
	// ~/.ssh/config.
	SSHConfig string
}

// New is the host list of env, checked with tower's ssh.
func New(env *config.Env) *List {
	self, _ := os.Executable()
	return &List{Env: env, SSH: transport.New(env.CMDir()), Self: self}
}

// Load reads the list.
func (l *List) Load() ([]config.Host, error) { return config.LoadHosts(l.Env.HostsFile()) }

// Aliases are the ssh config's host aliases (no patterns), in file order.
func (l *List) Aliases() []string {
	path := l.SSHConfig
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		path = home + "/.ssh/config"
	}
	return SSHAliases(path)
}

// Find returns the index of the host named name (any case), or -1.
func Find(hosts []config.Host, name string) int {
	return slices.IndexFunc(hosts, func(h config.Host) bool { return strings.EqualFold(h.Name, name) })
}

// Clean is a name as a label keeps it: trimmed, with ':' and white space
// turned into '-', since labels show as <host>:<session>.
func Clean(name string) string {
	name = strings.TrimSpace(name)
	return strings.Map(func(r rune) rune {
		if r == ':' || r == ' ' || r == '\t' {
			return '-'
		}
		return r
	}, name)
}

// CheckName applies the naming rules to name (already Clean) for a host
// whose ssh target is target: names are unique regardless of case, "local"
// is this machine's, and a name equal to an ssh alias is only for the host
// that alias reaches.
// skip is the index of the host being renamed, or -1.
func CheckName(hosts []config.Host, aliases []string, name, target string, skip int) error {
	if name == "" {
		return errors.New("a name is needed")
	}
	if strings.EqualFold(name, proto.LocalName) {
		return fmt.Errorf("%q is this machine's name", proto.LocalName)
	}
	if i := Find(hosts, name); i >= 0 && i != skip {
		return fmt.Errorf("a host named %q already exists", name)
	}
	for _, a := range aliases {
		if strings.EqualFold(a, name) && !strings.EqualFold(a, target) {
			return fmt.Errorf("%q is an ssh alias for another host", a)
		}
	}
	return nil
}

// FreeName is the name a host reached by target gets by default: its
// host part (config.DefaultName), cleaned, suffixed -2, -3, … until the
// rules allow it.
func FreeName(hosts []config.Host, aliases []string, target string) string {
	base := Clean(config.DefaultName(target))
	if base == "" {
		base = "host"
	}
	name := base
	for n := 2; CheckName(hosts, aliases, name, target, -1) != nil; n++ {
		name = base + "-" + strconv.Itoa(n)
	}
	return name
}

// Step is one check's progress: Running while it goes, then its result.
type Step struct {
	transport.Check
	Running bool
}

// Check runs the checks of h in order: ssh with BatchMode, tmux 3.2 or
// later, the OS, and the tower binary, which it installs (this build)
// when it is missing and h pins none. step, if not nil, hears each check
// start and end.
func (l *List) Check(ctx context.Context, h config.Host, step func(Step)) []transport.Check {
	if step == nil {
		step = func(Step) {}
	}
	inst := install.New(version.Version, l.Self)
	towerCmd := inst.Command()
	if h.Tower != "" {
		towerCmd = transport.RemoteCommand(h.Tower)
	}
	towerCmd = transport.WithHome(h.Home, towerCmd)
	step(Step{Check: transport.Check{Name: "ssh", Detail: "connecting to " + h.Target()}, Running: true})
	checks := l.SSH.CheckHost(ctx, h, towerCmd)
	last := &checks[len(checks)-1]
	for _, c := range checks[:len(checks)-1] {
		step(Step{Check: c})
	}
	if last.Name == "tower" && !last.OK && h.Tower == "" {
		// The last check: put this build there, as the home would on
		// connect.
		step(Step{Check: transport.Check{Name: "tower", Detail: "installing " + version.Version}, Running: true})
		ictx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err := inst.Install(ictx, l.SSH.On(h), h.Name)
		cancel()
		if err != nil {
			last.Detail = err.Error()
		} else {
			last.OK, last.Detail = true, "installed "+version.Version
		}
	}
	step(Step{Check: *last})
	return checks
}

// Failed reports whether any check failed.
func Failed(checks []transport.Check) bool {
	return slices.ContainsFunc(checks, func(c transport.Check) bool { return !c.OK })
}

// Add checks h and adds it to the list, whatever the checks found: a host
// that failed one stays, with the reason shown, until it is fixed. The
// name must pass CheckName first.
func (l *List) Add(ctx context.Context, h config.Host, step func(Step)) ([]transport.Check, error) {
	if h.SSH == h.Name {
		h.SSH = ""
	}
	checks := l.Check(ctx, h, step)
	err := l.edit(func(hosts []config.Host) ([]config.Host, error) {
		if i := Find(hosts, h.Name); i >= 0 {
			return nil, fmt.Errorf("a host named %q already exists", hosts[i].Name)
		}
		return append(hosts, h), nil
	})
	return checks, err
}

// Remove takes the host named name off the list; its sessions keep
// running.
func (l *List) Remove(name string) error {
	return l.editOne(name, func(hosts []config.Host, i int) ([]config.Host, error) {
		return slices.Delete(hosts, i, i+1), nil
	})
}

// SetOn turns the host named name on or off.
func (l *List) SetOn(name string, on bool) error {
	return l.editOne(name, func(hosts []config.Host, i int) ([]config.Host, error) {
		if on {
			hosts[i].Enabled = nil
		} else {
			hosts[i].Enabled = &on
		}
		return hosts, nil
	})
}

// Rename gives the host named old the label name, under the naming rules.
// A host whose ssh target was its old name keeps that target.
func (l *List) Rename(old, name string) error {
	name = Clean(name)
	aliases := l.Aliases()
	return l.editOne(old, func(hosts []config.Host, i int) ([]config.Host, error) {
		h := &hosts[i]
		if err := CheckName(hosts, aliases, name, h.Target(), i); err != nil {
			return nil, err
		}
		if h.SSH == "" {
			h.SSH = h.Name
		}
		h.Name = name
		if h.SSH == h.Name {
			h.SSH = ""
		}
		return hosts, nil
	})
}

// editOne edits the host named name.
func (l *List) editOne(name string, fn func([]config.Host, int) ([]config.Host, error)) error {
	return l.edit(func(hosts []config.Host) ([]config.Host, error) {
		i := Find(hosts, name)
		if i < 0 {
			return nil, fmt.Errorf("no host named %q", name)
		}
		return fn(hosts, i)
	})
}

// edit loads the list, changes it and saves it, then tells the home.
func (l *List) edit(fn func([]config.Host) ([]config.Host, error)) error {
	hosts, err := l.Load()
	if err != nil {
		return err
	}
	if hosts, err = fn(hosts); err != nil {
		return err
	}
	if err := config.SaveHosts(l.Env.HostsFile(), hosts); err != nil {
		return err
	}
	Reload(l.Env)
	return nil
}

// Reload tells a running towerd to read hosts.toml again; one that starts
// later reads it anyway.
func Reload(env *config.Env) {
	c := client.New(env)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Call(ctx, proto.CallReload, nil, nil)
}
