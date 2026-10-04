package proto

import (
	"fmt"
	"strconv"
	"strings"
)

// Min and Max are the range of stream protocol versions this build speaks.
const (
	Min = 1
	Max = 1
)

// Stream message types.
const (
	THello = "hello"
	TState = "state"
	TView  = "view"
	TExec  = "exec"
	TRelay = "relay"
	TAck   = "ack"
	TPing  = "ping"
	TPong  = "pong"
	TLook  = "look" // a dashboard opened: refresh git state and zoxide directories
)

// Msg is one line of the home ↔ remote stream. T names the type and the
// matching field is set; a receiver ignores types it does not know.
type Msg struct {
	T     string   `json:"t"`
	Hello *Hello   `json:"hello,omitempty"`
	State *State   `json:"state,omitempty"`
	View  *View    `json:"view,omitempty"`
	Req   *Request `json:"req,omitempty"`
	Ack   *Ack     `json:"ack,omitempty"`
	Ping  *Ping    `json:"ping,omitempty"`
}

// Hello opens a stream in both directions. The home sends its range and
// identity; the remote answers with its choice, or Err.
type Hello struct {
	Min     int    `json:"min"`
	Max     int    `json:"max"`
	Proto   int    `json:"proto,omitempty"`
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	As      string `json:"as,omitempty"`
	Version string `json:"version,omitempty"`
	MKey    string `json:"mkey,omitempty"`
	OS      string `json:"os,omitempty"`
	Tmux    string `json:"tmux,omitempty"`
	Err     string `json:"err,omitempty"`
}

// Ping is a keepalive; its pong carries the same N and the sender's wall
// clock in unix milliseconds.
type Ping struct {
	N     uint64 `json:"n"`
	Clock int64  `json:"clock,omitempty"`
}

// Negotiate picks the highest protocol version both ranges contain.
func Negotiate(aMin, aMax, bMin, bMax int) (int, error) {
	hi := min(aMax, bMax)
	if hi < max(aMin, bMin) {
		return 0, fmt.Errorf("incompatible protocol: home speaks %d-%d, this host %d-%d", aMin, aMax, bMin, bMax)
	}
	return hi, nil
}

// Request ops.
const (
	OpKill    = "kill"
	OpRename  = "rename"
	OpNew     = "new"
	OpCapture = "capture"
	OpHas     = "has"
	OpSwitch  = "switch"
	OpDup     = "dup"
	OpPanes   = "panes" // a session's (or window's) panes: layout and what runs there
)

// Kinds of things a request acts on.
const (
	KindSession = "session"
	KindWindow  = "window"
)

// Request is an action asked of some towerd: from a dashboard (the act
// call), relayed to a home (relay), or sent by a home to a remote (exec).
type Request struct {
	ID       string `json:"id"`
	Op       string `json:"op"`
	Deadline int64  `json:"dl,omitempty"` // unix ms in the holder's clock
	Target   Ref    `json:"target"`
	Kind     string `json:"kind,omitempty"`
	Name     string `json:"name,omitempty"`
	Dir      string `json:"dir,omitempty"`
	Client   string `json:"client,omitempty"` // pid:created:name of the pressing client
	Loop     string `json:"loop,omitempty"`
	Gen      int    `json:"gen,omitempty"`
	Nonce    string `json:"nonce,omitempty"`
	From     string `json:"from,omitempty"` // towerd id of the asking machine
}

// Ack answers a Request.
type Ack struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Err   string `json:"err,omitempty"`
	Note  string `json:"note,omitempty"`
	Ended bool   `json:"ended,omitempty"` // switch: the loop ended the old client
	Gone  bool   `json:"gone,omitempty"`  // has: no such session (or no server)
	Text  string `json:"text,omitempty"`  // capture
	Panes []Pane `json:"panes,omitempty"` // capture (the target window's), panes
	Ref   *Ref   `json:"ref,omitempty"`   // new, dup: what was made
}

// Newer reports whether tower version a is newer than b. Versions are
// "X.Y.Z" or "X.Y.Z-dev+commit"; a development build sorts after its base
// release. Two different builds of one base are not ordered: Newer is
// false both ways, and Same tells them apart.
func Newer(a, b string) bool {
	an, ad := splitVersion(a)
	bn, bd := splitVersion(b)
	for i := range an {
		if an[i] != bn[i] {
			return an[i] > bn[i]
		}
	}
	return ad != "" && bd == ""
}

func splitVersion(v string) ([3]int, string) {
	var n [3]int
	base, dev, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	for i, p := range strings.SplitN(base, ".", 3) {
		n[i], _ = strconv.Atoi(p)
	}
	return n, dev
}
