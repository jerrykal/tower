package main

import (
	"context"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/loop"
)

func init() { attachLoop = runLoop }

// runLoop is the attach loop on this terminal, for this machine's tmux
// server (TOWER_TMUX).
func runLoop(dash bool) error {
	e, err := config.Load(nil)
	if err != nil {
		return err
	}
	if code := loop.Run(context.Background(), loop.Options{Dash: dash, Env: e}); code != 0 {
		return &exitError{code: code}
	}
	return nil
}
