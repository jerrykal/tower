package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/test/scenario/fakenet"
)

var bin string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "fakessh")
	bin = filepath.Join(dir, "ssh")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fake struct {
	t   *testing.T
	dir string
}

func newFake(t *testing.T) *fake {
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "hosts"), 0o700)
	t.Setenv("TOWER_FAKE_DIR", d)
	t.Cleanup(func() {
		b, _ := os.ReadFile(filepath.Join(d, "holders"))
		for _, p := range strings.Fields(string(b)) {
			exec.Command("kill", p).Run()
		}
	})
	return &fake{t, d}
}

func (f *fake) set(host string, k Knobs) {
	if k.Env == nil {
		k.Env = map[string]string{"PATH": os.Getenv("PATH"), "HOME": f.dir, "WHO": host}
	}
	fakenet.Save(f.dir, host, &k)
}

func (f *fake) ssh(args ...string) (string, string, int, time.Duration) {
	start := time.Now()
	cmd := exec.Command(bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Run()
	return out.String(), errb.String(), cmd.ProcessState.ExitCode(), time.Since(start)
}

var opts = []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=1", "-o", "ServerAliveInterval=1", "-o", "ServerAliveCountMax=1", "-o", "ControlPath=/x/%C"}

func run(host, cmd string, extra ...string) []string {
	a := append(append([]string{"-T"}, extra...), opts...)
	return append(a, host, "--", cmd)
}

func TestRunsInTheHostsEnvironment(t *testing.T) {
	f := newFake(t)
	f.set("h", Knobs{})
	out, _, code, _ := f.ssh(run("h", "echo $WHO; exit 7")...)
	if out != "h\n" || code != 7 {
		t.Fatalf("%q %d", out, code)
	}
	k := 3
	f.set("h", Knobs{Exit: &k})
	if _, _, code, _ := f.ssh(run("h", "true")...); code != 3 {
		t.Fatalf("exit knob: %d", code)
	}
	_, serr, code, _ := f.ssh(run("nosuch", "true")...)
	if code != 255 || !strings.Contains(serr, "Could not resolve hostname nosuch") {
		t.Fatalf("%q %d", serr, code)
	}
	b, _ := os.ReadFile(filepath.Join(f.dir, "ssh.log"))
	if !strings.Contains(string(b), `"batch":"yes"`) || !strings.Contains(string(b), `"cp":"/x/%C"`) {
		t.Fatalf("log: %s", b)
	}
}

func TestDelay(t *testing.T) {
	f := newFake(t)
	f.set("h", Knobs{DelayMs: 100})
	// A connection (1 RTT), then the output and the exit status each ½.
	out, _, _, d := f.ssh(run("h", "echo hi")...)
	if out != "hi\n" || d < 280*time.Millisecond || d > 700*time.Millisecond {
		t.Fatalf("%q in %v", out, d)
	}
}

func TestDownMessages(t *testing.T) {
	f := newFake(t)
	cases := map[string]string{
		"refused":  "Connection refused",
		"hostkey":  "Host key verification failed.",
		"auth":     "Permission denied",
		"password": "Permission denied",
		"resolve":  "Could not resolve hostname",
		"timeout":  "Operation timed out",
	}
	for down, want := range cases {
		f.set("h", Knobs{Down: down})
		_, serr, code, _ := f.ssh(run("h", "true")...)
		if code != 255 || !strings.Contains(serr, want) {
			t.Errorf("%s: %q %d", down, serr, code)
		}
	}
}

func TestDropAndFreeze(t *testing.T) {
	f := newFake(t)
	f.set("h", Knobs{})
	done := make(chan string, 1)
	go func() {
		_, serr, code, _ := f.ssh(run("h", "sleep 5")...)
		done <- serr + "|" + string(rune('0'+code%10))
	}()
	time.Sleep(300 * time.Millisecond)
	f.set("h", Knobs{Drop: 1})
	select {
	case r := <-done:
		if !strings.Contains(r, "closed by remote host") {
			t.Fatal(r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drop did not end the session")
	}
	// Freeze: ssh gives up after the alive window (1s here); the remote
	// side is kept by a holder.
	f.set("h", Knobs{})
	go func() {
		_, serr, _, _ := f.ssh(run("h", "sleep 5")...)
		done <- serr
	}()
	time.Sleep(300 * time.Millisecond)
	f.set("h", Knobs{Freeze: true})
	select {
	case r := <-done:
		if !strings.Contains(r, "not responding") {
			t.Fatal(r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("freeze never gave up")
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "holders")); len(b) == 0 {
		t.Fatal("no holder kept the remote side")
	}
}

func TestStallBlocksBytes(t *testing.T) {
	f := newFake(t)
	f.set("h", Knobs{})
	cmd := exec.Command(bin, run("h", "cat")...)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Start()
	defer cmd.Process.Kill()
	got := make(chan string, 10)
	go func() {
		buf := make([]byte, 100)
		for {
			n, err := out.Read(buf)
			if err != nil {
				return
			}
			got <- string(buf[:n])
		}
	}()
	io.WriteString(in, "a")
	if s := <-got; s != "a" {
		t.Fatal(s)
	}
	f.set("h", Knobs{Stall: true})
	time.Sleep(100 * time.Millisecond)
	io.WriteString(in, "b")
	select {
	case s := <-got:
		t.Fatalf("a byte moved while stalled: %q", s)
	case <-time.After(400 * time.Millisecond):
	}
	f.set("h", Knobs{})
	select {
	case s := <-got:
		if s != "b" {
			t.Fatal(s)
		}
	case <-time.After(time.Second):
		t.Fatal("stall never ended")
	}
}

func TestMasterCostAndExit(t *testing.T) {
	f := newFake(t)
	f.set("h", Knobs{Mux: true, DelayMs: 40})
	_, _, _, first := f.ssh(run("h", "true")...)
	_, _, _, second := f.ssh(run("h", "true")...)
	// A new master costs 5.5 RTT + ½ + ½; a session on it 1.5 + 1.
	if first < 500*time.Millisecond || second > first*3/4 {
		t.Fatalf("first %v second %v", first, second)
	}
	done := make(chan string, 1)
	go func() {
		_, serr, _, _ := f.ssh(run("h", "sleep 5")...)
		done <- serr
	}()
	time.Sleep(400 * time.Millisecond)
	f.ssh(append([]string{"-O", "exit"}, append(opts, "h")...)...)
	select {
	case r := <-done:
		if !strings.Contains(r, "Shared connection to h closed") {
			t.Fatal(r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("-O exit left the session")
	}
	// A network change: sessions on the old master hang, then give up
	// at halfopen_at + the alive window.
	f.ssh(run("h", "true")...)
	go func() {
		_, serr, _, _ := f.ssh(run("h", "sleep 5")...)
		done <- serr
	}()
	time.Sleep(400 * time.Millisecond)
	f.set("h", Knobs{Mux: true, HalfOpenAt: time.Now().UnixMilli()})
	start := time.Now()
	select {
	case r := <-done:
		if !strings.Contains(r, "not responding") || time.Since(start) < 800*time.Millisecond {
			t.Fatalf("%q after %v", r, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("half-open master never gave up")
	}
	// A new master works.
	if out, _, code, _ := f.ssh(run("h", "echo ok")...); out != "ok\n" || code != 0 {
		t.Fatalf("new master: %q %d", out, code)
	}
}
