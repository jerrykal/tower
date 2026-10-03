package relay

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// newPty opens a pty for a test and closes it after.
func newPty(t testing.TB) *Pty {
	t.Helper()
	p, err := OpenPty()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func fdOf(t testing.TB, f *os.File) int {
	t.Helper()
	fd, err := fileFd(f)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func TestOpenPty(t *testing.T) {
	p := newPty(t)
	if _, err := os.Stat(p.Name); err != nil {
		t.Fatalf("slave %q: %v", p.Name, err)
	}
	sfd := fdOf(t, p.Slave)
	m, err := GetModes(sfd)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetModes(sfd, m.Raw()); err != nil {
		t.Fatal(err)
	}
	// Both ways, byte for byte, once raw.
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	go p.Master.Write(all)
	got := make([]byte, 256)
	if _, err := io.ReadFull(p.Slave, got); err != nil || !bytes.Equal(got, all) {
		t.Fatalf("master → slave: %v %q", err, got)
	}
	go p.Slave.Write(all)
	if _, err := io.ReadFull(p.Master, got); err != nil || !bytes.Equal(got, all) {
		t.Fatalf("slave → master: %v %q", err, got)
	}
}

func TestModes(t *testing.T) {
	p := newPty(t)
	fd := fdOf(t, p.Slave)
	cooked, err := GetModes(fd)
	if err != nil {
		t.Fatal(err)
	}
	if cooked.t.Lflag&unix.ICANON == 0 || cooked.t.Lflag&unix.ECHO == 0 {
		t.Fatalf("a new pty is not cooked: lflag %#x", cooked.t.Lflag)
	}
	raw := cooked.Raw()
	if raw.t.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG|unix.IEXTEN) != 0 ||
		raw.t.Iflag&(unix.ICRNL|unix.IXON|unix.ISTRIP) != 0 ||
		raw.t.Oflag&unix.OPOST != 0 || raw.t.Cc[unix.VMIN] != 1 || raw.t.Cc[unix.VTIME] != 0 {
		t.Fatalf("not raw: %+v", raw.t)
	}
	if raw.t.Cflag != cooked.t.Cflag {
		t.Fatalf("raw changed the character size or parity")
	}
	if err := SetModes(fd, raw); err != nil {
		t.Fatal(err)
	}
	back, err := GetModes(fd)
	if err != nil {
		t.Fatal(err)
	}
	if back.t.Lflag&^stateBits != raw.t.Lflag&^stateBits || back.t.Iflag != raw.t.Iflag || back.t.Oflag != raw.t.Oflag {
		t.Fatalf("set %+v, read back %+v", raw.t, back.t)
	}

	if !cooked.Same(raw) || !raw.Same(cooked) {
		t.Fatal("raw mode makes different modes")
	}
	pending := cooked
	pending.t.Lflag |= unix.PENDIN | unix.FLUSHO
	if !cooked.Same(pending) {
		t.Fatal("kernel state bits make different modes")
	}
	erase := cooked
	erase.t.Cc[unix.VERASE] = 'h' & 0x1f
	if cooked.Same(erase) {
		t.Fatal("another erase character is the same modes")
	}
	noctl := cooked
	noctl.t.Lflag ^= unix.ECHOCTL
	if cooked.Same(noctl) {
		t.Fatal("echoctl changed is the same modes")
	}
	speed := cooked
	speed.t.Ospeed = 1200
	if cooked.Same(speed) {
		t.Fatal("another speed is the same modes")
	}
}

func TestSize(t *testing.T) {
	p := newPty(t)
	mfd, sfd := fdOf(t, p.Master), fdOf(t, p.Slave)
	if err := SetSize(mfd, 37, 121); err != nil {
		t.Fatal(err)
	}
	rows, cols, err := GetSize(sfd)
	if err != nil || rows != 37 || cols != 121 {
		t.Fatalf("size %dx%d %v, want 37x121", rows, cols, err)
	}
}

// waitFor polls cond every millisecond until it holds or d passes.
func waitFor(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for !cond() {
		if time.Now().After(end) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}
