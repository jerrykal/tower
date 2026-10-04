package ui

import (
	"context"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/hosts"
	"github.com/jerrykal/tower/internal/transport"
)

// HostList is the host list the dashboard edits: hosts.toml on the
// home's machine (hosts.List). nil: the dashboard edits no hosts.
type HostList interface {
	Load() ([]config.Host, error)
	Aliases() []string
	Add(ctx context.Context, h config.Host, step func(hosts.Step)) ([]transport.Check, error)
	Check(ctx context.Context, h config.Host, step func(hosts.Step)) []transport.Check
	Remove(name string) error
	Rename(old, name string) error
	SetOn(name string, on bool) error
}

var _ HostList = (*hosts.List)(nil)

// picker is the add-host picker: the ssh aliases not in the list, and a
// last row adding the query itself as an ssh target.
type picker struct {
	in      input
	list    []config.Host
	aliases []string // every ssh alias, for the naming rules
	cands   []string // the aliases not in the list
	shown   []string
	hits    [][]int
	raw     bool // the "+ ssh <query>" row
	at      int
}

func (p *picker) refilter() {
	q := foldQuery(p.in.text)
	p.shown, p.hits = nil, nil
	exact := false
	for _, a := range p.cands {
		if pos, ok := filterMatch(q, a); ok {
			p.shown = append(p.shown, a)
			p.hits = append(p.hits, pos)
		}
		exact = exact || a == strings.TrimSpace(p.in.String())
	}
	p.raw = strings.TrimSpace(p.in.String()) != "" && !exact
	p.at = min(max(p.at, 0), max(p.rows()-1, 0))
}

func (p *picker) rows() int {
	n := len(p.shown)
	if p.raw {
		n++
	}
	return n
}

// openPicker is a: the add-host picker.
func (m *Model) openPicker() tea.Cmd {
	if why := m.hostEditable(nil); why != "" {
		m.setErr(why)
		return nil
	}
	list, err := m.c.Hosts.Load()
	if err != nil {
		m.setErr(err.Error())
		return nil
	}
	p := &picker{list: list, aliases: m.c.Hosts.Aliases()}
	for _, a := range p.aliases {
		taken := slices.ContainsFunc(list, func(h config.Host) bool {
			return strings.EqualFold(h.Target(), a) || strings.EqualFold(h.Name, a)
		})
		if !taken {
			p.cands = append(p.cands, a)
		}
	}
	p.refilter()
	m.back = m.mode
	m.picker, m.mode = p, modeAddHost
	return nil
}

func (m *Model) pickerKey(k tea.KeyPressMsg) tea.Cmd {
	p := m.picker
	s := k.String()
	switch s {
	case "esc", "ctrl+c":
		m.picker, m.mode = nil, m.back
		return nil
	case "ctrl+j", "down", "ctrl+n":
		p.at = min(p.at+1, max(p.rows()-1, 0))
		return nil
	case "ctrl+k", "up", "ctrl+p":
		p.at = max(p.at-1, 0)
		return nil
	case "enter":
		return m.pick()
	}
	if s == "backspace" || s == "ctrl+u" {
		p.in.edit(s, false)
		p.refilter()
		return nil
	}
	if t := typed(k); t != "" {
		p.in.cur = len(p.in.text)
		p.in.insert(t)
		p.refilter()
	}
	return nil
}

// pick is ⏎ in the picker: an alias is added under its own name unless
// that is taken; a raw target, or a taken alias, asks for a name.
func (m *Model) pick() tea.Cmd {
	p := m.picker
	if p.rows() == 0 {
		return nil
	}
	m.picker, m.mode = nil, m.back
	if p.at < len(p.shown) {
		alias := p.shown[p.at]
		name := hosts.Clean(alias)
		if err := hosts.CheckName(p.list, p.aliases, name, alias, -1); err == nil {
			return m.addHost(config.Host{Name: name, SSH: alias})
		}
		return m.askHostName(p, alias, name+" is taken")
	}
	return m.askHostName(p, strings.TrimSpace(p.in.String()), "")
}

func (m *Model) askHostName(p *picker, target, why string) tea.Cmd {
	return m.ask(&prompt{kind: pHostName, pill: "HOST NAME", about: "for " + target, ssh: target,
		hint: "⏎ for " + hosts.FreeName(p.list, p.aliases, target), err: why}, "")
}

// hostCheck is a host being added or checked again, as its row and the
// breadcrumb show it.
type hostCheck struct {
	host    config.Host
	running bool
	step    hosts.Step // the latest
	checks  []transport.Check
	added   bool
}

// failed is the first failed check, if any.
func (c *hostCheck) failed() bool { return hosts.Failed(c.checks) }

// reason is why the host failed its checks.
func (c *hostCheck) reason() string {
	for _, k := range c.checks {
		if !k.OK {
			return k.Name + ": " + k.Detail
		}
	}
	return ""
}

// checkMsg is one step of a host's checks, or their end.
type checkMsg struct {
	name   string
	step   hosts.Step
	done   bool
	checks []transport.Check
	err    error
	ch     chan checkMsg
}

func waitCheck(ch chan checkMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		msg.ch = ch
		return msg
	}
}

// addHost runs the checks of a new host (the same as tower host add),
// shown in its row as they go, and adds it to the list.
func (m *Model) addHost(h config.Host) tea.Cmd {
	m.checks[h.Name] = &hostCheck{host: h, running: true}
	m.focus = colHosts
	if m.mode == modeFind {
		m.mode = modeNormal
	}
	m.setBusy("checking " + h.Name + "…")
	return tea.Batch(m.runChecks(h, true), m.spinIfNeeded())
}

// recheck is ⏎ on a host whose check failed: the checks again.
func (m *Model) recheck(c *hostCheck) tea.Cmd {
	c.running, c.checks = true, nil
	m.setBusy("checking " + c.host.Name + "…")
	return tea.Batch(m.runChecks(c.host, false), m.spinIfNeeded())
}

func (m *Model) runChecks(h config.Host, add bool) tea.Cmd {
	ch := make(chan checkMsg, 8)
	hl, ctx := m.c.Hosts, m.ctx
	go func() {
		defer close(ch)
		send := func(msg checkMsg) {
			select {
			case ch <- msg:
			case <-ctx.Done():
			}
		}
		step := func(s hosts.Step) { send(checkMsg{name: h.Name, step: s}) }
		var checks []transport.Check
		var err error
		if add {
			checks, err = hl.Add(ctx, h, step)
		} else {
			checks = hl.Check(ctx, h, step)
		}
		send(checkMsg{name: h.Name, done: true, checks: checks, err: err})
	}()
	return waitCheck(ch)
}

func (m *Model) gotCheck(msg checkMsg) tea.Cmd {
	c := m.checks[msg.name]
	if c == nil {
		return nil
	}
	if !msg.done {
		c.step = msg.step
		return waitCheck(msg.ch)
	}
	c.running = false
	c.checks = msg.checks
	switch {
	case msg.err != nil:
		delete(m.checks, msg.name)
		m.setErr("add " + msg.name + ": " + msg.err.Error())
		return nil
	case c.failed():
		c.added = true
		m.setErr(msg.name + " " + c.reason())
	default:
		c.added = true
		m.sel.host = msg.name
		return tea.Batch(m.setNote(msg.name+": every check passed"), m.read())
	}
	return m.read()
}
