package main

import (
	"context"
	"os"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/tmux"
)

// cmdLast is tower last, tmux's switch-client -l across hosts, run by
// prefix L for the client that pressed it (TOWER_CLIENT). It falls back to
// tmux's own switch-client -l when the terminal has no loop, the loop has
// no previous session, or towerd cannot be reached.
func cmdLast(args []string) error {
	cl := os.Getenv("TOWER_CLIENT")
	id, _ := proto.ParseClient(cl)
	name := id.Name
	env, err := config.Load(nil)
	srv := tmux.Server{Bin: tmux.Bin()}
	if err == nil {
		srv.Args = env.Tmux
	}
	fallback := func() error {
		args := []string{"switch-client", "-l"}
		if name != "" {
			args = append(args, "-c", name)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := srv.Run(ctx, args...)
		return err
	}
	if err != nil || cl == "" {
		return fallback()
	}
	c := client.New(env)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var res proto.LastResult
	if err := c.Call(ctx, proto.CallLast, proto.LastArgs{Client: cl}, &res); err != nil {
		return fallback()
	}
	switch {
	case res.Stored:
		return nil // the loop moves the terminal
	case res.Local:
		tmux.UseBin(srv.Bin)
		a := []string{"switch-client", "-c", name, "-t", res.Target.Session}
		if res.Target.Window != "" {
			a = append(a, ";", "select-window", "-t", res.Target.Window)
		}
		_, err := srv.Run(ctx, a...)
		return err
	}
	return fallback()
}
