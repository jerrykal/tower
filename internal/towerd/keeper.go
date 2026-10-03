package towerd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// keepEvery is how often the keeper checks its towerd.
const keepEvery = 250 * time.Millisecond

// Keep is `tower _keep <pid> <state dir>`, the one pane of _tower. While
// towerd pid lives it waits. Once it is gone, a towerd started since (in
// towerd.pid) is watched instead; with none, the keeper ends the control
// client the dead towerd left (ctl.pid) and exits, which ends _tower:
// neither keeps the server alive after the last session.
func Keep(pid int, stateDir string) {
	for {
		for alive(pid) {
			time.Sleep(keepEvery)
		}
		if next := readPid(filepath.Join(stateDir, "towerd.pid")); next > 1 && next != pid && alive(next) {
			pid = next
			continue
		}
		if ctl := readPid(filepath.Join(stateDir, "ctl.pid")); ctl > 1 && alive(ctl) && isControlClient(ctl) {
			if cur := readPid(filepath.Join(stateDir, "towerd.pid")); cur <= 1 || ppid(ctl) != cur || !alive(cur) {
				syscall.Kill(ctl, syscall.SIGKILL)
			}
		}
		return
	}
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func readPid(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}
