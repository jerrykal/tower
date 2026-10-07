// Package proto holds the types every part of tower shares: the model of
// hosts, sessions and windows, the messages of the home ↔ remote stream,
// and the calls served on towerd's unix socket. It is data and pure
// helpers only.
package proto

import "strings"

// Ref names a target: a session (and optionally a window and pane) on one
// tmux server instance of one host.
type Ref struct {
	Host    string `json:"host,omitempty"` // towerd id
	Name    string `json:"name,omitempty"` // host label at the home, for display
	Inst    string `json:"inst,omitempty"` // tmux server instance, "pid:start_time"
	Session string `json:"s,omitempty"`    // "$3"
	Window  string `json:"w,omitempty"`    // "@7"
	Pane    string `json:"p,omitempty"`    // "%1"
	Label   string `json:"label,omitempty"`
}

// IsZero reports whether r names nothing.
func (r Ref) IsZero() bool { return r.Host == "" && r.Session == "" }

// SameSession reports whether a and b name the same session of the same
// server instance.
func (r Ref) SameSession(o Ref) bool {
	return r.Host == o.Host && r.Session == o.Session && r.Inst == o.Inst
}

// String is the form used in logs and notes: name:label (or the ids).
func (r Ref) String() string {
	host := r.Name
	if host == "" {
		host = r.Host
	}
	s := r.Label
	if s == "" {
		s = r.Session
	}
	var b strings.Builder
	b.WriteString(host)
	b.WriteByte(':')
	b.WriteString(s)
	if r.Window != "" {
		b.WriteByte(':')
		b.WriteString(r.Window)
	}
	return b.String()
}

// Session is one tmux session as its own towerd sees it.
type Session struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Path     string   `json:"path,omitempty"`
	Ago      int64    `json:"ago"`             // ms since session_last_attached, on the owner's clock
	Attached int      `json:"att,omitempty"`   // attached clients, towerd's own left out
	Group    string   `json:"group,omitempty"` // session_group when grouped
	Windows  []Window `json:"wins,omitempty"`
	Git      *Git     `json:"git,omitempty"` // the session's directory, when it is in a git repo
}

// Git is what the dashboard shows of a directory in a git repo.
type Git struct {
	Branch string `json:"branch,omitempty"` // the branch, or a short commit when detached
	Dirty  bool   `json:"dirty,omitempty"`
	Repo   string `json:"repo,omitempty"` // a linked worktree: the main worktree's directory name
}

// Dir is a zoxide directory with no session yet.
type Dir struct {
	Path string `json:"path"`           // as the host spells it, ~ for its home
	Root bool   `json:"root,omitempty"` // a git repo's root (a .git file or directory)
	Git  *Git   `json:"git,omitempty"`  // branch and dirt, for a root
	Net  bool   `json:"net,omitempty"`  // on a network mount: listed, never checked
}

// Pane is one pane of a window, for the layout preview and for saying
// what a kill would end.
type Pane struct {
	ID      string `json:"id"`
	Window  string `json:"w"`
	Left    int    `json:"x"`
	Top     int    `json:"y"`
	Width   int    `json:"cols"`
	Height  int    `json:"rows"`
	Command string `json:"cmd,omitempty"`
	Path    string `json:"path,omitempty"` // the pane's directory, ~ for its host's home
	Active  bool   `json:"active,omitempty"`
}

// Window is one tmux window.
type Window struct {
	ID       string `json:"id"`
	Index    int    `json:"idx"`
	Name     string `json:"name"`
	Panes    int    `json:"panes,omitempty"`
	Active   bool   `json:"active,omitempty"`
	Bell     bool   `json:"bell,omitempty"`
	Activity bool   `json:"act,omitempty"`
	Silence  bool   `json:"sil,omitempty"`
}

// Client is a tmux client that tower registered: one attach of a loop.
type Client struct {
	Name    string `json:"name"` // tmux client name
	Pid     int    `json:"pid"`
	Session string `json:"s,omitempty"`
	Window  string `json:"w,omitempty"`
	Loop    string `json:"loop"`
	Gen     int    `json:"gen"`
	Home    string `json:"home"` // towerd id of the loop's home
}

// Host statuses, as the home sees each host.
const (
	StatusLocal      = "local"
	StatusUp         = "up"
	StatusConnecting = "connecting"
	StatusStalled    = "stalled"
	StatusDown       = "down"
	StatusFailed     = "failed"
	StatusDup        = "dup"
	StatusOff        = "off"
	StatusInstalling = "installing" // the home is putting its build there
)

// Host is one host in the home's view.
type Host struct {
	ID       string    `json:"id,omitempty"` // towerd id, once known
	Name     string    `json:"name"`
	Status   string    `json:"status"`
	Reason   string    `json:"reason,omitempty"`
	OS       string    `json:"os,omitempty"`
	Tmux     string    `json:"tmux,omitempty"`
	Version  string    `json:"version,omitempty"`
	MKey     string    `json:"mkey,omitempty"`
	Inst     string    `json:"inst,omitempty"`
	NoServer bool      `json:"nosrv,omitempty"`
	Sessions []Session `json:"sessions,omitempty"`
	Dirs     []Dir     `json:"dirs,omitempty"` // zoxide directories, most frecent first
	Seen     int64     `json:"seen,omitempty"` // ms since last heard, for a host not up
	RTT      int64     `json:"rtt,omitempty"`  // ms, the link's slow recent round trip
	Link     int       `json:"link,omitempty"` // link generation: grows with each connect, on any link to the host (ms)
}

// Reachable reports whether requests and hand-offs to h can go ahead.
func (h *Host) Reachable() bool {
	return h.Status == StatusLocal || h.Status == StatusUp
}

// Loop is one attach loop of a home, as its view carries it.
type Loop struct {
	ID   string `json:"id"`
	Gen  int    `json:"gen"`
	Cur  Ref    `json:"cur"`
	Prev Ref    `json:"prev,omitempty"`
	Seen bool   `json:"seen,omitempty"` // the home has seen this attach's client
}

// View is the home's merged picture of every host.
type View struct {
	Seq   uint64 `json:"seq"`
	Home  string `json:"home"` // towerd id of the home
	Hosts []Host `json:"hosts"`
	Loops []Loop `json:"loops,omitempty"`
	Pad   string `json:"pad,omitempty"` // test padding (TOWER_TEST_PAD)
}

// HostByID returns the host with towerd id id, or nil.
func (v *View) HostByID(id string) *Host {
	if v == nil {
		return nil
	}
	for i := range v.Hosts {
		if v.Hosts[i].ID == id {
			return &v.Hosts[i]
		}
	}
	return nil
}

// LoopByID returns the loop with id id, or nil.
func (v *View) LoopByID(id string) *Loop {
	if v == nil {
		return nil
	}
	for i := range v.Loops {
		if v.Loops[i].ID == id {
			return &v.Loops[i]
		}
	}
	return nil
}

// State is one towerd's picture of its own tmux, as sent to one home.
type State struct {
	Seq      uint64    `json:"seq"`
	Inst     string    `json:"inst,omitempty"`
	NoServer bool      `json:"nosrv,omitempty"`
	Sessions []Session `json:"sessions,omitempty"`
	Dirs     []Dir     `json:"dirs,omitempty"`
	Clients  []Client  `json:"clients,omitempty"` // only the receiving home's loops
	Pad      string    `json:"pad,omitempty"`     // test padding (TOWER_TEST_PAD)
}
