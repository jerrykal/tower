package scenario

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jerrykal/tower/internal/relay"
	"golang.org/x/sys/unix"
)

// The suite's binary doubles as the load programs the relay scenarios run
// on a simulated host (over ssh) and beside a terminal: with
// helperVar set it runs that program instead of the tests.
const helperVar = "SCENARIO_HELPER"

func runHelper(name string) int {
	switch name {
	case "flood":
		return helperFlood()
	case "buildlog":
		return helperBuildLog()
	case "reader":
		return helperReader()
	case "sleep":
		// A process that only lives a while (whatever its argv says).
		time.Sleep(30 * time.Second)
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown helper", name)
	return 2
}

// helperCommand is the remote command that runs helper name with env
// (KEY=value words, no quoting needed).
func helperCommand(name string, env ...string) string {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := helperVar + "=" + name
	for _, e := range env {
		cmd += " " + e
	}
	return cmd + " exec " + exe + " -test.run='^$'"
}

func envInt(name string) int {
	n, _ := strconv.Atoi(os.Getenv(name))
	return n
}

// rawStdin makes the helper's terminal raw, as a full-screen program
// (tmux) makes its own: keys arrive one by one, output is not translated.
func rawStdin() {
	if m, err := relay.GetModes(0); err == nil {
		relay.SetModes(0, m.Raw())
	}
}

// keyByte is the key the load scenarios type; their output never holds it.
const keyByte = 0x01

// colouredLine is line i of a coloured log.
func colouredLine(i int) string {
	levels := []string{"\x1b[32mINFO\x1b[0m", "\x1b[33mWARN\x1b[0m", "\x1b[1;31mFAIL\x1b[0m", "\x1b[36mDBUG\x1b[0m"}
	return fmt.Sprintf("\x1b[2m2026-10-04T12:%02d:%02d.%06dZ\x1b[22m %s \x1b[38;5;%dmworker-%d\x1b[39m request %d served in %dms\r\n",
		i/60%60, i%60, i*7919%1000000, levels[i%len(levels)], 16+i%216, i%32, i, i*31%997)
}

// buildLine is line i of n of a build log.
func buildLine(i, n int) string {
	return fmt.Sprintf("[%7d/%7d] \x1b[32mCC\x1b[0m src/module%03d/unit%05d.o\r\n", i, n, i%1000, i%100000)
}

// helperFlood writes coloured text to its terminal for ever, at $RATE
// bytes a second (0: as fast as it can). Each key (keyByte) it reads is
// logged with its arrival time (unix ns) in $KEYLOG and echoed after the
// write in progress, or at once while pacing waits. Every 64 KiB it
// stores the count written so far in $PROGRESS.
func helperFlood() int {
	rawStdin()
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
					if c != keyByte {
						continue
					}
					log.WriteString(strconv.FormatInt(now, 10) + "\n")
					pending.Add(1)
					select {
					case keyed <- struct{}{}:
					default:
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
	echo := func() {
		for ; pending.Load() > 0; pending.Add(-1) {
			os.Stdout.Write([]byte{keyByte})
		}
	}
	rate := envInt("RATE")
	start := time.Now()
	timer := time.NewTimer(0)
	var written, mark int64
	var cnt [8]byte
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

// helperBuildLog writes a build log of $LINES lines, one write a line.
// A write that comes back short (a pty's can on macOS) goes on with the
// rest, as a program's write loop does.
func helperBuildLog() int {
	rawStdin()
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

// helperReader reads the terminal on fd 3 until $COUNT bytes, then exits;
// or fails once nothing has come for 10s: bytes lost on the way (as a
// burst's last ones can be over real ssh on macOS) never come.
func helperReader() int {
	want := int64(envInt("COUNT"))
	buf := make([]byte, 64<<10)
	var got int64
	for got < want {
		fds := []unix.PollFd{{Fd: 3, Events: unix.POLLIN}}
		if n, err := unix.Poll(fds, 10000); n == 0 && err == nil {
			fmt.Fprintf(os.Stderr, "reader: %d of %d: nothing for 10s\n", got, want)
			return 1
		}
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
