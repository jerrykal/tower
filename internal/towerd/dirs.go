package towerd

import (
	"os"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/dirs"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/stream"
)

// lookEvery bounds how often dashboards' reads refresh git state and
// zoxide directories: the first read of a dashboard opening refreshes,
// and a dashboard kept open refreshes at most this often.
func lookEvery() time.Duration { return config.Duration("TOWER_LOOK_EVERY", 10*time.Second) }

// newDirs makes the refresher of git state and zoxide directories. The
// binaries are found in its own goroutine, never on towerd's start.
func (d *Daemon) newDirs() *dirs.Refresher {
	home, _ := os.UserHomeDir()
	o := dirs.Options{
		Home:     home,
		Every:    config.Duration("TOWER_DIRS_EVERY", time.Minute),
		OnChange: d.dirsChanged,
		Log:      d.logf,
	}
	if config.Flag("TOWER_GIT", true) {
		o.Git = dirs.FindGit
	}
	if config.Flag("TOWER_DIRS", true) {
		o.Zoxide = dirs.FindZoxide
	}
	if p := os.Getenv("TOWER_TEST_MOUNTS"); p != "" {
		o.Mounts = func() (dirs.Mounts, error) { return dirs.ReadMounts(p) }
	}
	return dirs.New(o)
}

// dirsChanged follows a change in git state or zoxide directories:
// dashboards here wake, and each connected home's state and the home's
// views go out (each only if its body changed, paced as always).
func (d *Daemon) dirsChanged() {
	d.mu.Lock()
	d.bump()
	var recs []*homeRec
	for _, r := range d.homes {
		if r.conn != nil {
			recs = append(recs, r)
		}
	}
	h := d.home
	d.mu.Unlock()
	for _, r := range recs {
		r.pace.Kick()
	}
	if h != nil {
		h.kickViews()
	}
}

// withGit gives each session its directory's git state.
func (d *Daemon) withGit(ss []proto.Session) []proto.Session {
	for i := range ss {
		ss[i].Git = d.dirs.Git(ss[i].Path)
	}
	return ss
}

// look is a dashboard reading, here (from nil) or on another machine (a
// look message from a home, *homeRec, or from a host of this home,
// *link). At most every lookEvery this machine refreshes its git state and
// zoxide directories, and passes the look on: a dashboard's to every home
// connected here and every host of this home; a host's to this home's
// other hosts; a home's to nobody. So a look never goes round.
func (d *Daemon) look(from any) {
	d.mu.Lock()
	if time.Since(d.lookAt) < lookEvery() {
		d.mu.Unlock()
		return
	}
	d.lookAt = time.Now()
	var send []*stream.Conn
	links := func(except *link) {
		if d.home == nil {
			return
		}
		for _, l := range d.home.links {
			if l != except && l.conn != nil && l.status == proto.StatusUp {
				send = append(send, l.conn)
			}
		}
	}
	switch src := from.(type) {
	case nil:
		for _, r := range d.homes {
			if r.conn != nil {
				send = append(send, r.conn)
			}
		}
		links(nil)
	case *link:
		links(src)
	}
	d.mu.Unlock()
	d.dirs.Look()
	for _, c := range send {
		c.Send(&proto.Msg{T: proto.TLook})
	}
}
