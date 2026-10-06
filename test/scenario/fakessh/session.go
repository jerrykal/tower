package main

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jerrykal/tower/internal/relay"
)

// conn is one ssh connection: its own, or a session on the host's master.
type conn struct {
	a      *args
	w      *watch
	master int64 // the master this session rides or made; 0: its own connection
	drop   int   // the drop count when it started
}

// end says why a session ended.
type end int

const (
	endExit    end = iota // the remote command exited
	endDrop               // the host dropped the connection
	endMaster             // the master went away
	endTimeout            // half-open past the alive window
	endSignal             // ended locally
)

func session(a *args, k *Knobs) int {
	c := &conn{a: a, w: newWatch(a.host, k), drop: k.Drop}
	if code, ok := c.connect(); !ok {
		return code
	}
	switch {
	case a.tty && isTerminal(os.Stdin) && c.w.get().Pty:
		return c.runPty()
	case a.tty && isTerminal(os.Stdin):
		return c.runTTY()
	default:
		return c.runPlain()
	}
}

// useMaster reports whether a session rides an existing master, as ssh
// does with any ControlMaster value: "no" only declines to become one
// (the home's probe relies on that to test the master itself).
func (c *conn) useMaster() bool {
	return c.w.get().Mux
}

// connect opens the connection, costing what a real one costs, or fails
// as the host's knobs say.
func (c *conn) connect() (int, bool) {
	k := c.w.get()
	rtt := k.RTT()
	if c.useMaster() {
		if m, ok := readMaster(c.a.host); ok {
			c.master = m
			if c.halfOpen() {
				return c.hangUntilGiveUp(), false
			}
			time.Sleep(rtt * 3 / 2)
			return 0, true
		}
	}
	if k.Down != "" {
		return refuse(c.a, &k), false
	}
	if k.Freeze {
		if k.Mux {
			// A new master waits out the freeze, up to ConnectTimeout.
			if !c.waitThaw(c.a.connectTimeout()) {
				fmt.Fprintf(os.Stderr, "ssh: connect to host %s port 22: Operation timed out\n", c.a.host)
				return 255, false
			}
			time.Sleep(time.Duration(rand.IntN(1000)) * time.Millisecond)
		} else if !c.waitThaw(c.a.alive()) {
			fmt.Fprintf(os.Stderr, "Timeout, server %s not responding.\n", c.a.host)
			return 255, false
		}
	}
	time.Sleep(time.Duration(k.LatencyMs) * time.Millisecond)
	if !k.Mux {
		time.Sleep(rtt)
		return 0, true
	}
	if c.a.opt("ControlMaster") != "no" {
		if m, ok := makeMaster(c.a.host); ok {
			c.master = m
		}
	}
	time.Sleep(rtt * 11 / 2)
	return 0, true
}

// waitThaw waits for the freeze to end, at most d (0: forever).
func (c *conn) waitThaw(d time.Duration) bool {
	start := time.Now()
	for c.w.get().Freeze {
		if d > 0 && time.Since(start) >= d {
			return false
		}
		time.Sleep(pollEvery)
	}
	return true
}

// halfOpen reports whether the master this session is on was made before
// a network change: dead, though alive locally.
func (c *conn) halfOpen() bool {
	if c.master == 0 {
		return false
	}
	k := c.w.get()
	return k.HalfOpenAt > c.master && time.Now().UnixMilli() >= k.HalfOpenAt
}

// frozen: nothing moves on this connection.
func (c *conn) frozen() bool {
	if c.master != 0 {
		return c.halfOpen()
	}
	return c.w.get().Freeze
}

func (c *conn) hangUntilGiveUp() int {
	for {
		if c.giveUp() {
			removeMaster(c.a.host, c.master)
			fmt.Fprintf(os.Stderr, "Timeout, server %s not responding.\n", c.a.host)
			return 255
		}
		time.Sleep(pollEvery)
	}
}

// giveUp reports whether a half-open master has outlived the alive window.
func (c *conn) giveUp() bool {
	alive := c.a.alive()
	if alive == 0 || !c.halfOpen() {
		return false
	}
	return time.Now().UnixMilli() >= c.w.get().HalfOpenAt+alive.Milliseconds()
}

// monitor reports the first event that ends the session.
func (c *conn) monitor(events chan<- end, relayed bool) {
	var frozenSince time.Time
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for range t.C {
		k := c.w.get()
		if k.Drop > c.drop {
			events <- endDrop
			return
		}
		if c.master != 0 {
			if m, ok := readMaster(c.a.host); !ok || m != c.master {
				if c.halfOpen() {
					events <- endTimeout
				} else {
					events <- endMaster
				}
				return
			}
			if c.giveUp() {
				removeMaster(c.a.host, c.master)
				events <- endTimeout
				return
			}
			continue
		}
		if relayed && c.frozen() {
			if frozenSince.IsZero() {
				frozenSince = time.Now()
			}
			if alive := c.a.alive(); alive > 0 && time.Since(frozenSince) >= alive {
				events <- endTimeout
				return
			}
		} else {
			frozenSince = time.Time{}
		}
	}
}

func signals(events chan<- end) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	go func() {
		<-ch
		events <- endSignal
	}()
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func (c *conn) command() *exec.Cmd {
	k := c.w.get()
	cmd := exec.Command("/bin/sh", "-c", c.a.remote)
	if c.a.remote == "" {
		cmd = exec.Command("/bin/sh")
	}
	env := maps.Clone(k.Env)
	// ssh sends the client's TERM with a pty request; the remote's own
	// TERM never reaches a tty session.
	if c.a.tty && isTerminal(os.Stdin) {
		env["TERM"] = os.Getenv("TERM")
	}
	cmd.Env = envList(env)
	if h := k.Env["HOME"]; h != "" {
		cmd.Dir = h
	}
	return cmd
}

// finish ends the process for ev: the remote's exit status arrives
// delay_ms after it ended; an exit knob overrides it.
func (c *conn) finish(ev end, code int, ended time.Time) int {
	k := c.w.get()
	switch ev {
	case endExit:
		time.Sleep(time.Until(ended.Add(time.Duration(k.DelayMs) * time.Millisecond)))
		if k := c.w.get(); k.Exit != nil {
			return *k.Exit
		}
		return code
	case endDrop:
		if c.master != 0 {
			removeMaster(c.a.host, c.master)
		}
		fmt.Fprintf(os.Stderr, "Connection to %s closed by remote host.\n", c.a.host)
	case endMaster:
		fmt.Fprintf(os.Stderr, "Shared connection to %s closed.\n", c.a.host)
	case endTimeout:
		fmt.Fprintf(os.Stderr, "Timeout, server %s not responding.\n", c.a.host)
	}
	return 255
}

// hold keeps the given files open in a process of their own, so the
// remote side of a half-open connection stays attached after ssh is gone.
func hold(files ...*os.File) {
	cmd := exec.Command("sleep", "600")
	cmd.ExtraFiles = files
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		recordHolder(cmd.Process.Pid)
		cmd.Process.Release()
	}
}

// holdUntilMasterGoes keeps the given files open in a process of their own
// while the master made at born is the host's: its session's command
// hangs up only once the master exits or is lost.
func holdUntilMasterGoes(f *os.File, host string, born int64) {
	cmd := exec.Command("/bin/sh", "-c", `while [ "$(cat "$1" 2>/dev/null)" = "$2" ]; do sleep 0.1; done`, "sh", masterPath(host), strconv.FormatInt(born, 10))
	cmd.ExtraFiles = []*os.File{f}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		recordHolder(cmd.Process.Pid)
		cmd.Process.Release()
	}
}

// runPlain is a session without a tty: the stream, a probe, a command.
func (c *conn) runPlain() int {
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	cmd := c.command()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "fakessh:", err)
		return 255
	}
	inR.Close()
	outW.Close()
	errW.Close()
	events := make(chan end, 4)
	signals(events)
	go c.monitor(events, true)
	var drained sync.WaitGroup
	drained.Add(2)
	go newPipe(c.w, c.frozen).run(inW, os.Stdin, func() { inW.Close() })
	go newPipe(c.w, c.frozen).run(os.Stdout, outR, drained.Done)
	go newPipe(c.w, c.frozen).run(os.Stderr, errR, drained.Done)
	exited := make(chan int, 1)
	go func() {
		cmd.Wait()
		exited <- cmd.ProcessState.ExitCode()
	}()
	select {
	case code := <-exited:
		ended := time.Now()
		waitTimeout(&drained, 2*time.Second)
		return c.finish(endExit, code, ended)
	case ev := <-events:
		if ev == endTimeout || ev == endSignal && c.frozen() {
			// Half-open: the remote side never hears that ssh is gone.
			hold(inW, outR, errR)
			if ev == endSignal {
				return 255
			}
			return c.finish(ev, 0, time.Time{})
		}
		inW.Close()
		if ev == endDrop {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		} else {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
		}
		select {
		case <-exited:
		case <-time.After(time.Second):
		}
		return c.finish(ev, 0, time.Time{})
	}
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// runTTY is ssh -t to a host without a pty: the remote side uses the
// terminal itself, undelayed.
func (c *conn) runTTY() int {
	cmd := c.command()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "fakessh:", err)
		return 255
	}
	events := make(chan end, 4)
	signals(events)
	go c.monitor(events, false)
	exited := make(chan int, 1)
	go func() {
		cmd.Wait()
		exited <- cmd.ProcessState.ExitCode()
	}()
	select {
	case code := <-exited:
		return c.finish(endExit, code, time.Now())
	case ev := <-events:
		sig := syscall.SIGHUP
		if ev == endDrop {
			sig = syscall.SIGKILL
		}
		cmd.Process.Signal(sig)
		select {
		case <-exited:
		case <-time.After(time.Second):
		}
		return c.finish(ev, 0, time.Time{})
	}
}

// runPty is ssh -t to a host with a pty: the command leads a session on a
// pty of its own with the terminal's modes and size; keys and output
// cross the shaped link.
func (c *conn) runPty() int {
	master, slave, err := openPty()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakessh: pty:", err)
		return 255
	}
	copyModes(int(os.Stdin.Fd()), int(slave.Fd()))
	cmd := c.command()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "fakessh:", err)
		return 255
	}
	slave.Close()
	old, _ := makeRaw(int(os.Stdin.Fd()))
	defer restore(int(os.Stdin.Fd()), old)
	events := make(chan end, 4)
	signals(events)
	go c.monitor(events, true)
	go c.resizes(master)
	var drained sync.WaitGroup
	drained.Add(1)
	go newPipe(c.w, c.frozen).run(master, os.Stdin, nil)
	go newPipe(c.w, c.frozen).run(os.Stdout, master, drained.Done)
	exited := make(chan int, 1)
	go func() {
		cmd.Wait()
		exited <- cmd.ProcessState.ExitCode()
	}()
	select {
	case code := <-exited:
		ended := time.Now()
		waitTimeout(&drained, 2*time.Second)
		return c.finish(endExit, code, ended)
	case ev := <-events:
		if ev == endTimeout || ev == endSignal && c.frozen() {
			hold(master)
			if ev == endSignal {
				return 255
			}
			return c.finish(ev, 0, time.Time{})
		}
		if ev == endSignal && c.master != 0 {
			// A session on a master outlives its ssh, as a plain ssh -tt
			// over a master, killed, does: its command still runs, pty
			// and all, until the master goes (decision 112).
			holdUntilMasterGoes(master, c.a.host, c.master)
			return 255
		}
		if ev == endDrop {
			cmd.Process.Kill()
		}
		master.Close() // the remote session's hang-up
		select {
		case <-exited:
		case <-time.After(time.Second):
		}
		return c.finish(ev, 0, time.Time{})
	}
}

// resizes applies the terminal's window size to the host's pty delay_ms
// after each SIGWINCH.
func (c *conn) resizes(master *os.File) {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, syscall.SIGWINCH)
	for range ch {
		rows, cols, err := relay.GetSize(int(os.Stdin.Fd()))
		if err != nil {
			continue
		}
		d := time.Duration(c.w.get().DelayMs) * time.Millisecond
		time.AfterFunc(d, func() { relay.SetSize(int(master.Fd()), rows, cols) })
	}
}
