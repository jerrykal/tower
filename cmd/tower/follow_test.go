package main

import (
	"os"
	"slices"
	"testing"
)

// TestMain keeps the tests off the user's tower: no follow into the
// build of a towerd running here, and config and state in a directory of
// their own.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tower-cmd-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("TOWER_FOLLOW", "0")
	os.Setenv("TOWER_HOME", dir)
	os.Unsetenv("TOWER_TMUX")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestTmuxArg(t *testing.T) {
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"status"}, nil},
		{[]string{"status", "--tmux", "-L x"}, []string{"-L", "x"}},
		{[]string{"status", "--json", "--tmux=-L y"}, []string{"-L", "y"}},
		{[]string{"status", "-tmux", ""}, []string{}},
		{[]string{"host", "add", "--", "--tmux", "-L z"}, nil},
	} {
		got := tmuxArg(c.args)
		if !slices.Equal(got, c.want) || (got == nil) != (c.want == nil) {
			t.Errorf("%q: %q, want %q", c.args, got, c.want)
		}
	}
}

// A followed tower takes TOWER_FOLLOWED out of its environment, even an
// empty one, and never follows again.
func TestFollowedTakesTheMarkOut(t *testing.T) {
	t.Setenv("TOWER_FOLLOW", "1")
	t.Setenv("TOWER_FOLLOWED", "0.0.9")
	follow("status", []string{"status"})
	if _, ok := os.LookupEnv("TOWER_FOLLOWED"); ok || followedFrom != "0.0.9" {
		t.Fatalf("followed from %q, mark left %v", followedFrom, ok)
	}
	followedFrom = ""
	t.Setenv("TOWER_FOLLOWED", "")
	t.Setenv("TOWER_FOLLOW", "0")
	follow("status", []string{"status"})
	if _, ok := os.LookupEnv("TOWER_FOLLOWED"); ok || followedFrom != "" {
		t.Fatalf("an empty mark: followed from %q, left %v", followedFrom, ok)
	}
}
