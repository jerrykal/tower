package relay

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// LS08 measures the relay under load against the same command given the
// terminal. The terminal is a pty read by a process of its own, so the
// test process's CPU time is the relay's.

const loadSize = 200 << 20

// cpuTime is this process's user and system time so far.
func cpuTime() time.Duration {
	var ru unix.Rusage
	unix.Getrusage(unix.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

type rate struct {
	bytes   int64
	elapsed time.Duration
	cpu     time.Duration // the relay's, when relayed
}

func (r rate) mbps() float64 { return float64(r.bytes) / r.elapsed.Seconds() / 1e6 }

// pump runs argv, relayed or given the terminal, until a reader process
// has read count bytes from the terminal, and checks nothing more came.
func pump(t *testing.T, argv, env []string, count int64, relayed bool) rate {
	t.Helper()
	tt := newTestTerminal(t, 50, 200)
	rargv, renv := helperArgv("reader", "COUNT="+strconv.FormatInt(count, 10))
	reader := exec.Command(rargv[0], rargv[1:]...)
	reader.Env = renv
	reader.ExtraFiles = []*os.File{tt.pty.Master}
	reader.Stderr = os.Stderr
	if err := reader.Start(); err != nil {
		t.Fatal(err)
	}
	cpu0, start := cpuTime(), time.Now()
	var ch <-chan relayResult
	var direct *exec.Cmd
	if relayed {
		ch = relay(startSession(t, argv, env, cookedModes(t)), tt.Terminal)
	} else {
		direct = startDirect(t, tt.pty, argv, env)
	}
	if err := reader.Wait(); err != nil {
		t.Fatalf("reader: %v", err)
	}
	r := rate{bytes: count, elapsed: time.Since(start), cpu: cpuTime() - cpu0}
	if relayed {
		select {
		case res := <-ch:
			if res.err != nil || res.code != 0 {
				t.Fatalf("relay: %+v", res)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the relay did not end")
		}
	} else if err := direct.Wait(); err != nil {
		t.Fatalf("command: %v", err)
	}
	// Nothing more: a hang-up (the command given the terminal led its
	// session) reads as nothing.
	fds := []unix.PollFd{{Fd: int32(tt.mfd), Events: unix.POLLIN}}
	if n, _ := unix.Poll(fds, 50); n > 0 {
		extra := make([]byte, 256)
		if k, _ := unix.Read(tt.mfd, extra); k > 0 {
			t.Fatalf("more than %d bytes reached the terminal: %q", count, extra[:k])
		}
	}
	return r
}

// compare runs argv given the terminal, then relayed, and checks the relay
// keeps up: 100 MB/s, or 0.9 of the direct rate.
func compare(t *testing.T, name string, argv, env []string, count int64) {
	t.Helper()
	d := pump(t, argv, env, count, false)
	r := pump(t, argv, env, count, true)
	t.Logf("%s, %d bytes: given the terminal %.0f MB/s; relayed %.0f MB/s, the relay %v CPU (%.0f%% of a core)",
		name, count, d.mbps(), r.mbps(), r.cpu.Round(time.Millisecond), 100*r.cpu.Seconds()/r.elapsed.Seconds())
	if r.mbps() < 100 && r.mbps() < 0.9*d.mbps() {
		t.Fatalf("%s relayed at %.0f MB/s, under 100 MB/s and under 0.9 × %.0f MB/s", name, r.mbps(), d.mbps())
	}
}

// writeRepeated writes block to path until size bytes.
func writeRepeated(t *testing.T, path string, block []byte, size int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	for n := 0; n < size; {
		k := min(len(block), size-n)
		w.Write(block[:k])
		n += k
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestLS08Cat(t *testing.T) {
	if testing.Short() {
		t.Skip("200 MiB cats: not with -short")
	}
	dir := t.TempDir()

	var text bytes.Buffer
	for i := 0; text.Len() < 4<<20; i++ {
		text.WriteString(colouredLine(i))
	}
	coloured := filepath.Join(dir, "coloured.log")
	writeRepeated(t, coloured, text.Bytes(), loadSize)
	compare(t, "coloured log", []string{"cat", coloured}, nil, loadSize)

	r := rand.New(rand.NewPCG(8, 8))
	var wide bytes.Buffer
	words := []string{"中文字符", "日本語", "한국어", "😀", "👨‍👩‍👧", "漢字"}
	for wide.Len() < 4<<20 {
		if r.IntN(3) == 0 {
			for range 1 + r.IntN(64) {
				b := byte(r.IntN(256))
				if b == keyByte {
					b++
				}
				wide.WriteByte(b)
			}
		} else {
			wide.WriteString(words[r.IntN(len(words))])
		}
	}
	if bytes.IndexByte(wide.Bytes(), keyByte) >= 0 {
		t.Fatal("the binary holds the key byte")
	}
	binary := filepath.Join(dir, "wide.bin")
	writeRepeated(t, binary, wide.Bytes(), loadSize)
	compare(t, "wide characters and binary", []string{"cat", binary}, nil, loadSize)
}

func TestLS08BuildLog(t *testing.T) {
	const lines = 400000
	var count int64
	for i := 1; i <= lines; i++ {
		count += int64(len(buildLine(i, lines)))
	}
	argv, env := helperArgv("buildlog", "LINES="+strconv.Itoa(lines))
	compare(t, "build log, a write a line", argv, env, count)
}

// floodReader reads a terminal flooded with output, counting what comes
// and signalling each key echoed in it; it can be paused.
type floodReader struct {
	received atomic.Int64
	echoes   chan struct{}
	paused   atomic.Bool
	done     chan struct{}
}

func readFlood(fd int) *floodReader {
	f := &floodReader{echoes: make(chan struct{}, 1024), done: make(chan struct{})}
	go func() {
		defer close(f.done)
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

func TestLS08KeysIntoFlood(t *testing.T) {
	for _, c := range []struct {
		name string
		rate int
	}{{"paced at 40 MB/s", 40e6}, {"unpaced", 0}} {
		t.Run(c.name, func(t *testing.T) {
			keylog := filepath.Join(t.TempDir(), "keys")
			argv, env := helperArgv("flood", "RATE="+strconv.Itoa(c.rate), "KEYLOG="+keylog)
			s := startSession(t, argv, env, cookedModes(t).Raw())
			tt := newTestTerminal(t, 50, 200)
			ch := relay(s, tt.Terminal)
			f := readFlood(tt.mfd)
			if !waitFor(10*time.Second, func() bool { return f.received.Load() >= 8<<20 }) {
				t.Fatalf("only %d bytes of flood", f.received.Load())
			}
			const keys = 200
			sent := make([]int64, keys)
			var echo durations
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
			s.Terminate()
			if res := <-ch; res.err != nil {
				t.Fatal(res.err)
			}
			log, err := os.ReadFile(keylog)
			if err != nil {
				t.Fatal(err)
			}
			arrived := strings.Fields(string(log))
			if len(arrived) != keys {
				t.Fatalf("%d of %d keys reached the host", len(arrived), keys)
			}
			var toHost durations
			for i, a := range arrived {
				ns, _ := strconv.ParseInt(a, 10, 64)
				toHost = append(toHost, time.Duration(ns-sent[i]))
			}
			t.Logf("key to host: median %v p99 %v; echo: median %v p99 %v; %d MB of flood",
				toHost.pct(0.5), toHost.pct(0.99), echo.pct(0.5), echo.pct(0.99), f.received.Load()/1e6)
			if toHost.pct(0.5) >= time.Millisecond {
				t.Fatalf("keys reach the host in %v at the median", toHost.pct(0.5))
			}
		})
	}
}

func TestLS08Backpressure(t *testing.T) {
	progress := filepath.Join(t.TempDir(), "progress")
	written := func() int64 {
		b, err := os.ReadFile(progress)
		if err != nil || len(b) < 8 {
			return 0
		}
		return int64(binary.LittleEndian.Uint64(b))
	}
	argv, env := helperArgv("flood", "RATE=0", "PROGRESS="+progress)
	s := startSession(t, argv, env, cookedModes(t).Raw())
	tt := newTestTerminal(t, 50, 200)

	var mu sync.Mutex
	var heapMin, heapMax uint64 = ^uint64(0), 0
	stop := make(chan struct{})
	sampled := make(chan struct{})
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

	ch := relay(s, tt.Terminal)
	f := readFlood(tt.mfd)
	time.Sleep(time.Second)
	f.paused.Store(true)
	time.Sleep(1500 * time.Millisecond)
	p1, cpu1 := written(), cpuTime()
	time.Sleep(500 * time.Millisecond)
	p2, cpu2 := written(), cpuTime()
	time.Sleep(500 * time.Millisecond)
	f.paused.Store(false)
	p3 := written()
	grew := waitFor(3*time.Second, func() bool { return written()-p3 > 1<<20 })
	p4 := written()
	close(stop)
	<-sampled
	s.Terminate()
	if res := <-ch; res.err != nil {
		t.Fatal(res.err)
	}

	t.Logf("written before the stop %d KiB; stopped: +%d KiB in 0.5s, the relay %v CPU; resumed: +%d KiB; heap %d KiB to %d KiB",
		p1>>10, (p2-p1)>>10, (cpu2 - cpu1).Round(time.Millisecond), (p4-p3)>>10, heapMin>>10, heapMax>>10)
	if p1 < 1<<20 {
		t.Fatalf("only %d bytes before the stop", p1)
	}
	if p2-p1 >= 64<<10 {
		t.Fatalf("the host wrote %d bytes in 0.5s to a terminal that was not reading", p2-p1)
	}
	if !grew {
		t.Fatalf("the host wrote only %d bytes in 3s once the terminal read again", p4-p3)
	}
	if heapMax-heapMin >= 4<<20 {
		t.Fatalf("the heap went from %d to %d bytes", heapMin, heapMax)
	}
}
