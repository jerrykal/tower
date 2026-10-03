package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A control master is modelled as the file masters/<host>, holding the
// unix ms it was made. Sessions riding it end when the file goes (ssh -O
// exit, a drop) or changes, and freeze when a network change came after
// it (halfopen_at).

func masterPath(host string) string { return filepath.Join(fakeDir(), "masters", host) }

func readMaster(host string) (int64, bool) {
	b, err := os.ReadFile(masterPath(host))
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil
}

// makeMaster creates the master file; false if another session won.
func makeMaster(host string) (int64, bool) {
	os.MkdirAll(filepath.Dir(masterPath(host)), 0o700)
	f, err := os.OpenFile(masterPath(host), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, false
	}
	now := time.Now().UnixMilli()
	fmt.Fprintln(f, now)
	f.Close()
	return now, true
}

// removeMaster removes the master file if it is still the one made at
// born.
func removeMaster(host string, born int64) {
	if v, ok := readMaster(host); ok && v == born {
		os.Remove(masterPath(host))
	}
}
