package proto

import "encoding/json"

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
	Detail json.RawMessage `json:"detail,omitempty"`
}

// StopArgs asks towerd to exit; with IfOlderThan set, only if its version
// is older than that.
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

// LastArgs asks where tower last goes for Client.
type LastArgs struct {
	Client string `json:"client"`
}

// LastResult: Local means a plain switch-client to Target on the client's
// own server; Stored means a hand-off was stored and the loop will act;
// neither means fall back to switch-client -l.
type LastResult struct {
	Local  bool   `json:"local,omitempty"`
	Stored bool   `json:"stored,omitempty"`
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
}
