// Package config is where tower's files live and who this towerd is:
// paths per machine and tmux server, identities, the host list and small
// file helpers.
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Env is where one towerd (one user, machine and tmux server) keeps its
// files.
type Env struct {
	Home      string   // TOWER_HOME, or ""
	ConfigDir string   // ~/.config/tower, or $TOWER_HOME/config
	StateRoot string   // ~/.local/state/tower, or $TOWER_HOME/state
	Tmux      []string // tmux arguments selecting the server
	Tag       string   // default, L-<name>, S-<hash>
	MKey      string   // machine key
	StateDir  string   // StateRoot/towerd/<mkey>-<tag>
	RunDir    string   // sockets and control paths
}

// sockMax is the longest unix socket path the platforms allow (macOS's
// sun_path is 104 bytes, Linux's 108).
const sockMax = 104

// ssh appends this many bytes to a control path while it creates the
// socket.
const sshTempSuffix = 17

// Load builds the Env for the tmux server selected by tmuxArgs (nil: the
// TOWER_TMUX environment variable). Directories are created.
func Load(tmuxArgs []string) (*Env, error) { return LoadWith(os.Getenv, tmuxArgs) }

// LoadWith is Load reading the environment through getenv: the scenario
// harness computes a simulated host's paths with that host's variables.
func LoadWith(getenv func(string) string, tmuxArgs []string) (*Env, error) {
	if tmuxArgs == nil {
		tmuxArgs = strings.Fields(getenv("TOWER_TMUX"))
	}
	e := &Env{Home: getenv("TOWER_HOME"), Tmux: tmuxArgs, Tag: TmuxTag(tmuxArgs)}
	if e.Home != "" {
		e.ConfigDir = filepath.Join(e.Home, "config")
		e.StateRoot = filepath.Join(e.Home, "state")
	} else {
		home := getenv("HOME")
		if home == "" {
			var err error
			if home, err = os.UserHomeDir(); err != nil {
				return nil, err
			}
		}
		e.ConfigDir = xdg(getenv, "XDG_CONFIG_HOME", filepath.Join(home, ".config"), "tower")
		e.StateRoot = xdg(getenv, "XDG_STATE_HOME", filepath.Join(home, ".local", "state"), "tower")
	}
	mkey, err := machineKey(getenv)
	if err != nil {
		return nil, err
	}
	e.MKey = mkey
	e.StateDir = filepath.Join(e.StateRoot, "towerd", mkey+"-"+e.Tag)
	if err := os.MkdirAll(e.StateDir, 0o700); err != nil {
		return nil, err
	}
	e.RunDir, err = runDir(getenv, e)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(e.CMDir(), 0o700); err != nil {
		return nil, err
	}
	return e, nil
}

func xdg(getenv func(string) string, name, def, sub string) string {
	if v := getenv(name); filepath.IsAbs(v) {
		return filepath.Join(v, sub)
	}
	return filepath.Join(def, sub)
}

// Socket is towerd's unix socket.
func (e *Env) Socket() string { return filepath.Join(e.RunDir, e.Tag+".sock") }

// LockPath is the file whose flock makes one towerd per server.
func (e *Env) LockPath() string { return filepath.Join(e.RunDir, e.Tag+".lock") }

// CMDir holds ssh control sockets.
func (e *Env) CMDir() string { return filepath.Join(e.RunDir, "cm") }

// HostsFile is the host list.
func (e *Env) HostsFile() string { return filepath.Join(e.ConfigDir, "hosts.toml") }

// State returns a file in the towerd's state directory.
func (e *Env) State(name string) string { return filepath.Join(e.StateDir, name) }

// TmuxTag names the tmux server tmuxArgs select: default, L-<name> or
// S-<hash of the socket path>.
func TmuxTag(tmuxArgs []string) string {
	for i := 0; i < len(tmuxArgs); i++ {
		a := tmuxArgs[i]
		switch {
		case a == "-L" && i+1 < len(tmuxArgs):
			return "L-" + safeTag(tmuxArgs[i+1])
		case strings.HasPrefix(a, "-L") && len(a) > 2:
			return "L-" + safeTag(a[2:])
		case a == "-S" && i+1 < len(tmuxArgs):
			return "S-" + shortHash(tmuxArgs[i+1], 8)
		case strings.HasPrefix(a, "-S") && len(a) > 2:
			return "S-" + shortHash(a[2:], 8)
		}
	}
	return "default"
}

var unsafeTag = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// safeTag keeps a -L name usable in a file name; a name that had to change
// gets a hash so two names never collide.
func safeTag(name string) string {
	s := unsafeTag.ReplaceAllString(name, "_")
	if s != name || len(s) > 32 {
		if len(s) > 23 {
			s = s[:23]
		}
		s += "-" + shortHash(name, 8)
	}
	return s
}

func shortHash(s string, n int) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:n]
}

var mkeyRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ValidMKey reports whether s has the form of a machine key.
func ValidMKey(s string) bool { return mkeyRE.MatchString(s) }

// MachineKey is sha256(machine id | uid), 12 hex. TOWER_MKEY, when valid,
// is taken as is: processes that start helpers pass their key down so the
// helpers skip the lookup (ioreg costs about 10ms on macOS).
func MachineKey() (string, error) { return machineKey(os.Getenv) }

func machineKey(getenv func(string) string) (string, error) {
	if k := getenv("TOWER_MKEY"); ValidMKey(k) {
		return k, nil
	}
	id, err := machineID(getenv)
	if err != nil {
		return "", err
	}
	return shortHash(id+"|"+strconv.Itoa(os.Getuid()), 12), nil
}

func machineID(getenv func(string) string) (string, error) {
	if id := getenv("TOWER_MACHINE_ID"); id != "" {
		return id, nil
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err != nil {
			return "", fmt.Errorf("machine id: %w", err)
		}
		m := regexp.MustCompile(`"IOPlatformUUID" = "([^"]+)"`).FindSubmatch(out)
		if m == nil {
			return "", errors.New("machine id: no IOPlatformUUID")
		}
		return string(m[1]), nil
	}
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b)), nil
		}
	}
	h, err := os.Hostname()
	if err != nil {
		return "", errors.New("machine id: none found")
	}
	return "host:" + h, nil
}

// runDir picks where sockets go: $XDG_RUNTIME_DIR/tower/<machine key>
// unless TOWER_HOME is set (it is per user, while a TOWER_HOME is one
// simulated machine of several on one box), else the first candidate short
// enough for every socket it will hold, each keyed by the state root and
// machine key.
func runDir(getenv func(string) string, e *Env) (string, error) {
	if x := getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(x) && e.Home == "" {
		d := filepath.Join(x, "tower", e.MKey)
		if fits(d, e.Tag) {
			return d, os.MkdirAll(d, 0o700)
		}
	}
	uid := strconv.Itoa(os.Getuid())
	h := shortHash(e.StateRoot+"|"+e.MKey, 8)
	tmp := getenv("TMPDIR")
	if tmp == "" {
		tmp = "/tmp"
	}
	cands := []string{
		filepath.Join(e.StateRoot, "run-"+e.MKey),
		filepath.Join(tmp, "tower-"+uid, h),
		filepath.Join("/tmp", "tower-"+uid, h),
	}
	for _, d := range cands {
		if fits(d, e.Tag) {
			if err := os.MkdirAll(d, 0o700); err != nil {
				continue
			}
			return d, nil
		}
	}
	return "", fmt.Errorf("no run directory short enough for a %d-byte socket path (tried %s)", sockMax, strings.Join(cands, ", "))
}

// fits reports whether every socket under d fits: the towerd socket, and a
// control path (cm/ and %C's 40 hex) while ssh creates it.
func fits(d, tag string) bool {
	towerd := len(d) + 1 + len(tag) + len(".sock")
	cm := len(d) + len("/cm/") + 40 + sshTempSuffix
	return towerd < sockMax && cm < sockMax
}

// NewID returns 8 random hex characters: towerd, loop, request ids, nonces.
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// TowerdID reads the towerd id in stateDir, making it on first use.
func TowerdID(stateDir string) (string, error) {
	p := filepath.Join(stateDir, "id")
	if b, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(b)); len(id) == 8 {
			return id, nil
		}
	}
	id := NewID()
	if err := WriteFile(p, []byte(id+"\n")); err != nil {
		return "", err
	}
	return id, nil
}
