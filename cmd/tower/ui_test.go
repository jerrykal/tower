package main

import (
	"fmt"
	"os"
	"slices"
	"testing"
)

func TestServerArgs(t *testing.T) {
	dir := t.TempDir()
	own := fmt.Sprintf("%s/tmux-%d", dir, os.Getuid())
	t.Setenv("TMUX_TMPDIR", dir)
	for _, c := range []struct {
		towerTmux, tmux string
		want            []string
	}{
		{"-L x", own + "/work,1,0", nil},
		{"", own + "/default,1,0", []string{}},
		{"", own + "/work,1,0", []string{"-L", "work"}},
		{"", "/elsewhere/sock,1,0", []string{"-S", "/elsewhere/sock"}},
		{"", "", nil},
	} {
		t.Setenv("TOWER_TMUX", c.towerTmux)
		t.Setenv("TMUX", c.tmux)
		got := serverArgs()
		if !slices.Equal(got, c.want) || (got == nil) != (c.want == nil) {
			t.Errorf("TOWER_TMUX=%q TMUX=%q: %q, want %q", c.towerTmux, c.tmux, got, c.want)
		}
	}
}

func TestOutsideTmuxIsTheLoop(t *testing.T) {
	t.Setenv("TMUX", "")
	called := false
	defer func(f func(bool) error) { attachLoop = f }(attachLoop)
	attachLoop = func(dash bool) error { called = dash; return nil }
	if err := run([]string{"dash"}); err != nil || !called {
		t.Fatalf("tower dash outside tmux runs the attach loop: %v", err)
	}
}
