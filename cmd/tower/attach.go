package main

import (
	"errors"
	"flag"
	"os"

	"github.com/jerrykal/tower/internal/loop"
	"github.com/jerrykal/tower/internal/version"
)

// cmdAttach is the attach shim: tower attach --loop L --gen G --home H
// --inst I --mkey K [--standby] --tmux ARGS $3 [@7].
func cmdAttach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	var s loop.Shim
	fs.StringVar(&s.Loop, "loop", "", "the loop's id")
	fs.IntVar(&s.Gen, "gen", 0, "the attach generation")
	fs.StringVar(&s.Home, "home", "", "the loop's home (towerd id)")
	fs.StringVar(&s.Inst, "inst", "", "the tmux server instance the target was listed on")
	fs.StringVar(&s.MKey, "mkey", "", "this machine's key, as its towerd told the home")
	fs.BoolVar(&s.Standby, "standby", false, "wait for the attach's go line first")
	var tm tmuxFlag
	fs.Var(&tm, "tmux", "tmux arguments selecting the server")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	s.Tmux = tm.value()
	s.Version = version.Version
	if !s.Standby {
		if len(pos) < 1 || len(pos) > 2 {
			return errors.New("usage: tower attach --loop L --gen G --home H --inst I --mkey K --tmux ARGS <session> [window]")
		}
		s.Session = pos[0]
		if len(pos) == 2 {
			s.Window = pos[1]
		}
	}
	return loop.RunShim(s, os.Stdin)
}
