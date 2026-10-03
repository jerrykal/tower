package relay

import (
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The relay works on raw file descriptors with poll(2) rather than through
// os.File and the runtime's poller: it must know a byte is there before it
// reads one, it waits on several descriptors and a deadline at once, and
// ptys are not pollable through kqueue on every macOS release.

// poll waits for the events asked in fds, retrying on EINTR. timeout < 0
// waits for ever. A negative fd in fds is ignored, as poll(2) does.
func poll(fds []unix.PollFd, timeout time.Duration) (int, error) {
	ms := -1
	var end time.Time
	if timeout >= 0 {
		ms = int((timeout + time.Millisecond - 1) / time.Millisecond)
		end = time.Now().Add(timeout)
	}
	for {
		n, err := unix.Poll(fds, ms)
		if err != unix.EINTR {
			return n, err
		}
		if timeout >= 0 {
			left := time.Until(end)
			if left < 0 {
				left = 0
			}
			ms = int((left + time.Millisecond - 1) / time.Millisecond)
		}
	}
}

// readFd reads once from fd, retrying on EINTR. It returns errEOF at end
// of file, and unix.EAGAIN when a nonblocking fd has nothing.
func readFd(fd int, p []byte) (int, error) {
	for {
		n, err := unix.Read(fd, p)
		switch {
		case err == unix.EINTR:
			continue
		case err != nil:
			return 0, err
		case n == 0:
			return 0, errEOF
		}
		return n, nil
	}
}

// errEOF is the end of a descriptor's input: end of file, or a pty master
// whose slaves are all closed (EIO).
var errEOF = errors.New("relay: end of input")

// isEOF reports whether err from readFd means the input has ended for good.
func isEOF(err error) bool {
	return err == errEOF || err == unix.EIO
}

// writeAll writes all of p to fd, waiting for room on a nonblocking fd.
// While it waits, any of stops that becomes readable or hangs up ends the
// wait: writeAll then returns errStopped with the rest unwritten.
func writeAll(fd int, p []byte, stops ...int) error {
	for len(p) > 0 {
		n, err := unix.Write(fd, p)
		if n > 0 {
			p = p[n:]
		}
		switch err {
		case nil, unix.EINTR:
		case unix.EAGAIN:
			fds := make([]unix.PollFd, 1+len(stops))
			fds[0] = unix.PollFd{Fd: int32(fd), Events: unix.POLLOUT}
			for i, s := range stops {
				fds[1+i] = unix.PollFd{Fd: int32(s), Events: unix.POLLIN}
			}
			if _, err := poll(fds, -1); err != nil {
				return err
			}
			for _, f := range fds[1:] {
				if f.Revents != 0 {
					return errStopped
				}
			}
		default:
			return err
		}
	}
	return nil
}

// errStopped is a write cut short by its stop descriptor.
var errStopped = errors.New("relay: stopped")

// pipe makes a pipe whose ends are closed on exec. The read end is
// nonblocking.
func pipe() (r, w int, err error) {
	var p [2]int
	syscall.ForkLock.RLock()
	err = unix.Pipe(p[:])
	if err == nil {
		unix.CloseOnExec(p[0])
		unix.CloseOnExec(p[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return -1, -1, os.NewSyscallError("pipe", err)
	}
	if err := unix.SetNonblock(p[0], true); err != nil {
		unix.Close(p[0])
		unix.Close(p[1])
		return -1, -1, os.NewSyscallError("fcntl", err)
	}
	return p[0], p[1], nil
}

// fileFd returns the descriptor under f without changing its blocking
// mode (os.File.Fd would put a nonblocking file in blocking mode).
func fileFd(f *os.File) (int, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd := -1
	if err := rc.Control(func(u uintptr) { fd = int(u) }); err != nil {
		return -1, err
	}
	return fd, nil
}
