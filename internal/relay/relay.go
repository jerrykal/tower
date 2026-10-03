package relay

import (
	"time"

	"golang.org/x/sys/unix"
)

// maxDrain bounds what the relay reads from one source after the command
// has exited (a process it left behind may still be writing).
const maxDrain = 4 << 20

// input copies the terminal's input to the pty. It reads only once poll
// says a byte is there, and stops without reading when quit, the
// session's stop or its exit is signalled, or the terminal hangs up.
func (s *Session) input(t *Terminal, quit int) {
	buf := make([]byte, bufSize)
	fds := []unix.PollFd{
		{Fd: int32(t.in), Events: unix.POLLIN},
		{Fd: int32(quit), Events: unix.POLLIN},
		{Fd: int32(s.stopR), Events: unix.POLLIN},
		{Fd: int32(s.exitR), Events: unix.POLLIN},
	}
	for {
		for i := range fds {
			fds[i].Revents = 0
		}
		if _, err := poll(fds, -1); err != nil {
			return
		}
		if fds[1].Revents|fds[2].Revents|fds[3].Revents != 0 {
			return
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			return // the terminal hung up
		}
		n, err := readFd(t.in, buf)
		if err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return
		}
		if writeAll(s.master, buf[:n], quit, s.stopR, s.exitR) != nil {
			return
		}
	}
}

// output copies the stdout pipe's and the pty's output to the terminal
// until the command exits, putting the loop's queued writes in at
// boundaries.
func (s *Session) output(t *Terminal, wakeR int) error {
	buf := make([]byte, bufSize)
	if p := s.pending; len(p) > 0 {
		s.pending = nil
		if err := t.emit(p); err != nil {
			return err
		}
	}
	fds := []unix.PollFd{
		{Fd: int32(s.out), Events: unix.POLLIN},
		{Fd: int32(s.master), Events: unix.POLLIN},
		{Fd: int32(wakeR), Events: unix.POLLIN},
		{Fd: int32(s.exitR), Events: unix.POLLIN},
		{Fd: int32(s.quitR), Events: unix.POLLIN},
	}
	for {
		// Queued writes go now if they can; else poll until they must.
		if err := t.emit(nil); err != nil {
			return err
		}
		wait := time.Duration(-1)
		if t.queued.Load() {
			t.behind = true
			wait = max(time.Until(t.deadline()), 0)
		}
		for i := range fds {
			fds[i].Revents = 0
		}
		if _, err := poll(fds, wait); err != nil {
			return err
		}
		if fds[4].Revents != 0 {
			return ErrAbandoned
		}
		if fds[2].Revents != 0 {
			for {
				if _, err := readFd(wakeR, buf); err != nil {
					break
				}
			}
		}
		for i := 0; i < 2; i++ {
			if fds[i].Revents == 0 {
				continue
			}
			n, err := readFd(int(fds[i].Fd), buf)
			if n > 0 {
				s.relayed.Add(int64(n))
				if err := t.emit(buf[:n]); err != nil {
					return err
				}
			} else if err != unix.EAGAIN {
				fds[i].Fd = -1 // ended: poll skips it from now on
			}
		}
		if fds[3].Revents != 0 {
			// Everything the command wrote is in the pipe and the pty now.
			for _, fd := range []int{s.out, s.master} {
				for drained := 0; drained < maxDrain; {
					n, _ := readFd(fd, buf)
					if n == 0 {
						break
					}
					if err := t.emit(buf[:n]); err != nil {
						return err
					}
					drained += n
				}
			}
			return nil
		}
	}
}
