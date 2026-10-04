package ui

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/tmux"
	"github.com/jerrykal/tower/internal/transport"
)

// Open is `tower` typed at a prompt inside tmux: it opens the dashboard
// in a popup for its own client, as the key binding would, so tower never
// nests tmux. env are the variables the popup's tower needs (its tmux
// server, machine key, tmux binary); every other TOWER_* variable of this
// process goes along too. self is the tower binary to run, by path, so no
// version manager's shim sits on the popup's start.
func Open(ctx context.Context, sv Tmux, self string, env map[string]string) error {
	out, err := sv.Run(ctx, "display-message", "-p", "#{client_pid}:#{client_created}:#{client_name}")
	if err != nil {
		return err
	}
	id := strings.TrimSpace(out)
	cl, err := proto.ParseClient(id)
	if err != nil {
		return fmt.Errorf("no tmux client to open the dashboard in (%v)", err)
	}
	vars := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "TOWER_") {
			vars[k] = v
		}
	}
	for k, v := range env {
		if v != "" {
			vars[k] = v
		}
	}
	vars["TOWER_CLIENT"] = id
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var cmd strings.Builder
	for _, k := range keys {
		cmd.WriteString(k + "=" + transport.ShellQuote(vars[k]) + " ")
	}
	cmd.WriteString("exec " + transport.ShellQuote(self) + " dash")
	args := append([]string{"display-popup", "-E", "-c", cl.Name}, proto.PopupSize...)
	_, err = sv.Run(ctx, append(args, cmd.String())...)
	return err
}

var _ Tmux = tmux.Server{}
