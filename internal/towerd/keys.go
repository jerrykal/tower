package towerd

import (
	"slices"
	"strings"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/tmux"
	"github.com/jerrykal/tower/internal/transport"
)

// alertIndex is the hook array index towerd's alert hooks use.
const alertIndex = "7193"

var alertHooks = []string{"alert-bell", "alert-activity", "alert-silence"}

// keyRec is a key towerd bound: what it replaced, and its own binding as
// list-keys prints it, so stop puts back only a key that still has it.
type keyRec struct {
	Table string `json:"table"`
	Key   string `json:"key"`
	Prev  string `json:"prev,omitempty"` // the replaced binding, a bind-key line; empty: unbound
	Ours  string `json:"ours"`
}

type keysFile struct {
	Keys []keyRec `json:"keys"`
}

// keys binds M-o (the dashboard) and prefix L (tower last) where the
// user's config leaves them free, and adds the alert hooks.
type keys struct {
	w *watcher
}

func newKeys(w *watcher) *keys { return &keys{w: w} }

func (k *keys) path() string { return k.w.d.env.State("keys.json") }

// binding finds key's line in a list-keys listing of one table:
// "bind-key [-r] -T table key command…". It returns the line and the
// command.
func binding(lines []string, table, key string) (line, cmd string) {
	for _, l := range lines {
		f := strings.Fields(l)
		i := slices.Index(f, "-T")
		if i < 0 || i+2 >= len(f) || f[i+1] != table || f[i+2] != key {
			continue
		}
		rest := l
		for range i + 3 {
			rest = strings.TrimLeft(rest, " \t")
			rest = rest[strings.IndexAny(rest, " \t")+1:]
		}
		return l, strings.TrimSpace(rest)
	}
	return "", ""
}

func (k *keys) list(ctl *tmux.Control, table string) []string {
	r, err := ctl.Do("list-keys -T " + table)
	if err != nil || r.Err {
		return nil
	}
	return r.Lines
}

// shellLine is the shell command a binding runs: TOWER_CLIENT from the
// pressing client, towerd's own TOWER_* variables (a binding starts from
// the server's environment, not towerd's), and the tower binary. Each
// format expansion on the way doubles nothing but the #s escaped here.
func (k *keys) shellLine(expansions int, args ...string) string {
	d := k.w.d
	var b strings.Builder
	b.WriteString("TOWER_CLIENT=#{client_pid}:#{client_created}:#{client_name}")
	env := slices.Clone(d.towerEnv)
	env = slices.DeleteFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "TOWER_TMUX=") || strings.HasPrefix(kv, "TOWER_MKEY=") || strings.HasPrefix(kv, "TOWER_TMUX_BIN=")
	})
	env = append(env, "TOWER_TMUX="+strings.Join(d.env.Tmux, " "), "TOWER_MKEY="+d.env.MKey, "TOWER_TMUX_BIN="+d.tm.Bin)
	slices.Sort(env)
	for _, kv := range env {
		// Only the value is quoted: a quoted NAME=value is no assignment.
		k, v, _ := strings.Cut(kv, "=")
		s := k + "=" + transport.ShellQuote(v)
		for range expansions {
			s = tmux.Literal(s)
		}
		b.WriteString(" " + s)
	}
	exe := transport.ShellQuote(d.self)
	for range expansions {
		exe = tmux.Literal(exe)
	}
	b.WriteString(" " + exe)
	for _, a := range args {
		b.WriteString(" " + a)
	}
	return b.String()
}

// install takes the keys and hooks (TOWER_BIND=0, TOWER_ALERTS=0 turn
// them off) and records what it replaced.
func (k *keys) install(ctl *tmux.Control) {
	d := k.w.d
	var kf keysFile
	config.ReadJSON(k.path(), &kf)
	var state []string
	if config.Flag("TOWER_BIND", true) {
		take := func(table, key, cmd string, free func(cmd string) bool) {
			lines := k.list(ctl, table)
			line, cur := binding(lines, table, key)
			prev := line
			i := slices.IndexFunc(kf.Keys, func(r keyRec) bool { return r.Table == table && r.Key == key })
			if i >= 0 && line == kf.Keys[i].Ours {
				// Still ours from a towerd that did not stop cleanly.
				prev = kf.Keys[i].Prev
			} else if line != "" && !free(cur) {
				d.logf("keys: %s %s is bound to %q: left alone", table, key, cur)
				if i >= 0 {
					kf.Keys = slices.Delete(kf.Keys, i, i+1)
				}
				return
			}
			bind := "bind-key -T " + table + " " + key + " " + cmd
			if r, err := ctl.Do(bind); err != nil || r.Err {
				d.logf("keys: %s: %s %v", bind, r.Text(), err)
				return
			}
			ours, _ := binding(k.list(ctl, table), table, key)
			rec := keyRec{Table: table, Key: key, Prev: prev, Ours: ours}
			if i >= 0 {
				kf.Keys[i] = rec
			} else {
				kf.Keys = append(kf.Keys, rec)
			}
			state = append(state, table+" "+key)
		}
		popup := "display-popup -E -w 90% -h 90% " + tmux.Quote(k.shellLine(1))
		take("root", "M-o", "run-shell -C "+tmux.Quote(popup), func(string) bool { return false })
		last := "run-shell -b " + tmux.Quote(k.shellLine(2, "last"))
		take("prefix", "L", "run-shell -C "+tmux.Quote(last), func(cur string) bool { return cur == "switch-client -l" })
	}
	config.WriteJSON(k.path(), kf)
	if config.Flag("TOWER_ALERTS", true) {
		for _, hook := range alertHooks {
			r, err := ctl.Do("show-hooks -g " + hook)
			if err != nil {
				continue
			}
			cur := hookAt(r.Lines, hook)
			if cur != "" && !strings.Contains(cur, "tower-alert") {
				d.logf("keys: %s[%s] is the user's: left alone", hook, alertIndex)
				continue
			}
			cmd := "display-message -c " + tmux.Quote(ctl.Name()) + " tower-alert"
			if r, err := ctl.Do("set-hook -g " + tmux.Quote(hook+"["+alertIndex+"]") + " " + tmux.Quote(cmd)); err != nil || r.Err {
				d.logf("keys: set-hook %s: %s %v", hook, r.Text(), err)
				continue
			}
		}
		state = append(state, "alerts")
	}
	d.mu.Lock()
	d.bindState = strings.Join(state, ", ")
	d.mu.Unlock()
}

// hookAt is the command at alertIndex in a show-hooks listing.
func hookAt(lines []string, hook string) string {
	p := hook + "[" + alertIndex + "] "
	for _, l := range lines {
		if strings.HasPrefix(l, p) {
			return strings.TrimSpace(strings.TrimPrefix(l, p))
		}
	}
	return ""
}

// restore puts back every key that still has towerd's binding, and
// removes towerd's alert hooks.
func (k *keys) restore(ctl *tmux.Control) {
	d := k.w.d
	var kf keysFile
	config.ReadJSON(k.path(), &kf)
	var keep []keyRec
	for _, rec := range kf.Keys {
		line, _ := binding(k.list(ctl, rec.Table), rec.Table, rec.Key)
		if line != rec.Ours {
			continue // the user rebound it meanwhile
		}
		cmd := "unbind-key -T " + rec.Table + " " + rec.Key
		if rec.Prev != "" {
			cmd = rec.Prev
		}
		if r, err := ctl.Do(cmd); err != nil || r.Err {
			d.logf("keys: restoring %s %s: %s %v", rec.Table, rec.Key, r.Text(), err)
			keep = append(keep, rec)
		}
	}
	config.WriteJSON(k.path(), keysFile{Keys: keep})
	for _, hook := range alertHooks {
		r, err := ctl.Do("show-hooks -g " + hook)
		if err != nil {
			continue
		}
		if strings.Contains(hookAt(r.Lines, hook), "tower-alert") {
			ctl.Do("set-hook -gu " + tmux.Quote(hook+"["+alertIndex+"]"))
		}
	}
}
