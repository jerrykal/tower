// Command tower brings the tmux servers on several machines together:
// switch between, manage and watch sessions across hosts as if they were
// on one tmux server.
package main

import (
	"fmt"
	"os"

	"github.com/jerrykal/tower/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tower:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return towerCmd(false)
	}
	switch args[0] {
	case "dash":
		return towerCmd(true)
	case "_ui":
		return uiScript(args[1:])
	case "version", "--version", "-V":
		fmt.Println(version.Version)
		return nil
	case "help", "--help", "-h":
		return usage()
	}
	return fmt.Errorf("unknown command %q (see tower help)", args[0])
}

func usage() error {
	fmt.Print(`usage: tower <command>

commands:
  version   print tower's version
  help      show this help
`)
	return nil
}
