package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/tmux"
	"github.com/jerrykal/tower/internal/ui"
)

// attachLoop is `tower` (and, with dash, `tower dash`) outside tmux: the
// attach loop, which owns the terminal. The loop sets it.
var attachLoop = func(dash bool) error {
	return errors.New("the attach loop is not built yet")
}

// towerCmd is `tower` and `tower dash`. Inside tmux it is the dashboard,
// never a nested attach: in the popup a key binding opened (TOWER_CLIENT
// names the client that pressed it) it runs here; typed at a prompt, it
// opens that popup for its own client.
func towerCmd(dash bool) error {
	if os.Getenv("TMUX") == "" {
		return attachLoop(dash)
	}
	ctx := context.Background()
	e, err := config.Load(serverArgs())
	if err != nil {
		return err
	}
	id := os.Getenv("TOWER_CLIENT")
	c, err := ui.Dial(ctx, e, id)
	if err != nil {
		return err
	}
	if id != "" {
		return ui.Run(ctx, c)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return ui.Open(ctx, c.Tmux, self, map[string]string{
		"TOWER_TMUX":     strings.Join(e.Tmux, " "),
		"TOWER_MKEY":     e.MKey,
		"TOWER_TMUX_BIN": tmux.Bin(),
	})
}

// uiScript is `tower _ui …`: the dashboard's actions without the TUI.
func uiScript(args []string) error {
	ctx := context.Background()
	e, err := config.Load(serverArgs())
	if err != nil {
		return err
	}
	c, err := ui.Dial(ctx, e, os.Getenv("TOWER_CLIENT"))
	if err != nil {
		return err
	}
	return ui.Script(ctx, c, args, os.Stdout)
}

// serverArgs names the tmux server we run in when TOWER_TMUX does not
// (nil: let config read TOWER_TMUX): from $TMUX's socket path, `-L name`
// when the socket is in tmux's own directory, else `-S path`. A `tower`
// typed in a `tmux -L work` session then reaches that server's towerd.
func serverArgs() []string {
	if os.Getenv("TOWER_TMUX") != "" {
		return nil
	}
	sock, _, _ := strings.Cut(os.Getenv("TMUX"), ",")
	if sock == "" {
		return nil
	}
	tmp := os.Getenv("TMUX_TMPDIR")
	if tmp == "" {
		tmp = "/tmp"
	}
	dir, name := filepath.Split(sock)
	if sameDir(dir, filepath.Join(tmp, fmt.Sprintf("tmux-%d", os.Getuid()))) {
		if name == "default" {
			return []string{}
		}
		return []string{"-L", name}
	}
	return []string{"-S", sock}
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}
