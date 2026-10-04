package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/hosts"
	"github.com/jerrykal/tower/internal/proto"
)

func cmdHost(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: tower host add|rm|on|off|ls …")
	}
	env, err := config.Load(nil)
	if err != nil {
		return err
	}
	l := hosts.New(env)
	switch args[0] {
	case "add":
		return hostAdd(l, args[1:], out)
	case "rm", "remove":
		return hostEdit(args[1:], out, "removed", l.Remove)
	case "on", "off":
		on := args[0] == "on"
		return hostEdit(args[1:], out, "turned "+args[0], func(name string) error { return l.SetOn(name, on) })
	case "ls", "list":
		return hostList(env, out)
	}
	return fmt.Errorf("unknown host command %q", args[0])
}

// hostAdd checks the host in order (ssh, tmux, OS, tower), adds it to
// hosts.toml (a failed check keeps it, with the reason and the fix), and
// tells the home.
func hostAdd(l *hosts.List, args []string, out io.Writer) error {
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
	list, err := l.Load()
	if err != nil {
		return err
	}
	aliases := l.Aliases()
	h := config.Host{Name: hosts.Clean(*name), SSH: pos[0], Tmux: *tmuxArgs, Tower: *tower}
	if h.Name == "" {
		h.Name = hosts.Clean(config.DefaultName(pos[0]))
	}
	if *noStandby {
		f := false
		h.Standby = &f
	}
	if err := hosts.CheckName(list, aliases, h.Name, pos[0], -1); err != nil {
		return &exitError{code: 1, msg: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	checks, err := l.Add(ctx, h, nil)
	for _, c := range checks {
		mark := "✓"
		if !c.OK {
			mark = "✗"
		}
		line := mark + " " + c.Name
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(out, line)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "added %s\n", h.Name)
	if hosts.Failed(checks) {
		return &exitError{code: 1, msg: h.Name + " is added but not usable yet: see the reason above"}
	}
	return nil
}

func hostEdit(args []string, out io.Writer, did string, edit func(name string) error) error {
	if len(args) != 1 {
		return errors.New("usage: tower host rm|on|off <name>")
	}
	if err := edit(args[0]); err != nil {
		return &exitError{code: 1, msg: err.Error()}
	}
	fmt.Fprintf(out, "%s %s\n", did, args[0])
	return nil
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
