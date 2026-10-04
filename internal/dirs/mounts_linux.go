package dirs

// SystemMounts is this machine's mount table: /proc/self/mounts, which
// the kernel writes from memory, so reading it never waits on a server.
func SystemMounts() (Mounts, error) { return ReadMounts("/proc/self/mounts") }
