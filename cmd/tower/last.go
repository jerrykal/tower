package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/tmux"
	"github.com/jerrykal/tower/internal/ui"
)

// cmdLast is tower last, tmux's switch-client -l across hosts, run by
// prefix L for the client that pressed it (TOWER_CLIENT). It falls back to
// tmux's own switch-client -l when the terminal has no loop, the loop has
// no previous session, or towerd cannot be reached. A hand-off refused
// says why on the client, as tmux's own errors do.
func cmdLast(args []string) error {
	cl := os.Getenv("TOWER_CLIENT")
	id, _ := proto.ParseClient(cl)
	name := id.Name
	env, err := config.Load(nil)
	srv := tmux.Server{Bin: tmux.Bin()}
	if err == nil {
		srv.Args = env.Tmux
	}
	// An error once the pressing client is gone (a hand-off before this
	// one ended it) is moot: run-shell would only put the pane, which the
	// terminal's next client may show, in view mode with it.
	settle := func(err error) error {
		if err == nil || name == "" {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if listed, lerr := ui.ClientListed(ctx, srv, id); lerr == nil && !listed {
			return nil
		}
		return err
	}
	fallback := func() error {
		args := []string{"switch-client", "-l"}
		if name != "" {
			args = append(args, "-c", name)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := srv.Run(ctx, args...)
		return settle(err)
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
	case res.Stored && !res.Asker:
		// The loop ends the client (Ended), or a towerd from before the
		// asker's part says neither: the loop moves the terminal.
		return nil
	case res.Stored:
		// The loop was not waiting to end the client (none waiting, or
		// it confirmed too late): end it here.
		tmux.UseBin(srv.Bin)
		dash := &ui.Conn{Tmux: srv, Client: cl}
		return settle(dash.EndClient(ctx))
	case res.Local:
		tmux.UseBin(srv.Bin)
		a := []string{"switch-client", "-c", name, "-t", res.Target.Session}
		if res.Target.Window != "" {
			a = append(a, ";", "select-window", "-t", res.Target.Window)
		}
		_, err := srv.Run(ctx, a...)
		return settle(err)
	case !res.Target.IsZero():
		// Refused: an earlier attach's client (its loop moved on since
		// the key), or a host not up.
		tmux.UseBin(srv.Bin)
		if _, err := srv.Run(ctx, refusal(name, res.Note)...); err != nil && strings.Contains(err.Error(), "usage: display-message") {
			// tmux 3.2 takes -c for a flag: say it on the client tmux
			// finds for the pane prefix L was pressed in, most likely the
			// one that pressed it.
			srv.Run(ctx, refusal("", res.Note)...)
		}
		return nil
	}
	return fallback()
}

// refusal is the display-message that says on client name (none: the
// client tmux finds) why the home refused a hand-off. display-message
// expands formats, and the note can hold a host's ssh error: its #s are
// doubled, so a #(…) in it stays text.
func refusal(name, note string) []string {
	a := []string{"display-message"}
	if name != "" {
		a = append(a, "-c", name)
	}
	return append(a, tmux.Literal("tower: "+note))
}
