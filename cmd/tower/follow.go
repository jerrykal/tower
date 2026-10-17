package main

import (
	"context"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/version"
)

// followedFrom is the version of the tower that followed into this one.
var followedFrom string

// stays are the commands that never follow: towerd and the helpers a
// towerd or a home runs by their exact path, and what this build says of
// itself. Every other command follows, one a later build added included.
var stays = map[string]bool{
	"towerd": true, "_keep": true, "_ensure": true, "_ui": true, "attach": true,
	"version": true, "--version": true, "-V": true, "help": true, "--help": true, "-h": true,
}

// follow re-runs this command as the build the local towerd runs when
// that towerd is remote-only and a home of its build is connected
// (served): it is the home's build, so tower typed on a remote behaves
// as the home's own, whatever tower PATH found. It returns when there is
// nothing to follow (no towerd, the same version, not served,
// TOWER_FOLLOW=0) or the exec fails. A binding runs towerd's binary
// already (TOWER_CLIENT set). The towerd asked is the one the command
// acts on: the pane's server for the dashboard, --tmux or TOWER_TMUX for
// the rest.
func follow(cmd string, args []string) {
	if v, ok := os.LookupEnv("TOWER_FOLLOWED"); ok {
		// Kept from what this process starts: a tmux server an attach
		// loop starts would hand it to every pane.
		followedFrom = v
		os.Unsetenv("TOWER_FOLLOWED")
		if v != "" {
			return
		}
	}
	if stays[cmd] || os.Getenv("TOWER_CLIENT") != "" || !config.Flag("TOWER_FOLLOW", true) {
		return
	}
	tmuxArgs := tmuxArg(args)
	if cmd == "" || cmd == "dash" {
		tmuxArgs = serverArgs()
	}
	e, err := config.Load(tmuxArgs)
	if err != nil {
		return
	}
	// The machine key costs a subprocess on macOS: the command's own
	// Load, and the build it follows into, take it from here.
	os.Setenv("TOWER_MKEY", e.MKey)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var st proto.Status
	if err := client.New(e).Call(ctx, proto.CallStatus, proto.StatusArgs{}, &st); err != nil {
		return
	}
	if !st.Served || st.Home || st.Version == version.Version || st.Exe == "" {
		return
	}
	argv := append([]string{st.Exe}, args...)
	syscall.Exec(st.Exe, argv, append(os.Environ(), "TOWER_FOLLOWED="+version.Version))
}

// tmuxArg is the --tmux value in args as the command's flag parses it,
// nil when there is none (TOWER_TMUX applies).
func tmuxArg(args []string) []string {
	for i, a := range args {
		if a == "--" {
			break
		}
		for _, p := range []string{"--tmux", "-tmux"} {
			if a == p && i+1 < len(args) {
				return fieldsOrEmpty(args[i+1])
			}
			if v, ok := strings.CutPrefix(a, p+"="); ok {
				return fieldsOrEmpty(v)
			}
		}
	}
	return nil
}

func fieldsOrEmpty(v string) []string {
	if f := strings.Fields(v); f != nil {
		return f
	}
	return []string{}
}
