package dirs

import "golang.org/x/sys/unix"

// SystemMounts is this machine's mount table, as `mount` prints it:
// getfsstat with MNT_NOWAIT, which answers from the kernel's cached
// statistics instead of asking each filesystem (and so never waits on an
// unresponsive server), with no process to start.
func SystemMounts() (Mounts, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	buf := make([]unix.Statfs_t, n+8)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	m := make(Mounts, 0, n)
	for _, s := range buf[:n] {
		m = append(m, Mount{Dir: unix.ByteSliceToString(s.Mntonname[:]), Type: unix.ByteSliceToString(s.Fstypename[:])})
	}
	return sortMounts(m), nil
}
