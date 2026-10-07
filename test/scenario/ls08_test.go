package scenario

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/relay"
	"github.com/jerrykal/tower/internal/transport"
	"golang.org/x/sys/unix"
)

// LS08: the relay under load, through the home's ssh to a host with a
// pty, against that ssh given the terminal itself. The terminal is a pty
// in raw mode whose other side the scenario reads, as a terminal emulator
// would; the scenario's process does the relaying, so its CPU time is the
// relay's.

const ls08Size = 200 << 20

// ls08Term is the terminal of an attach: a raw pty.
type ls08Term struct {
	pty  *relay.Pty
	term *relay.Terminal
	mfd  int
	cook relay.Modes // its modes before raw
}

func newLS08Term(t *testing.T) *ls08Term {
	t.Helper()
	p, err := relay.OpenPty()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	sfd := int(p.Slave.Fd())
	cook, err := relay.GetModes(sfd)
	if err != nil {
		t.Fatal(err)
	}
	relay.SetSize(sfd, 50, 200)
	if err := relay.SetModes(sfd, cook.Raw()); err != nil {
		t.Fatal(err)
	}
	term, err := relay.NewTerminal(p.Slave, p.Slave)
	if err != nil {
		t.Fatal(err)
	}
	return &ls08Term{pty: p, term: term, mfd: int(p.Master.Fd()), cook: cook}
}

// ls08Attach is a remote command run through the home's ssh -t, plain
// (ssh given the terminal: its stdio, session leader with it as the
// controlling terminal) or relayed (on a session of the loop's own,
// relayed as the loop relays it).
type ls08Attach struct {
	plain *exec.Cmd
	sess  *relay.Session
	res   chan error
}

func ls08Run(t *testing.T, w *World, home *Host, tt *ls08Term, remote string, relayed bool) *ls08Attach {
	t.Helper()
	// -q: real ssh's "Connection to … closed." is not the command's. Keys
	// go as tower's attach sends them: not on ssh 9.5+'s 20ms timer.
	bin := home.EnvMap()["TOWER_SSH"]
	argv := []string{bin, "-q", "-t", "-o", "BatchMode=yes"}
	if o := (&transport.SSH{Bin: bin}).Options(config.Host{Name: "B"}); slices.Contains(o, "ObscureKeystrokeTiming=no") {
		argv = append(argv, "-o", "ObscureKeystrokeTiming=no")
	}
	argv = append(argv, "B", "--", remote)
	a := &ls08Attach{res: make(chan error, 1)}
	if !relayed {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = home.Env()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tt.pty.Slave, tt.pty.Slave, tt.pty.Slave
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		a.plain = cmd
		go func() { a.res <- cmd.Wait() }()
		return a
	}
	s, err := relay.Start(argv, home.Env(), tt.cook, 50, 200)
	if err != nil {
		t.Fatal(err)
	}
	a.sess = s
	go func() {
		code, err := s.Relay(tt.term)
		if err == nil && code != 0 {
			err = errExit(code)
		}
		a.res <- err
	}()
	return a
}

type errExit int

func (e errExit) Error() string { return "exit " + strconv.Itoa(int(e)) }

// end ends the attach (SIGTERM to ssh) and waits for it.
func (a *ls08Attach) end(t *testing.T) {
	t.Helper()
	if a.plain != nil {
		a.plain.Process.Signal(syscall.SIGTERM)
	} else {
		a.sess.Terminate()
	}
	select {
	case <-a.res:
	case <-time.After(10 * time.Second):
		t.Fatal("the attach did not end")
	}
	if a.sess != nil {
		a.sess.Close()
	}
}

// wait waits for the attach's command to exit by itself, successfully.
func (a *ls08Attach) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-a.res:
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the attach did not end")
	}
	if a.sess != nil {
		a.sess.Close()
	}
}

func ls08Rusage(who int) time.Duration {
	var ru unix.Rusage
	unix.Getrusage(who, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

type ls08Rate struct {
	elapsed       time.Duration
	relayCPU      time.Duration // this process: the relay
	othersCPU     time.Duration // ssh, the host's command and the reader
	bytes         int64
	mode, command string
}

func (r ls08Rate) mbps() float64 { return float64(r.bytes) / r.elapsed.Seconds() / 1e6 }

// ls08Pump runs remote until a reader process has read count bytes from
// the terminal, and checks that nothing more arrives.
func ls08Pump(t *testing.T, w *World, home *Host, remote string, count int64, relayed bool) ls08Rate {
	t.Helper()
	tt := newLS08Term(t)
	exe, _ := os.Executable()
	reader := exec.Command(exe, "-test.run=^$")
	reader.Env = append(os.Environ(), helperVar+"=reader", "COUNT="+strconv.FormatInt(count, 10))
	reader.ExtraFiles = []*os.File{tt.pty.Master}
	reader.Stderr = os.Stderr
	if err := reader.Start(); err != nil {
		t.Fatal(err)
	}
	self, kids, start := ls08Rusage(unix.RUSAGE_SELF), ls08Rusage(unix.RUSAGE_CHILDREN), time.Now()
	a := ls08Run(t, w, home, tt, remote, relayed)
	if err := reader.Wait(); err != nil {
		a.end(t)
		t.Fatalf("reader: %v", err)
	}
	r := ls08Rate{elapsed: time.Since(start), bytes: count}
	a.wait(t)
	r.relayCPU = ls08Rusage(unix.RUSAGE_SELF) - self
	r.othersCPU = ls08Rusage(unix.RUSAGE_CHILDREN) - kids
	fds := []unix.PollFd{{Fd: int32(tt.mfd), Events: unix.POLLIN}}
	if n, _ := unix.Poll(fds, 100); n > 0 {
		extra := make([]byte, 256)
		if k, _ := unix.Read(tt.mfd, extra); k > 0 {
			t.Fatalf("more than %d bytes reached the terminal: %q", count, extra[:k])
		}
	}
	return r
}

// ls08Report is TT_LS08_REPORT, a file: on a shared runner, which can be
// too slow for LS08's floor (decision 135), each rate is measured three
// times, the best of each compared, and a shortfall written there rather
// than failed. Bytes lost or extra fail all the same.
var ls08Report = os.Getenv("TT_LS08_REPORT")

// ls08Compare runs remote plain, then relayed: the relay keeps up at 100
// MB/s, or 0.9 of the plain rate.
func ls08Compare(t *testing.T, w *World, home *Host, name, remote string, count int64) {
	t.Helper()
	p := ls08Pump(t, w, home, remote, count, false)
	r := ls08Pump(t, w, home, remote, count, true)
	if ls08Report != "" {
		for range 2 {
			if q := ls08Pump(t, w, home, remote, count, false); q.elapsed < p.elapsed {
				p = q
			}
			if q := ls08Pump(t, w, home, remote, count, true); q.elapsed < r.elapsed {
				r = q
			}
		}
	}
	t.Logf("%s, %d bytes: plain %.0f MB/s (ssh, host and reader %v CPU); relayed %.0f MB/s (relay %v CPU, %.0f%% of a core; ssh, host and reader %v)",
		name, count, p.mbps(), p.othersCPU.Round(time.Millisecond), r.mbps(), r.relayCPU.Round(time.Millisecond),
		100*r.relayCPU.Seconds()/r.elapsed.Seconds(), r.othersCPU.Round(time.Millisecond))
	if r.mbps() < 100 && r.mbps() < 0.9*p.mbps() {
		msg := fmt.Sprintf("%s relayed at %.0f MB/s: under 100 MB/s and under 0.9 × plain %.0f MB/s", name, r.mbps(), p.mbps())
		if ls08Report == "" {
			t.Error(msg)
			return
		}
		t.Log("reported, not failed: " + msg)
		f, err := os.OpenFile(ls08Report, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		fmt.Fprintf(f, "%s (best of 3)\n", msg)
	}
}

func ls08File(t *testing.T, path string, block []byte, size int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	for n := 0; n < size; {
		k := min(len(block), size-n)
		bw.Write(block[:k])
		n += k
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

type ls08Durations []time.Duration

func (d ls08Durations) pct(p float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*p))]
}

// ls08Reader reads the terminal of a flooded attach, counting what comes
// and signalling each key echoed in it; it can be paused.
type ls08Reader struct {
	received atomic.Int64
	echoes   chan struct{}
	paused   atomic.Bool
}

func ls08Read(fd int) *ls08Reader {
	f := &ls08Reader{echoes: make(chan struct{}, 1024)}
	go func() {
		buf := make([]byte, 64<<10)
		for {
			for f.paused.Load() {
				time.Sleep(5 * time.Millisecond)
			}
			n, err := unix.Read(fd, buf)
			if err == unix.EINTR {
				continue
			}
			if err != nil || n == 0 {
				return
			}
			f.received.Add(int64(n))
			for k := bytes.Count(buf[:n], []byte{keyByte}); k > 0; k-- {
				f.echoes <- struct{}{}
			}
		}
	}()
	return f
}

// ls08Keys types 200 keys into a flood at rate (0: unpaced), each 5ms
// after the previous one's echo, once 8 MiB have arrived; all must reach
// the host and come back. It returns key-to-host and echo times.
func ls08Keys(t *testing.T, w *World, home *Host, rate int, relayed bool) (toHost, echo ls08Durations) {
	t.Helper()
	keylog := filepath.Join(w.Dir, "keys-"+strconv.Itoa(rate)+"-"+strconv.FormatBool(relayed))
	tt := newLS08Term(t)
	a := ls08Run(t, w, home, tt, helperCommand("flood", "RATE="+strconv.Itoa(rate), "KEYLOG="+keylog), relayed)
	defer a.end(t)
	f := ls08Read(tt.mfd)
	w.Eventually(20*time.Second, "8 MiB of flood", func() bool { return f.received.Load() >= 8<<20 })
	const keys = 200
	sent := make([]int64, keys)
	for i := range keys {
		time.Sleep(5 * time.Millisecond)
		now := time.Now()
		sent[i] = now.UnixNano()
		if _, err := unix.Write(tt.mfd, []byte{keyByte}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-f.echoes:
			echo = append(echo, time.Since(now))
		case <-time.After(10 * time.Second):
			t.Fatalf("key %d: no echo", i)
		}
	}
	b, err := os.ReadFile(keylog)
	if err != nil {
		t.Fatal(err)
	}
	arrived := strings.Fields(string(b))
	if len(arrived) != keys {
		t.Fatalf("%d of %d keys reached the host", len(arrived), keys)
	}
	for i, s := range arrived {
		ns, _ := strconv.ParseInt(s, 10, 64)
		toHost = append(toHost, time.Duration(ns-sent[i]))
	}
	return toHost, echo
}

func TestLS08(t *testing.T) {
	w := NewWorld(t, "ls08")
	home := w.Host("A", nil)
	w.Host("B", nil, SSHHost())
	w.Shape("B", func(l *Link) { l.Pty = true })

	t.Run("cat", func(t *testing.T) {
		if testing.Short() {
			t.Skip("200 MiB cats: not with -short")
		}
		var text bytes.Buffer
		for i := 0; text.Len() < 4<<20; i++ {
			text.WriteString(colouredLine(i))
		}
		coloured := filepath.Join(w.Dir, "coloured.log")
		ls08File(t, coloured, text.Bytes(), ls08Size)
		ls08Compare(t, w, home, "coloured log", "stty raw -echo -opost; exec cat "+coloured, ls08Size)
		os.Remove(coloured)

		r := rand.New(rand.NewPCG(8, 8))
		var wide bytes.Buffer
		words := []string{"中文字符", "日本語", "한국어", "😀", "👨‍👩‍👧", "漢字"}
		for wide.Len() < 4<<20 {
			if r.IntN(3) == 0 {
				for range 1 + r.IntN(64) {
					c := byte(r.IntN(256))
					if c == keyByte {
						c++
					}
					wide.WriteByte(c)
				}
			} else {
				wide.WriteString(words[r.IntN(len(words))])
			}
		}
		bin := filepath.Join(w.Dir, "wide.bin")
		ls08File(t, bin, wide.Bytes(), ls08Size)
		ls08Compare(t, w, home, "wide characters and binary", "stty raw -echo -opost; exec cat "+bin, ls08Size)
		os.Remove(bin)
	})

	t.Run("build log", func(t *testing.T) {
		const lines = 400000
		var count int64
		for i := 1; i <= lines; i++ {
			count += int64(len(buildLine(i, lines)))
		}
		ls08Compare(t, w, home, "build log, a write a line", helperCommand("buildlog", "LINES="+strconv.Itoa(lines)), count)
	})

	for _, c := range []struct {
		name string
		rate int
	}{{"keys at 40 MB/s", 40e6}, {"keys unpaced", 0}} {
		t.Run(c.name, func(t *testing.T) {
			ph, pe := ls08Keys(t, w, home, c.rate, false)
			rh, re := ls08Keys(t, w, home, c.rate, true)
			t.Logf("key to host: plain median %v p99 %v, relayed median %v p99 %v", ph.pct(0.5), ph.pct(0.99), rh.pct(0.5), rh.pct(0.99))
			t.Logf("echo: plain median %v p99 %v, relayed median %v p99 %v", pe.pct(0.5), pe.pct(0.99), re.pct(0.5), re.pct(0.99))
			if rh.pct(0.5) >= time.Millisecond {
				t.Errorf("relayed keys reach the host in %v at the median", rh.pct(0.5))
			}
			if c.rate > 0 && re.pct(0.5)-pe.pct(0.5) >= time.Millisecond {
				t.Errorf("the relay adds %v to the median echo at 40 MB/s", re.pct(0.5)-pe.pct(0.5))
			}
		})
	}

	t.Run("backpressure", func(t *testing.T) {
		progress := filepath.Join(w.Dir, "progress")
		written := func() int64 {
			b, err := os.ReadFile(progress)
			if err != nil || len(b) < 8 {
				return 0
			}
			return int64(binary.LittleEndian.Uint64(b))
		}
		tt := newLS08Term(t)
		var mu sync.Mutex
		var heapMin, heapMax uint64 = ^uint64(0), 0
		stop, sampled := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(sampled)
			var ms runtime.MemStats
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
				}
				runtime.ReadMemStats(&ms)
				mu.Lock()
				heapMin, heapMax = min(heapMin, ms.HeapAlloc), max(heapMax, ms.HeapAlloc)
				mu.Unlock()
			}
		}()
		a := ls08Run(t, w, home, tt, helperCommand("flood", "RATE=0", "PROGRESS="+progress), true)
		f := ls08Read(tt.mfd)
		time.Sleep(time.Second)
		f.paused.Store(true)
		time.Sleep(1500 * time.Millisecond)
		p1, cpu1 := written(), ls08Rusage(unix.RUSAGE_SELF)
		time.Sleep(500 * time.Millisecond)
		p2, cpu2 := written(), ls08Rusage(unix.RUSAGE_SELF)
		time.Sleep(500 * time.Millisecond)
		f.paused.Store(false)
		p3 := written()
		grew := false
		for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if grew = written()-p3 > 1<<20; grew {
				break
			}
		}
		p4 := written()
		close(stop)
		<-sampled
		a.end(t)
		t.Logf("written before the stop %d KiB; stopped: +%d KiB in 0.5s, the relay %v CPU; resumed: +%d KiB; heap %d KiB to %d KiB",
			p1>>10, (p2-p1)>>10, (cpu2 - cpu1).Round(time.Millisecond), (p4-p3)>>10, heapMin>>10, heapMax>>10)
		if p1 < 1<<20 {
			t.Fatalf("only %d bytes before the stop", p1)
		}
		if p2-p1 >= 64<<10 {
			t.Errorf("the host wrote %d bytes in 0.5s to a terminal that was not reading", p2-p1)
		}
		if !grew {
			t.Errorf("the host wrote only %d bytes in 3s once the terminal read again", p4-p3)
		}
		if heapMax-heapMin >= 4<<20 {
			t.Errorf("the heap went from %d to %d bytes", heapMin, heapMax)
		}
	})
}
