package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// fakeTowerd answers status, refuses "nope", and never answers "hang".
func fakeTowerd(t *testing.T) *Client {
	dir, err := os.MkdirTemp("/tmp", "tc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	e := &config.Env{RunDir: dir, Tag: "t", StateDir: dir}
	l, err := net.Listen("unix", e.Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var call proto.Call
				json.Unmarshal(line, &call)
				var r proto.Reply
				switch call.Op {
				case "status":
					r.Result, _ = json.Marshal(proto.Status{ID: "abcd1234", Version: call.Version})
				case "nope":
					r.Err = "refused: because"
				case "hang":
					time.Sleep(10 * time.Second)
				}
				b, _ := json.Marshal(r)
				c.Write(append(b, '\n'))
			}()
		}
	}()
	return &Client{Env: e, Version: "0.0.1"}
}

func TestCall(t *testing.T) {
	c := fakeTowerd(t)
	var st proto.Status
	if err := c.Call(context.Background(), "status", nil, &st); err != nil || st.ID != "abcd1234" || st.Version != "0.0.1" {
		t.Fatalf("%+v %v", st, err)
	}
	err := c.Call(context.Background(), "nope", nil, nil)
	var ce *CallError
	if !errors.As(err, &ce) || ce.Msg != "refused: because" {
		t.Fatalf("refusal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Call(ctx, "hang", nil, nil); err == nil || time.Since(start) > time.Second {
		t.Fatalf("a cancelled call returns at once: %v after %v", err, time.Since(start))
	}
}

func TestNoTowerd(t *testing.T) {
	e := &config.Env{RunDir: t.TempDir(), Tag: "none"}
	err := (&Client{Env: e}).Call(context.Background(), "status", nil, nil)
	if !errors.Is(err, ErrNoTowerd) {
		t.Fatal(err)
	}
}

// The default server (no tmux arguments) is named as such: a towerd
// started for it never takes TOWER_TMUX from its environment.
func TestStartArgs(t *testing.T) {
	for _, tc := range []struct {
		tmux    []string
		bridged bool
		want    []string
	}{
		{[]string{}, true, []string{"towerd", "--bridged", "--tmux", ""}},
		{nil, false, []string{"towerd", "--tmux", ""}},
		{[]string{"-L", "work"}, false, []string{"towerd", "--tmux", "-L work"}},
	} {
		c := &Client{Env: &config.Env{Tmux: tc.tmux}, Bridged: tc.bridged}
		if got := c.startArgs(); !slices.Equal(got, tc.want) {
			t.Errorf("tmux %q, bridged %v: %q, want %q", tc.tmux, tc.bridged, got, tc.want)
		}
	}
}

func TestLockHeld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	f, _ := os.Create(p)
	defer f.Close()
	if LockHeld(p) {
		t.Fatal("not held yet")
	}
	if err := lockFile(f); err != nil {
		t.Fatal(err)
	}
	if !LockHeld(p) {
		t.Fatal("held")
	}
}
