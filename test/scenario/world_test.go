package scenario

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
	"github.com/jerrykal/tower/internal/tmux"
	"github.com/jerrykal/tower/test/scenario/fakenet"
)

// Timings every simulated host runs with: shortened so the suite runs in
// minutes. Scenarios override them per host (World.Timing, Host.Set).
var testTimings = map[string]string{
	"TOWER_PING":         "1000",
	"TOWER_SILENCE":      "2000",
	"TOWER_BACKOFF_BASE": "200",
	"TOWER_BACKOFF_CAP":  "3000",
	"TOWER_STABLE":       "3000",
	"TOWER_NOSRV_POLL":   "400",
	"TOWER_IDLE":         "0",
	"TOWER_TEST_PICKER":  "1",
	"TOWER_TEST_HOOKS":   "1",
}

// ProductionTimings are tower's defaults, for the scenarios that measure
// what a user would see.
var ProductionTimings = map[string]string{
	"TOWER_SILENCE":      "15000",
	"TOWER_BACKOFF_BASE": "1000",
	"TOWER_BACKOFF_CAP":  "120000",
	"TOWER_STABLE":       "30000",
}

// World is one scenario: its hosts, homes and terminals, under one
// directory, torn down when the test ends.
type World struct {
	T        *testing.T
	ID       string
	Dir      string
	Fake     string
	UserHome string
	Marks    string // TOWER_TEST_TIMING file

	hosts   map[string]*Host
	order   []string
	terms   []*Term
	spawned []*exec.Cmd
	sockets []string // tmux servers to end at teardown
	timings map[string]string
}

// NewWorld makes scenario id's world. id is short ("s00", "lc08-150"): it
// goes into tmux socket names.
func NewWorld(t *testing.T, id string) *World {
	t.Helper()
	w := &World{T: t, ID: strings.ToLower(id), hosts: map[string]*Host{}, timings: maps.Clone(testTimings)}
	w.Dir = filepath.Join(root, w.ID)
	os.RemoveAll(w.Dir)
	w.Fake = filepath.Join(w.Dir, "fake")
	w.UserHome = filepath.Join(w.Dir, "userhome")
	w.Marks = filepath.Join(w.Dir, "marks")
	for _, d := range []string{w.Fake, filepath.Join(w.Fake, "hosts"), w.UserHome, filepath.Join(w.Dir, "race")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(w.teardown)
	return w
}

// Timing sets timing variables for every host made after it.
func (w *World) Timing(vars map[string]string) {
	maps.Copy(w.timings, vars)
}

// Host is one simulated machine and tmux server.
type Host struct {
	w         *World
	Name      string
	Machine   string // machine id
	HomeDir   string // TOWER_HOME
	Sock      string // tmux -L name
	BaseIndex int
	extra     map[string]string
	sockPath  string
}

// HostOpt configures a host.
type HostOpt func(*Host)

// Machine puts the host on machine m (several tmux servers on one machine).
func Machine(m string) HostOpt { return func(h *Host) { h.Machine = m } }

// HomeName gives the host the TOWER_HOME of another name (a shared home).
func HomeName(n string) HostOpt {
	return func(h *Host) { h.HomeDir = filepath.Join(h.w.Dir, "home-"+n) }
}

// BaseIndex sets the tmux base-index.
func BaseIndex(n int) HostOpt { return func(h *Host) { h.BaseIndex = n } }

// Env sets an extra variable on the host.
func Env(k, v string) HostOpt { return func(h *Host) { h.extra[k] = v } }

// Host makes a host with the given sessions (none: no tmux server). It is
// also registered with the fake ssh under its name.
func (w *World) Host(name string, sessions []string, opts ...HostOpt) *Host {
	w.T.Helper()
	h := &Host{w: w, Name: name, Machine: name, BaseIndex: 1, extra: map[string]string{},
		Sock: "tt-" + w.ID + "-" + name}
	for _, o := range opts {
		o(h)
	}
	if h.HomeDir == "" {
		h.HomeDir = filepath.Join(w.Dir, "home-"+h.Machine)
	}
	w.hosts[name] = h
	w.order = append(w.order, name)
	w.sockets = append(w.sockets, h.Sock)
	h.writeConf()
	for _, s := range sessions {
		h.NewSession(s)
	}
	w.SSH(name, h, fakenet.Knobs{})
	return h
}

// Get returns the host named name.
func (w *World) Get(name string) *Host { return w.hosts[name] }

func (h *Host) confPath() string { return filepath.Join(h.w.Dir, "tmux-"+h.Name+".conf") }

func (h *Host) writeConf() {
	popup := fmt.Sprintf(`TOWER_CLIENT=#{client_pid}:#{client_created}:#{client_name} %s 2>>%s`, towerBin, filepath.Join(h.w.Dir, "dash-"+h.Name+".log"))
	conf := strings.Join([]string{
		"set -g exit-empty on",
		"set -g default-shell /bin/sh",
		"set -g base-index " + strconv.Itoa(h.BaseIndex),
		"set -g escape-time 10",
		"set -g status-left '[#{host_short}:#S] '",
		"set -g status-left-length 40",
		"set -g detach-on-destroy no-detached",
		"bind -n M-o run-shell -C " + tmux.Quote("display-popup -E -w 100% -h 100% "+tmux.Quote(popup)),
		"bind -n M-l run-shell -C " + tmux.Quote("run-shell -b "+tmux.Quote(`TOWER_CLIENT=#{client_pid}:#{client_created}:#{client_name} `+towerBin+" last")),
	}, "\n") + "\n"
	if err := os.WriteFile(h.confPath(), []byte(conf), 0o600); err != nil {
		h.w.T.Fatal(err)
	}
}

// Set sets an extra environment variable on the host, for processes
// started after.
func (h *Host) Set(k, v string) { h.extra[k] = v }

// EnvMap is the environment of a process "on" the host.
func (h *Host) EnvMap() map[string]string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if k == "TMUX" || k == "TMUX_PANE" || strings.HasPrefix(k, "TOWER_") {
			continue
		}
		m[k] = v
	}
	m["HOME"] = h.w.UserHome
	m["XDG_CONFIG_HOME"] = filepath.Join(h.w.UserHome, ".config")
	m["TOWER_HOME"] = h.HomeDir
	m["TOWER_MACHINE_ID"] = h.Machine
	m["TOWER_TMUX"] = "-L " + h.Sock
	m["TOWER_SSH"] = fakeSSH
	m["TOWER_FAKE_DIR"] = h.w.Fake
	m["TOWER_TEST_TIMING"] = h.w.Marks
	m["TOWER_TEST_NAME"] = h.Name
	if raceBuild {
		m["GORACE"] = "log_path=" + filepath.Join(h.w.Dir, "race", "report")
	}
	maps.Copy(m, h.w.timings)
	for _, k := range []string{"TOWER_STANDBY", "TOWER_RELAY"} {
		if v := os.Getenv(k); v != "" {
			m[k] = v
		}
	}
	maps.Copy(m, h.extra)
	return m
}

// Env is EnvMap as a list.
func (h *Host) Env() []string {
	m := h.EnvMap()
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

func (h *Host) getenv(k string) string { return h.EnvMap()[k] }

// TmuxArgs select the host's server.
func (h *Host) TmuxArgs() []string { return []string{"-L", h.Sock} }

// Tmux runs a tmux command on the host's server.
func (h *Host) Tmux(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tmux.Bin(), append(h.TmuxArgs(), args...)...)
	cmd.Env = h.Env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// MustTmux is Tmux failing the test on error.
func (h *Host) MustTmux(args ...string) string {
	h.w.T.Helper()
	out, err := h.Tmux(args...)
	if err != nil {
		h.w.T.Fatal(err)
	}
	return out
}

// NewSession makes a detached session; the first starts the server with
// the host's config.
func (h *Host) NewSession(name string) {
	h.w.T.Helper()
	args := []string{"new-session", "-d", "-s", name}
	if !h.HasServer() {
		args = append([]string{"-f", h.confPath()}, "new-session", "-d", "-s", name, "-x", "100", "-y", "30")
	}
	var err error
	for range 10 {
		var out string
		out, err = h.Tmux(args...)
		if err == nil || !strings.Contains(out, "exited unexpectedly") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		h.w.T.Fatal(err)
	}
}

// HasServer reports whether the host's tmux server runs.
func (h *Host) HasServer() bool {
	_, err := h.Tmux("list-sessions")
	return err == nil
}

// SessionID is the id of the session named name.
func (h *Host) SessionID(name string) string {
	h.w.T.Helper()
	out, _ := h.Tmux("list-sessions", "-F", "#{session_id}\t#{session_name}")
	for _, l := range strings.Split(out, "\n") {
		id, n, _ := strings.Cut(l, "\t")
		if n == name {
			return id
		}
	}
	h.w.T.Fatalf("%s has no session %q:\n%s", h.Name, name, out)
	return ""
}

// Sessions are the host's session names, _tower left out.
func (h *Host) Sessions() []string {
	out, _ := h.Tmux("list-sessions", "-F", "#{session_name}")
	var s []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" && l != "_tower" {
			s = append(s, l)
		}
	}
	return s
}

// Clients are the sessions of the host's non-control clients, sorted.
func (h *Host) Clients() []string {
	out, _ := h.Tmux("list-clients", "-F", "#{client_control_mode}\t#{session_name}")
	var s []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		cm, name, ok := strings.Cut(l, "\t")
		if ok && cm == "0" {
			s = append(s, name)
		}
	}
	slices.Sort(s)
	return s
}

// ClientIDs are "pid:created:name" of the host's non-control clients on
// session (any session if empty): what a key binding passes as
// TOWER_CLIENT.
func (h *Host) ClientIDs(session string) []string {
	out, _ := h.Tmux("list-clients", "-F", "#{client_control_mode}\t#{session_name}\t#{client_pid}:#{client_created}:#{client_name}")
	var s []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(l, "\t", 3)
		if len(f) == 3 && f[0] == "0" && (session == "" || f[1] == session) {
			s = append(s, f[2])
		}
	}
	return s
}

// ControlClients counts the host's control-mode clients.
func (h *Host) ControlClients() int {
	out, _ := h.Tmux("list-clients", "-F", "#{client_control_mode}")
	return strings.Count(out, "1\n")
}

// SocketPath is the host's tmux socket path.
func (h *Host) SocketPath() string {
	if h.sockPath == "" {
		out, err := h.Tmux("display-message", "-p", "#{socket_path}")
		if err == nil {
			h.sockPath = strings.TrimSpace(out)
		}
	}
	return h.sockPath
}

// Paths are the host's tower paths, computed the way tower does.
func (h *Host) Paths() *config.Env {
	e, err := config.LoadWith(h.getenv, h.TmuxArgs())
	if err != nil {
		h.w.T.Fatal(err)
	}
	return e
}

// Client calls the host's towerd.
func (h *Host) Client() *client.Client {
	return &client.Client{Env: h.Paths(), Version: Version}
}

// Call makes one call to the host's towerd.
func (h *Host) Call(op string, args, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return h.Client().Call(ctx, op, args, result)
}

// Status is the host's full towerd status, or nil when none answers.
func (h *Host) Status() *proto.Status {
	var st proto.Status
	if err := h.Call(proto.CallStatus, proto.StatusArgs{Full: true}, &st); err != nil {
		return nil
	}
	if st.Detail == nil {
		st.Detail = &proto.Detail{}
	}
	return &st
}

// Tower runs the tower CLI on the host and returns its combined output.
func (h *Host) Tower(args ...string) (string, error) {
	return h.TowerEnv(nil, args...)
}

// TowerEnv runs the tower CLI on the host with extra variables.
func (h *Host) TowerEnv(extra map[string]string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, towerBin, args...)
	env := h.EnvMap()
	maps.Copy(env, extra)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// SSH registers ssh name alias with the fake ssh, running commands on
// target with the given knobs.
func (w *World) SSH(alias string, target *Host, k fakenet.Knobs) {
	w.T.Helper()
	if k.Env == nil {
		k.Env = target.EnvMap()
	}
	if err := fakenet.Save(w.Fake, alias, &k); err != nil {
		w.T.Fatal(err)
	}
}

// Knobs changes alias's knobs; live connections see it within 20ms.
func (w *World) Knobs(alias string, f func(*fakenet.Knobs)) {
	w.T.Helper()
	k, err := fakenet.Load(w.Fake, alias)
	if err != nil {
		w.T.Fatal(err)
	}
	f(k)
	if err := fakenet.Save(w.Fake, alias, k); err != nil {
		w.T.Fatal(err)
	}
}

// ResetKnobs puts alias back to an unshaped link, keeping its environment.
func (w *World) ResetKnobs(alias string) {
	w.Knobs(alias, func(k *fakenet.Knobs) { *k = fakenet.Knobs{Env: k.Env, Drop: k.Drop} })
}

// Drop closes alias's live connections.
func (w *World) Drop(alias string) { w.Knobs(alias, func(k *fakenet.Knobs) { k.Drop++ }) }

// SSHLog is every call the fake ssh logged.
func (w *World) SSHLog() []map[string]any {
	b, _ := os.ReadFile(filepath.Join(w.Fake, "ssh.log"))
	var out []map[string]any
	for _, l := range strings.Split(string(b), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if jsonUnmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// Remote is a host as a home lists it.
func (h *Host) Remote() config.Host {
	return config.Host{Name: h.Name, SSH: h.Name, Tmux: "-L " + h.Sock, Tower: towerBin}
}

// Home makes h a home listing remotes, starting its towerd (or, if a
// bridge already started one, activating its home role).
func (w *World) Home(h *Host, remotes ...config.Host) {
	w.T.Helper()
	if err := config.SaveHosts(h.Paths().HostsFile(), remotes); err != nil {
		w.T.Fatal(err)
	}
	if h.Status() != nil {
		if err := h.Call(proto.CallReload, nil, nil); err != nil {
			w.T.Fatal(err)
		}
		return
	}
	w.StartTowerd(h)
}

// StartTowerd spawns `tower towerd` on h and waits until it answers.
func (w *World) StartTowerd(h *Host) {
	w.T.Helper()
	cmd := exec.Command(towerBin, "towerd")
	cmd.Env = h.Env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		w.T.Fatal(err)
	}
	w.spawned = append(w.spawned, cmd)
	go cmd.Wait()
	for range 100 {
		if h.Status() != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	w.T.Fatalf("towerd on %s never answered", h.Name)
}

// Link is the home's link to the host named name, or the local host's
// watch when name is the home itself.
func (w *World) Link(home *Host, name string) proto.LinkStatus {
	st := home.Status()
	if st == nil {
		return proto.LinkStatus{Status: "no towerd"}
	}
	if name == home.Name {
		ws := st.Detail.Watch
		s := proto.StatusLocal
		if ws.NoServer {
			s = "nosrv"
		}
		return proto.LinkStatus{Name: name, Status: s, ID: st.ID, MKey: st.MKey, Inst: ws.Inst, Sessions: ws.Sessions}
	}
	for _, l := range st.Detail.Links {
		if l.Name == name {
			return l
		}
	}
	return proto.LinkStatus{Status: "unknown"}
}

// WaitLink waits until the home's link to name is in a status matching
// re, and returns how long it took.
func (w *World) WaitLink(home *Host, name, re string, d time.Duration) time.Duration {
	w.T.Helper()
	start := time.Now()
	rx := regexp.MustCompile("^(?:" + re + ")$")
	var last proto.LinkStatus
	for time.Since(start) < d {
		last = w.Link(home, name)
		if rx.MatchString(last.Status) {
			return time.Since(start)
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.T.Fatalf("%s's link to %s is %q (%s) after %v, want %s", home.Name, name, last.Status, last.Reason, d, re)
	return 0
}

// WaitUp waits for each named host to be up at the home.
func (w *World) WaitUp(home *Host, names ...string) {
	w.T.Helper()
	for _, n := range names {
		w.WaitLink(home, n, "up|local", 6*time.Second)
	}
}

// Loops are the home's loops.
func (w *World) Loops(home *Host) []proto.LoopStatus {
	if st := home.Status(); st != nil {
		return st.Detail.Loops
	}
	return nil
}

// FormatRef is how scenarios match a target: host:session[:window].
func FormatRef(r proto.Ref) string {
	s := r.Name + ":" + r.Label
	if r.Window != "" {
		s += ":" + r.Window
	}
	return s
}

// WaitLoop waits until some loop of the home has a current target
// matching re ("^B:bravo").
func (w *World) WaitLoop(home *Host, re string, d time.Duration) proto.LoopStatus {
	w.T.Helper()
	rx := regexp.MustCompile(re)
	start := time.Now()
	var seen []string
	for time.Since(start) < d {
		seen = seen[:0]
		for _, l := range w.Loops(home) {
			seen = append(seen, FormatRef(l.Cur))
			if rx.MatchString(FormatRef(l.Cur)) {
				return l
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.T.Fatalf("no loop of %s reached %s within %v; loops are at %v", home.Name, re, d, seen)
	return proto.LoopStatus{}
}

// Eventually polls cond every 20ms until it holds or d passes.
func (w *World) Eventually(d time.Duration, what string, cond func() bool) {
	w.T.Helper()
	start := time.Now()
	for time.Since(start) < d {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.T.Fatalf("%s: not within %v", what, d)
}

// Marks reads the timing marks: each "<unix µs> <what>".
func (w *World) ReadMarks() []Mark {
	b, _ := os.ReadFile(w.Marks)
	var out []Mark
	for _, l := range strings.Split(string(b), "\n") {
		ts, what, ok := strings.Cut(l, " ")
		if !ok {
			continue
		}
		us, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, Mark{At: time.UnixMicro(us), What: what})
	}
	slices.SortStableFunc(out, func(a, b Mark) int { return a.At.Compare(b.At) })
	return out
}

// ClearMarks empties the marks file.
func (w *World) ClearMarks() { os.WriteFile(w.Marks, nil, 0o600) }

// Mark is one timing mark.
type Mark struct {
	At   time.Time
	What string
}
