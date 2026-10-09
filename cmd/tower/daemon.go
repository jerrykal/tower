package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/towerd"
	"github.com/jerrykal/tower/internal/version"
)

func cmdTowerd(args []string) error {
	fs := flag.NewFlagSet("towerd", flag.ContinueOnError)
	bridged := fs.Bool("bridged", false, "started by a home's bridge: remote-only until a loop or reload")
	stdio := fs.Bool("stdio", false, "the bridge: join stdin and stdout to the local towerd")
	var tm tmuxFlag
	fs.Var(&tm, "tmux", "tmux arguments selecting the server")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	env, err := config.Load(tm.value())
	if err != nil {
		return err
	}
	if *stdio {
		return towerd.Bridge(env, version.Version, os.Stdin, os.Stdout)
	}
	err = towerd.Run(towerd.Options{Env: env, Version: version.Version, Bridged: *bridged})
	if errors.Is(err, towerd.ErrRunning) {
		return &exitError{code: 1, msg: err.Error()}
	}
	return err
}

func cmdKeep(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: tower _keep <towerd pid> <state dir>")
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		return err
	}
	towerd.Keep(pid, args[1])
	return nil
}

// localClient is a client of this machine's towerd for TOWER_TMUX's
// server, or --tmux's.
func localClient(tmuxArgs []string) (*client.Client, error) {
	env, err := config.Load(tmuxArgs)
	if err != nil {
		return nil, err
	}
	return client.New(env), nil
}

// cmdEnsure makes sure a current towerd runs and prints its status.
func cmdEnsure(args []string) error {
	fs := flag.NewFlagSet("_ensure", flag.ContinueOnError)
	var tm tmuxFlag
	fs.Var(&tm, "tmux", "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := localClient(tm.value())
	if err != nil {
		return err
	}
	st, err := c.Ensure(context.Background())
	if err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	fmt.Println(string(b))
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the full status as JSON")
	var tm tmuxFlag
	fs.Var(&tm, "tmux", "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := localClient(tm.value())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var st proto.Status
	if err := c.Call(ctx, proto.CallStatus, proto.StatusArgs{Full: true}, &st); err != nil {
		if errors.Is(err, client.ErrNoTowerd) {
			return &exitError{code: 1, msg: "towerd is not running"}
		}
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Print(formatStatus(&st))
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func formatStatus(st *proto.Status) string {
	var b strings.Builder
	role := "remote only"
	if st.Home {
		role = "home"
	}
	fmt.Fprintf(&b, "towerd %s  id %s  pid %d  %s  (%s)\n", st.Version, st.ID, st.Pid, st.Tag, role)
	if st.Detail == nil {
		return b.String()
	}
	w := st.Detail.Watch
	switch {
	case w.NoServer:
		b.WriteString("tmux: no server\n")
	default:
		fmt.Fprintf(&b, "tmux: %d sessions, server %s, control client %s\n", w.Sessions, w.Inst, w.CtlName)
	}
	if w.Keys != "" {
		fmt.Fprintf(&b, "keys: %s\n", w.Keys)
	}
	if ds := st.Detail.Dirs; ds.Git != "" || ds.Zoxide != "" {
		fmt.Fprintf(&b, "dirs: %d from zoxide (%s), %d repos (git %s); last refresh %dms (%d git runs), last look %dms (%d)\n",
			ds.Dirs, orNone(ds.Zoxide), ds.Repos, orNone(ds.Git), ds.TimerMs, ds.TimerRun, ds.LookMs, ds.LookRun)
	}
	if len(st.Detail.Links) > 0 {
		b.WriteString("hosts:\n")
		for _, l := range st.Detail.Links {
			fmt.Fprintf(&b, "  %-12s %-10s", l.Name, l.Status)
			if l.Status == proto.StatusUp || l.Status == proto.StatusStalled {
				fmt.Fprintf(&b, " %d sessions, rtt %dms", l.Sessions, l.RTT)
			}
			if l.Reason != "" {
				fmt.Fprintf(&b, " %s", l.Reason)
			}
			if l.Warn != "" {
				fmt.Fprintf(&b, " (%s)", l.Warn)
			}
			b.WriteByte('\n')
		}
	}
	for _, h := range st.Detail.Homes {
		state := "connected"
		if !h.Live {
			state = fmt.Sprintf("gone %s ago", (time.Duration(h.Age) * time.Millisecond).Round(time.Second))
		}
		fmt.Fprintf(&b, "home %s (%s) as %q: %s\n", h.Name, h.ID, h.As, state)
	}
	names := map[string]string{}
	for _, l := range st.Detail.Links {
		names[l.ID] = l.Name
	}
	for _, l := range st.Detail.Loops {
		fmt.Fprintf(&b, "loop %s gen %d: at %s, before %s\n", l.ID, l.Gen, l.Cur.String(), l.Prev.String())
		for _, s := range l.Standbys {
			name := names[s.Host]
			if name == "" {
				name = s.Host
			}
			state := "none"
			if s.State != "" {
				state = fmt.Sprintf("%s %s", s.State, (time.Duration(s.Ms) * time.Millisecond).Round(time.Second))
				if s.Again {
					state += ", reusable"
				}
			}
			fmt.Fprintf(&b, "  standby %s: %s; sessions opened %d, reused %d, given up %d", name, state, s.Opened, s.Reused, s.GivenUp)
			if s.Why != "" {
				fmt.Fprintf(&b, " (last: %s)", s.Why)
			}
			b.WriteByte('\n')
		}
		if e := l.End; e != nil {
			fmt.Fprintf(&b, "  last detach into a standby: gen %d ok=%v in %dms, %s ago", e.Gen, e.OK, e.Ms, (time.Duration(e.Ago) * time.Millisecond).Round(time.Second))
			if e.Note != "" {
				fmt.Fprintf(&b, " (%s)", e.Note)
			}
			b.WriteByte('\n')
		}
	}
	for _, c := range st.Detail.Clients {
		fmt.Fprintf(&b, "client %s (pid %d) of loop %s at home %s", c.Name, c.Pid, c.Loop, c.Home)
		if c.Reuse {
			b.WriteString(", reusable")
		}
		if c.End {
			b.WriteString(", to be detached once seen")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	var tm tmuxFlag
	fs.Var(&tm, "tmux", "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := localClient(tm.value())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, proto.CallStop, nil, nil); err != nil {
		if errors.Is(err, client.ErrNoTowerd) {
			return nil
		}
		return err
	}
	for ctx.Err() == nil {
		if _, err := os.Stat(c.Env.Socket()); err != nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("towerd did not stop")
}

func cmdNetChange(args []string) error {
	c, err := localClient(nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Call(ctx, proto.CallNetChange, nil, nil)
}
