package dirs

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Mount is one entry of a mount table: where a filesystem is mounted and
// its type as the kernel names it (nfs4, cifs, fuse.sshfs, smbfs …).
type Mount struct {
	Dir  string
	Type string
}

// Mounts is a mount table, deepest mount point first, so the first entry
// that contains a path is the filesystem the path is on.
type Mounts []Mount

// netTypes are the filesystems whose stat may block until a server
// answers (a hard NFS mount blocks for ever), or that cost a round trip
// per stat: paths on them are listed, never checked.
var netTypes = map[string]bool{
	// Linux
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true, "ncpfs": true,
	"afs": true, "9p": true, "ceph": true, "glusterfs": true, "lustre": true, "gpfs": true,
	"beegfs": true, "davfs": true, "autofs": true, "fuse": true,
	// macOS
	"afpfs": true, "webdav": true, "macfuse": true, "osxfuse": true, "osxfusefs": true, "fusefs": true,
}

// localFuse are FUSE filesystems known to be local. Every other
// fuse.<name> (sshfs, rclone, s3fs, gvfsd-fuse …) counts as a network
// mount: guessing wrong only costs a missing branch.
var localFuse = map[string]bool{
	"lxcfs": true, "snapfuse": true, "squashfuse": true, "portal": true, "mergerfs": true,
	"bindfs": true, "encfs": true, "gocryptfs": true, "ntfs-3g": true, "appimaged": true,
}

// IsNetType reports whether a filesystem type is a network one.
func IsNetType(t string) bool {
	if netTypes[t] {
		return true
	}
	if sub, ok := strings.CutPrefix(t, "fuse."); ok {
		return !localFuse[sub]
	}
	return false
}

// On returns the mount that holds path (absolute and clean), and whether
// one does.
func (m Mounts) On(path string) (Mount, bool) {
	for _, e := range m {
		if within(path, e.Dir) {
			return e, true
		}
	}
	return Mount{}, false
}

// Net reports whether path is on a network mount. Paths are compared as
// written: a symlink into a network mount is not seen through, since
// resolving it would be the very stat that blocks.
func (m Mounts) Net(path string) bool {
	e, ok := m.On(path)
	return ok && IsNetType(e.Type)
}

func within(path, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == dir || strings.HasPrefix(path, dir) && path[len(dir)] == '/'
}

// sortMounts puts deeper mount points first; a later entry for the same
// point (a mount over another) comes before an earlier one.
func sortMounts(m Mounts) Mounts {
	out := make(Mounts, 0, len(m))
	for i := len(m) - 1; i >= 0; i-- {
		out = append(out, m[i])
	}
	slices.SortStableFunc(out, func(a, b Mount) int { return len(b.Dir) - len(a.Dir) })
	return out
}

// ParseMounts reads a mount table in the format of /proc/self/mounts
// (fstab's: device, mount point, type, options …), octal escapes in the
// mount point included.
func ParseMounts(r io.Reader) (Mounts, error) {
	var m Mounts
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || strings.HasPrefix(f[0], "#") {
			continue
		}
		m = append(m, Mount{Dir: filepath.Clean(unescapeMount(f[1])), Type: f[2]})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return sortMounts(m), nil
}

// unescapeMount undoes the kernel's \ooo escapes (\040 a space, \011 a
// tab, \012 a newline, \134 a backslash).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ReadMounts reads a mount table file in /proc/self/mounts format.
func ReadMounts(path string) (Mounts, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseMounts(f)
}
