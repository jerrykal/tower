package proto

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Call is one request on towerd's unix socket: a JSON line, answered by
// one Reply line. Version is the caller's tower version.
type Call struct {
	Op      string          `json:"op"`
	Version string          `json:"v"`
	Args    json.RawMessage `json:"a,omitempty"`
}

// Reply answers a Call.
type Reply struct {
	Err    string          `json:"err,omitempty"`
	Result json.RawMessage `json:"r,omitempty"`
}

// Local call ops.
const (
	CallStatus     = "status"
	CallStop       = "stop"
	CallStream     = "stream"
	CallRegister   = "register"
	CallView       = "view"
	CallWatch      = "watch"
	CallAct        = "act"
	CallLoop       = "loop"
	CallLoopBye    = "loop-bye"
	CallPrepare    = "prepare"
	CallAfter      = "after"
	CallWaitSwitch = "wait-switch"
	CallHeld       = "held"
	CallStandby    = "standby"
	CallLast       = "last"
	CallWake       = "wake"
	CallNetChange  = "netchange"
	CallReload     = "reload"
	CallRetry      = "retry" // RetryArgs: connect to one host now
	// CallPlant stores a switch as if a dashboard had asked for it; only
	// with TOWER_TEST_HOOKS set (the scenario suite).
	CallPlant = "plant"
)

// StatusArgs asks for towerd's status; Full adds links, homes, loops and
// clients.
type StatusArgs struct {
	Full bool `json:"full,omitempty"`
}

// Status is towerd's answer to status.
type Status struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Pid     int    `json:"pid"`
	MKey    string `json:"mkey"`
	Tag     string `json:"tag"`
	TmuxBin string `json:"tmux_bin"`
	Home    bool   `json:"home,omitempty"` // plays the home role
	// Full status only.
	Detail *Detail `json:"detail,omitempty"`
}

// Detail is the full status: what towerd knows, for `tower status` and
// the scenario suite.
type Detail struct {
	Links   []LinkStatus    `json:"links,omitempty"`   // home role: one per host
	Homes   []HomeStatus    `json:"homes,omitempty"`   // remote role: one per connected home
	Clients []Client        `json:"clients,omitempty"` // registrations
	Loops   []LoopStatus    `json:"loops,omitempty"`   // home role
	Pending []PendingSwitch `json:"pending,omitempty"` // home role: stored switches
	Watch   WatchStatus     `json:"watch"`
	Dirs    DirsStatus      `json:"dirs"`
	CPUMs   int64           `json:"cpu_ms"` // towerd's CPU time so far, its finished children (git, zoxide, tmux) included
}

// DirsStatus is the refresher of git state and zoxide directories.
type DirsStatus struct {
	Git      string `json:"git,omitempty"`    // the git binary; empty: no git state
	Zoxide   string `json:"zoxide,omitempty"` // the zoxide binary; empty: no directories
	Dirs     int    `json:"dirs"`             // zoxide directories kept
	Repos    int    `json:"repos"`            // repos followed
	Rounds   int    `json:"rounds"`           // refreshes so far
	TimerMs  int64  `json:"timer_ms"`         // the last periodic refresh
	TimerRun int    `json:"timer_runs"`       // and its git status runs
	LookMs   int64  `json:"look_ms"`          // the last refresh a dashboard asked for
	LookRun  int    `json:"look_runs"`
}

// LinkStatus is one host link at the home.
type LinkStatus struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	ID       string `json:"id,omitempty"`
	MKey     string `json:"mkey,omitempty"`
	Inst     string `json:"inst,omitempty"`
	Version  string `json:"version,omitempty"`
	OS       string `json:"os,omitempty"`
	Proto    int    `json:"proto,omitempty"`
	Attempts int    `json:"attempts"` // connects tried so far
	Link     int    `json:"link"`     // connects that came up
	States   int    `json:"states"`   // state messages received
	RTT      int64  `json:"rtt_ms"`
	Offset   int64  `json:"offset_ms"`
	Stalled  bool   `json:"stalled,omitempty"`
	Rx       int64  `json:"rx"`
	Tx       int64  `json:"tx"`
	Sessions int    `json:"sessions"`
	Warn     string `json:"warn,omitempty"`
}

// HomeStatus is one home connected to this towerd.
type HomeStatus struct {
	ID   string `json:"id"`
	As   string `json:"as"`
	Name string `json:"name,omitempty"` // the home's own label
	Live bool   `json:"live"`
	Age  int64  `json:"age_ms,omitempty"` // since it disconnected
}

// LoopStatus is one loop at its home.
type LoopStatus struct {
	ID   string `json:"id"`
	Gen  int    `json:"gen"`
	Cur  Ref    `json:"cur"`
	Prev Ref    `json:"prev,omitempty"`
	Host string `json:"host,omitempty"`
	Seen bool   `json:"seen,omitempty"` // the home has seen this attach's client
}

// PendingSwitch is a stored switch waiting for its loop.
type PendingSwitch struct {
	Loop   string `json:"loop"`
	Gen    int    `json:"gen"`
	Target Ref    `json:"target"`
	Nonce  string `json:"nonce,omitempty"`
	Age    int64  `json:"age_ms"`
}

// WatchStatus is towerd's watch on its own tmux.
type WatchStatus struct {
	Inst     string `json:"inst,omitempty"`
	NoServer bool   `json:"nosrv,omitempty"`
	Sessions int    `json:"sessions"`
	CtlPid   int    `json:"ctl_pid,omitempty"`
	CtlName  string `json:"ctl_name,omitempty"`
	CtlBytes int64  `json:"ctl_bytes"` // bytes read from the control client
	Keys     string `json:"keys,omitempty"`
}

// StopArgs asks towerd to exit; with IfOlderThan set, only if a tower of
// that version replaces it (Replaces: older, or another build of the
// same base).
type StopArgs struct {
	IfOlderThan string `json:"if_older_than,omitempty"`
}

// RegisterArgs is the attach shim's registration of the tmux client it is
// about to become.
type RegisterArgs struct {
	Pid  int    `json:"pid"`
	Loop string `json:"loop"`
	Gen  int    `json:"gen"`
	Home string `json:"home"`
	Inst string `json:"inst"`
}

// Registered answers register.
type Registered struct {
	MKey    string `json:"mkey"`
	TmuxBin string `json:"tmux_bin"`
}

// ViewArgs asks for what a dashboard shows. Client is TOWER_CLIENT of the
// dashboard's client, or empty for the loop's own picker (Loop set).
type ViewArgs struct {
	Client string `json:"client,omitempty"`
	Loop   string `json:"loop,omitempty"`
	// Look: a dashboard is open and reading. Its towerd refreshes git
	// state and zoxide dirs (at most every 10s) and tells the other hosts
	// to. The attach loop's own reads leave it unset: they follow every
	// tmux change and must not cost a refresh.
	Look bool `json:"look,omitempty"`
}

// Dash is what a dashboard shows: the view to draw, where it runs, and the
// loop (if any) that owns its client.
type Dash struct {
	Gen   uint64 `json:"gen"`
	Self  string `json:"self"` // towerd id of the machine the dashboard runs on
	View  View   `json:"view"`
	Loop  string `json:"loop,omitempty"`
	Home  string `json:"home,omitempty"` // towerd id of that loop's home
	Note  string `json:"note,omitempty"` // e.g. "home A not connected (2m)"
	Owned bool   `json:"owned,omitempty"`
}

// WatchArgs waits until the generation differs from Gen.
type WatchArgs struct {
	Gen uint64 `json:"gen"`
}

// WatchResult carries the current generation.
type WatchResult struct {
	Gen uint64 `json:"gen"`
}

// LoopBeat is the loop's heartbeat, also its first call: it activates the
// home role and lets a restarted home relearn the loop.
type LoopBeat struct {
	ID   string `json:"id"`
	Gen  int    `json:"gen"`
	Cur  Ref    `json:"cur"`
	Prev Ref    `json:"prev,omitempty"`
}

// LoopAck answers a beat: the last target the home remembers for a new
// loop (from last.json).
type LoopAck struct {
	Last Ref `json:"last,omitempty"`
}

// LoopArgs names a loop (loop-bye, standby).
type LoopArgs struct {
	ID string `json:"id"`
}

// PrepareArgs asks the home to make Target the loop's next attach.
type PrepareArgs struct {
	Loop   string `json:"loop"`
	Target Ref    `json:"target"`
	Note   string `json:"note,omitempty"` // for the new client's status line ("now on B:spare")
}

// Prepared is the attach to run.
type Prepared struct {
	Gen    int      `json:"gen"`
	Target Ref      `json:"target"`
	Local  bool     `json:"local"`
	Argv   []string `json:"argv"`
	Go     string   `json:"go,omitempty"`  // remote: the standby's go line
	Key    string   `json:"key,omitempty"` // remote: the key a standby must have
	RTT    int64    `json:"rtt,omitempty"` // ms, the host's slow recent round trip
	Link   int      `json:"link,omitempty"`
}

// AfterArgs reports how an attach ended.
type AfterArgs struct {
	Loop  string `json:"loop"`
	Gen   int    `json:"gen"`
	Code  int    `json:"code"`
	Ended bool   `json:"ended,omitempty"` // the loop ended it for a stored switch
}

// What the loop does next.
const (
	NextHandoff   = "handoff"
	NextPicker    = "picker"
	NextReconnect = "reconnect"
	NextExit      = "exit"
)

// Next answers after.
type Next struct {
	Do     string `json:"do"`
	Target Ref    `json:"target,omitempty"`
	Note   string `json:"note,omitempty"`
}

// GenArgs names one attach of a loop (wait-switch, held).
type GenArgs struct {
	Loop string `json:"loop"`
	Gen  int    `json:"gen"`
}

// SwitchWake answers wait-switch: Switch is true when a switch was stored
// for this attach; false when the wait ended for another reason.
type SwitchWake struct {
	Switch bool `json:"switch"`
}

// HeldReply answers held: End tells the loop to end the old client itself.
type HeldReply struct {
	End bool `json:"end"`
}

// Offer is a standby the home offers a loop for one host.
type Offer struct {
	Host string   `json:"host"` // towerd id
	Key  string   `json:"key"`
	Argv []string `json:"argv"`
	RTT  int64    `json:"rtt,omitempty"`
}

// RetryArgs names the host (its label) to connect to now.
type RetryArgs struct {
	Host string `json:"host"`
}

// LastArgs asks where tower last goes for Client.
type LastArgs struct {
	Client string `json:"client"`
}

// LastResult: Local means a plain switch-client to Target on the client's
// own server; Stored means a hand-off was stored, and Ended that the loop
// ends the client (otherwise tower last does, as a dashboard would);
// neither means fall back to switch-client -l.
type LastResult struct {
	Local  bool   `json:"local,omitempty"`
	Stored bool   `json:"stored,omitempty"`
	Ended  bool   `json:"ended,omitempty"` // stored, and the loop ends the client
	Target Ref    `json:"target,omitempty"`
	Note   string `json:"note,omitempty"`
}

// GoLine is what a standby shim reads to become an attach.
type GoLine struct {
	Loop    string `json:"loop"`
	Gen     int    `json:"gen"`
	Home    string `json:"home"`
	Inst    string `json:"inst"`
	MKey    string `json:"mkey,omitempty"`
	Session string `json:"s"`
	Window  string `json:"w,omitempty"`
	Pane    string `json:"p,omitempty"`
	Note    string `json:"note,omitempty"` // the loop's note for the new client's status line
}

// ClientID is a TOWER_CLIENT value: the tmux client that pressed a key,
// "pid:created:name" (its pid, client_created and client_name).
type ClientID struct {
	Pid     int
	Created string
	Name    string
}

// ParseClient reads a TOWER_CLIENT value. The name may hold colons.
func ParseClient(s string) (ClientID, error) {
	f := strings.SplitN(s, ":", 3)
	if len(f) == 3 && f[2] != "" {
		if pid, err := strconv.Atoi(f[0]); err == nil && pid > 0 {
			return ClientID{Pid: pid, Created: f[1], Name: f[2]}, nil
		}
	}
	return ClientID{}, fmt.Errorf("TOWER_CLIENT %q is not pid:created:name", s)
}

// LocalName is the name a view gives the machine it is on, whatever its
// host name; no host in the list may take it.
const LocalName = "local"

// PopupSize is the dashboard popup's size, as display-popup arguments:
// the same for the popup tower opens and the M-o binding towerd makes.
var PopupSize = []string{"-w", "90%", "-h", "85%"}

// String is the TOWER_CLIENT form.
func (c ClientID) String() string { return strconv.Itoa(c.Pid) + ":" + c.Created + ":" + c.Name }
