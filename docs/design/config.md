# config

Where tower's files are, who this towerd is, and the host list. Pure
functions over the environment and the file system, plus small file
helpers; no daemons.

## Env

```go
type Env struct {
    Home      string   // TOWER_HOME, or ""
    ConfigDir string   // ~/.config/tower or $TOWER_HOME/config
    StateRoot string   // ~/.local/state/tower or $TOWER_HOME/state
    Tmux      []string // TOWER_TMUX split into arguments (-L name, -S path)
    Tag       string   // default, L-<name>, S-<hash>
    MKey      string   // machine key
    StateDir  string   // StateRoot/towerd/<mkey>-<tag>
    RunDir    string   // see below
}

func Load(tmuxArgs []string) (*Env, error)
func (e *Env) Socket() string // RunDir/<tag>.sock
func (e *Env) Lock() string   // RunDir/<tag>.lock
func (e *Env) CMDir() string  // RunDir/cm
```

- **Machine key**: sha256 of the machine id and the uid, 12 hex. The id is
  `TOWER_MACHINE_ID` when set (tests), else `/etc/machine-id` (Linux) or
  `IOPlatformUUID` from `ioreg` (macOS). `TOWER_MKEY` (12 lowercase hex) is
  taken as is.
- **Tag** from the tmux arguments: `-L name` → `L-name`, `-S path` →
  `S-` + 8 hex of the path's hash, nothing → `default`.
- **Run dir**: `$XDG_RUNTIME_DIR/tower/<mkey>` if set and `TOWER_HOME` is
  not (several simulated machines on one box must not share it); otherwise the first of
  `StateRoot/run-<mkey>`, `$TMPDIR/tower-<uid>/<hash>`,
  `/tmp/tower-<uid>/<hash>` whose longest socket path fits in 104 bytes,
  the longest being a control path (`cm/` + 40 hex for `%C`) plus ssh's 17
  temporary bytes. Directories are made 0700.

## Identity

```go
func TowerdID(stateDir string) (string, error) // read or create "id": 8 random hex
func NewID() string                            // 8 random hex (loops, requests, nonces)
```

## Hosts

```go
type Host struct {
    Name              string `toml:"name"`
    SSH               string `toml:"ssh"`
    Tmux              string `toml:"tmux,omitempty"`
    Tower             string `toml:"tower,omitempty"`
    Enabled           *bool  `toml:"enabled,omitempty"`
    Standby           *bool  `toml:"standby,omitempty"`
    ObscureKeystrokes bool   `toml:"obscure_keystrokes,omitempty"`
}

func LoadHosts(path string) ([]Host, error)
func SaveHosts(path string, hosts []Host) error // atomic rename
func DefaultName(target string) string          // me@box.lan:2222 → box
```

Names are unique; a load with duplicates fails naming them. A missing file
is an empty list.

## State files

`WriteJSON(path, v)` writes atomically (temp file and rename);
`ReadJSON(path, v)` treats a missing file as empty. Files are 0600.
