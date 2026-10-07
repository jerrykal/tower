package relay

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The test binary doubles as the commands the tests run on ptys: with
// relayHelper set it runs that helper instead of the tests.
const relayHelper = "RELAY_TEST_HELPER"

func TestMain(m *testing.M) {
	if h := os.Getenv(relayHelper); h != "" {
		os.Exit(runHelper(h))
	}
	os.Exit(m.Run())
}

// helperArgv is the command line and environment of helper name.
func helperArgv(name string, env ...string) ([]string, []string) {
	return []string{os.Args[0], "-test.run=^$"}, append(os.Environ(), append([]string{relayHelper + "=" + name}, env...)...)
}

func envInt(name string) int {
	n, _ := strconv.Atoi(os.Getenv(name))
	return n
}

func runHelper(name string) int {
	switch name {
	case "pieces":
		return helperPieces()
	case "flood":
		return helperFlood()
	case "buildlog":
		return helperBuildLog()
	case "reader":
		return helperReader()
	}
	fmt.Fprintln(os.Stderr, "unknown helper", name)
	return 2
}

// spin waits d without sleeping: sleeps this short overshoot by far.
func spin(d time.Duration) {
	for end := time.Now().Add(d); time.Now().Before(end); {
	}
}

// helperPieces writes $FILE to stdout in random pieces of 1–512 bytes,
// $PACE µs apart.
func helperPieces() int {
	data, err := os.ReadFile(os.Getenv("FILE"))
	if err != nil {
		return 1
	}
	r := rand.New(rand.NewPCG(uint64(envInt("SEED")), 7))
	pace := time.Duration(envInt("PACE")) * time.Microsecond
	for len(data) > 0 {
		n := min(1+r.IntN(512), len(data))
		if _, err := os.Stdout.Write(data[:n]); err != nil {
			return 1
		}
		data = data[n:]
		spin(pace)
	}
	return 0
}

// keyByte is the key the load tests type; their output never holds it.
const keyByte = 0x01

// helperFlood writes coloured text to stdout for ever, at $RATE bytes a
// second (0: as fast as it can). Each key (keyByte) read from stdin is
// logged with its arrival time in $KEYLOG and echoed after the write in
// progress (at once while pacing waits). Every 64 KiB it stores the count
// written so far in $PROGRESS.
func helperFlood() int {
	var chunk []byte
	for i := 0; len(chunk) < 16<<10; i++ {
		chunk = append(chunk, colouredLine(i)...)
	}
	chunk = chunk[:16<<10]
	var pending atomic.Int64
	keyed := make(chan struct{}, 1)
	if path := os.Getenv("KEYLOG"); path != "" {
		log, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return 1
		}
		go func() {
			b := make([]byte, 64)
			for {
				n, err := os.Stdin.Read(b)
				now := time.Now().UnixNano()
				for _, c := range b[:n] {
					if c == keyByte {
						log.WriteString(strconv.FormatInt(now, 10) + "\n")
						pending.Add(1)
						select {
						case keyed <- struct{}{}:
						default:
						}
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
	var progress *os.File
	if path := os.Getenv("PROGRESS"); path != "" {
		var err error
		if progress, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600); err != nil {
			return 1
		}
	}
	rate := envInt("RATE")
	start := time.Now()
	var written, mark int64
	var cnt [8]byte
	echo := func() {
		for ; pending.Load() > 0; pending.Add(-1) {
			os.Stdout.Write([]byte{keyByte})
		}
	}
	timer := time.NewTimer(0)
	for {
		if _, err := os.Stdout.Write(chunk); err != nil {
			return 0
		}
		written += int64(len(chunk))
		echo()
		if progress != nil && written-mark >= 64<<10 {
			mark = written
			binary.LittleEndian.PutUint64(cnt[:], uint64(written))
			progress.WriteAt(cnt[:], 0)
		}
		if rate <= 0 {
			continue
		}
		due := start.Add(time.Duration(float64(written) / float64(rate) * float64(time.Second)))
		for d := time.Until(due); d > 0; d = time.Until(due) {
			timer.Reset(d)
			select {
			case <-keyed:
				echo()
			case <-timer.C:
			}
		}
	}
}

// buildLine is line i of n of the build log.
func buildLine(i, n int) string {
	return fmt.Sprintf("[%7d/%7d] \x1b[32mCC\x1b[0m src/module%03d/unit%05d.o\r\n", i, n, i%1000, i%100000)
}

// helperBuildLog writes a build log of $LINES lines, one write a line.
// A write that comes back short (a pty's can on macOS) goes on with the
// rest, as a program's write loop does.
func helperBuildLog() int {
	n := envInt("LINES")
	for i := 1; i <= n; i++ {
		for b := []byte(buildLine(i, n)); len(b) > 0; {
			k, err := unix.Write(1, b)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				return 1
			}
			b = b[k:]
		}
	}
	return 0
}

// helperReader reads the terminal on fd 3 until $COUNT bytes, then exits.
func helperReader() int {
	want := int64(envInt("COUNT"))
	buf := make([]byte, 64<<10)
	var got int64
	for got < want {
		n, err := unix.Read(3, buf)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			fmt.Fprintf(os.Stderr, "reader: %d of %d: %v\n", got, want, err)
			return 1
		}
		got += int64(n)
	}
	return 0
}

// colouredLine is line i of a coloured log: a timestamp, a level in its
// colour, a message.
func colouredLine(i int) string {
	levels := []string{"\x1b[32mINFO\x1b[0m", "\x1b[33mWARN\x1b[0m", "\x1b[1;31mFAIL\x1b[0m", "\x1b[36mDBUG\x1b[0m"}
	return fmt.Sprintf("\x1b[2m2026-10-04T12:%02d:%02d.%06dZ\x1b[22m %s \x1b[38;5;%dmworker-%d\x1b[39m request %d served in %dms\r\n",
		i/60%60, i%60, i*7919%1000000, levels[i%len(levels)], 16+i%216, i%32, i, i*31%997)
}

// cookedModes are the modes of a new pty, as a terminal's are before a
// client makes it raw.
func cookedModes(t testing.TB) Modes {
	t.Helper()
	p := newPty(t)
	m, err := GetModes(fdOf(t, p.Slave))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// testTerminal is the loop's terminal for a test: a pty in raw mode whose
// master the test holds, as the terminal emulator.
type testTerminal struct {
	*Terminal
	pty *Pty
	mfd int
}

func newTestTerminal(t testing.TB, rows, cols int) *testTerminal {
	t.Helper()
	p := newPty(t)
	sfd := fdOf(t, p.Slave)
	m, err := GetModes(sfd)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetModes(sfd, m.Raw()); err != nil {
		t.Fatal(err)
	}
	if err := SetSize(sfd, rows, cols); err != nil {
		t.Fatal(err)
	}
	term, err := NewTerminal(p.Slave, p.Slave)
	if err != nil {
		t.Fatal(err)
	}
	return &testTerminal{Terminal: term, pty: p, mfd: fdOf(t, p.Master)}
}

// startSession starts argv on a pty for a test, and closes it after.
func startSession(t testing.TB, argv, env []string, modes Modes) *Session {
	t.Helper()
	s, err := Start(argv, env, modes, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

type relayResult struct {
	code int
	err  error
}

// relay runs s.Relay(term) in the background.
func relay(s *Session, term *Terminal) <-chan relayResult {
	ch := make(chan relayResult, 1)
	go func() {
		code, err := s.Relay(term)
		ch <- relayResult{code, err}
	}()
	return ch
}

// startDirect runs argv with the pty's slave as its terminal (stdin,
// stdout, stderr, controlling terminal): the command given the terminal,
// as ssh is without the relay.
func startDirect(t testing.TB, p *Pty, argv, env []string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = p.Slave, p.Slave, p.Slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return cmd
}

// durations summarises latency samples.
type durations []time.Duration

func (d durations) pct(p float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*p))]
}
