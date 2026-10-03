// Command tower brings the tmux servers on several machines together:
// switch between, manage and watch sessions across hosts as if they were
// on one tmux server.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jerrykal/tower/internal/version"
)

// exitError ends the process with a code and an optional message.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func main() {
	if err := run(os.Args[1:]); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.msg != "" {
				fmt.Fprintln(os.Stderr, "tower:", ee.msg)
			}
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "tower:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return towerCmd(false)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "dash":
		return towerCmd(true)
	case "_ui":
		return uiScript(rest)
	case "version", "--version", "-V":
		fmt.Println(version.Version)
		return nil
	case "help", "--help", "-h":
		return usage()
	case "towerd":
		return cmdTowerd(rest)
	case "_keep":
		return cmdKeep(rest)
	case "_ensure":
		return cmdEnsure(rest)
	case "status":
		return cmdStatus(rest)
	case "stop":
		return cmdStop(rest)
	case "netchange":
		return cmdNetChange(rest)
	case "host":
		return cmdHost(rest, os.Stdout)
	case "last":
		return cmdLast(rest)
	case "attach":
		return cmdAttach(rest)
	}
	return fmt.Errorf("unknown command %q (see tower help)", cmd)
}

func usage() error {
	fmt.Print(`usage: tower [<command>]

  (none)                   outside tmux: attach this terminal, to the last
                           target or through the picker; inside: the dashboard
commands:
  dash                     the same, starting at the picker (esc: the last target)
  status [--json]          what the local towerd knows: hosts, loops, clients
  stop                     stop the local towerd
  host add <ssh target>    add a host [--name N] [--tmux ARGS] [--tower PATH]
  host rm|on|off <name>    remove a host, or turn it on or off
  host ls                  list the hosts
  last                     go to the previous session, across hosts
  netchange                tell towerd the network changed
  towerd                   run the daemon [--bridged] [--stdio] [--tmux ARGS]
  version                  print tower's version
  help                     show this help
`)
	return nil
}

// parseFlags parses fs over args with flags and positional arguments in
// any order, returning the positional ones.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// tmuxFlag is --tmux: tmux arguments selecting the server, as one string.
type tmuxFlag struct {
	set  bool
	args []string
}

func (t *tmuxFlag) String() string { return strings.Join(t.args, " ") }
func (t *tmuxFlag) Set(v string) error {
	t.set, t.args = true, strings.Fields(v)
	return nil
}

// value is nil when --tmux was not given, so TOWER_TMUX applies.
func (t *tmuxFlag) value() []string {
	if !t.set {
		return nil
	}
	if t.args == nil {
		return []string{}
	}
	return t.args
}
