package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/install"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/transport"
	"github.com/jerrykal/tower/internal/version"
)

func cmdHost(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: tower host add|rm|on|off|ls …")
	}
	env, err := config.Load(nil)
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		return hostAdd(env, args[1:], out)
	case "rm", "remove":
		return hostEdit(env, args[1:], out, "removed", func(hs []config.Host, i int) []config.Host { return slices.Delete(hs, i, i+1) })
	case "on", "off":
		on := args[0] == "on"
		return hostEdit(env, args[1:], out, "turned "+args[0], func(hs []config.Host, i int) []config.Host {
			if on {
				hs[i].Enabled = nil
			} else {
				hs[i].Enabled = &on
			}
			return hs
		})
	case "ls", "list":
		return hostList(env, out)
	}
	return fmt.Errorf("unknown host command %q", args[0])
}

func findHost(hosts []config.Host, name string) int {
	return slices.IndexFunc(hosts, func(h config.Host) bool { return strings.EqualFold(h.Name, name) })
}

// hostAdd checks the host in order (ssh, tmux, OS, tower), adds it to
// hosts.toml (a failed check keeps it, with the reason and the fix), and
// tells the home.
func hostAdd(env *config.Env, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("host add", flag.ContinueOnError)
	name := fs.String("name", "", "the host's label (default: from the ssh target)")
	tmuxArgs := fs.String("tmux", "", "tmux arguments selecting the server there (-L name)")
	tower := fs.String("tower", "", "a tower binary you manage there (turns off install on connect)")
	noStandby := fs.Bool("no-standby", false, "no standby sessions for this host")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: tower host add <ssh target> [--name N] [--tmux ARGS] [--tower PATH]")
	}
	h := config.Host{Name: *name, SSH: pos[0], Tmux: *tmuxArgs, Tower: *tower}
	if h.Name == "" {
		h.Name = config.DefaultName(pos[0])
	}
	if h.SSH == h.Name {
		h.SSH = ""
	}
	if *noStandby {
		f := false
		h.Standby = &f
	}
	hosts, err := config.LoadHosts(env.HostsFile())
	if err != nil {
		return err
	}
	if findHost(hosts, h.Name) >= 0 {
		return &exitError{code: 1, msg: fmt.Sprintf("a host named %q already exists", h.Name)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ssh := transport.New(env.CMDir())
	self, _ := os.Executable()
	inst := install.New(version.Version, self)
	towerCmd := inst.Command()
	if h.Tower != "" {
		towerCmd = transport.RemoteCommand(h.Tower)
	}
	towerCmd = transport.WithHome(h.Home, towerCmd)
	failed := false
	checks := ssh.CheckHost(ctx, h, towerCmd)
	if last := &checks[len(checks)-1]; last.Name == "tower" && !last.OK && h.Tower == "" {
		// The last check: put this build there, as the home would on connect.
		ictx, icancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err := inst.Install(ictx, ssh.On(h), h.Name)
		icancel()
		if err != nil {
			last.Detail = err.Error()
		} else {
			last.OK, last.Detail = true, "installed "+version.Version
		}
	}
	for _, c := range checks {
		mark := "✓"
		if !c.OK {
			mark, failed = "✗", true
		}
		line := mark + " " + c.Name
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(out, line)
	}
	hosts = append(hosts, h)
	if err := config.SaveHosts(env.HostsFile(), hosts); err != nil {
		return err
	}
	fmt.Fprintf(out, "added %s\n", h.Name)
	reloadHome(env)
	if failed {
		return &exitError{code: 1, msg: h.Name + " is added but not usable yet: see the reason above"}
	}
	return nil
}

func hostEdit(env *config.Env, args []string, out io.Writer, did string, edit func([]config.Host, int) []config.Host) error {
	if len(args) != 1 {
		return errors.New("usage: tower host rm|on|off <name>")
	}
	hosts, err := config.LoadHosts(env.HostsFile())
	if err != nil {
		return err
	}
	i := findHost(hosts, args[0])
	if i < 0 {
		return &exitError{code: 1, msg: fmt.Sprintf("no host named %q", args[0])}
	}
	name := hosts[i].Name
	hosts = edit(hosts, i)
	if err := config.SaveHosts(env.HostsFile(), hosts); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s %s\n", did, name)
	reloadHome(env)
	return nil
}


// reloadHome tells a running towerd to read hosts.toml again; one that
// starts later reads it anyway.
func reloadHome(env *config.Env) {
	c := client.New(env)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Call(ctx, proto.CallReload, nil, nil)
}

func hostList(env *config.Env, out io.Writer) error {
	hosts, err := config.LoadHosts(env.HostsFile())
	if err != nil {
		return err
	}
	status := map[string]proto.LinkStatus{}
	c := client.New(env)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var st proto.Status
	if c.Call(ctx, proto.CallStatus, proto.StatusArgs{Full: true}, &st) == nil && st.Detail != nil {
		for _, l := range st.Detail.Links {
			status[l.Name] = l
		}
	}
	for _, h := range hosts {
		s := "off"
		if h.On() {
			s = "?"
		}
		if l, ok := status[h.Name]; ok {
			s = l.Status
			if l.Reason != "" {
				s += ": " + l.Reason
			}
		}
		fmt.Fprintf(out, "%-12s %-24s %s\n", h.Name, h.Target(), s)
	}
	return nil
}
