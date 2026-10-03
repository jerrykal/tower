// Package fakenet is the contract between the scenario harness and the
// fake ssh: each simulated host's link and faults, as a JSON file per ssh
// name under $TOWER_FAKE_DIR/hosts.
package fakenet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Knobs is one fake host's link and faults. The harness rewrites the
// file; live connections re-read it every PollEvery.
type Knobs struct {
	Env        map[string]string `json:"env"`                   // the remote side's whole environment
	Down       string            `json:"down,omitempty"`        // refused, hostkey, auth, password, resolve, timeout, tscheck
	LatencyMs  int               `json:"latency_ms,omitempty"`  // extra delay of a connection's start
	Exit       *int              `json:"exit,omitempty"`        // forced exit status when the remote command ends
	Freeze     bool              `json:"freeze,omitempty"`      // half-open: nothing moves; ssh gives up after the alive window
	Drop       int               `json:"drop,omitempty"`        // raising it closes live connections
	ODelayMs   int               `json:"o_delay_ms,omitempty"`  // every ssh -O takes this long
	DelayMs    int               `json:"delay_ms,omitempty"`    // one-way delay of each chunk
	JitterMs   int               `json:"jitter_ms,omitempty"`   // ± per chunk, order kept
	BwKBps     int               `json:"bw_kbps,omitempty"`     // KB/s per direction; 0 unlimited
	WindowKB   int               `json:"window_kb,omitempty"`   // bytes in flight per direction (2048)
	Stall      bool              `json:"stall,omitempty"`       // no byte moves; ssh never gives up
	Pty        bool              `json:"pty,omitempty"`         // ssh -t gets a pty on the host
	Mux        bool              `json:"mux,omitempty"`         // a control master shared by the host's sessions
	HalfOpenAt int64             `json:"halfopen_at,omitempty"` // unix ms: masters made before it are dead
}

// PollEvery is how often a live connection re-reads its host's knobs.
const PollEvery = 20 * time.Millisecond

// Path is the knobs file of ssh name alias under dir.
func Path(dir, alias string) string { return filepath.Join(dir, "hosts", alias+".json") }

// Load reads alias's knobs.
func Load(dir, alias string) (*Knobs, error) {
	b, err := os.ReadFile(Path(dir, alias))
	if err != nil {
		return nil, err
	}
	var k Knobs
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// Save writes alias's knobs atomically, so a reader never sees half a
// file.
func Save(dir, alias string, k *Knobs) error {
	b, err := json.Marshal(k)
	if err != nil {
		return err
	}
	p := Path(dir, alias)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// RTT is the link's round trip.
func (k *Knobs) RTT() time.Duration { return 2 * time.Duration(k.DelayMs) * time.Millisecond }

// Window is the bytes in flight per direction.
func (k *Knobs) Window() int {
	if k.WindowKB > 0 {
		return k.WindowKB << 10
	}
	return 2048 << 10
}
