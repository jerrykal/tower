// Command sshwrap is the scenario suite's ssh: it logs the call
// ($TOWER_TEST_SSH_LOG), then runs ssh with the world's config
// ($TOWER_TEST_SSH_CONFIG) and neither the user's nor the system's.
package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/jerrykal/tower/test/scenario/sshcall"
)

func main() {
	if a, err := sshcall.Parse(os.Args[1:]); err == nil && a != nil {
		sshcall.Log(a)
	}
	argv := append([]string{"ssh", "-F", os.Getenv("TOWER_TEST_SSH_CONFIG")}, os.Args[1:]...)
	err := syscall.Exec("/usr/bin/ssh", argv, os.Environ())
	fmt.Fprintln(os.Stderr, "sshwrap:", err)
	os.Exit(255)
}
