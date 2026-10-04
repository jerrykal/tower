package ui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/hosts"
	"github.com/jerrykal/tower/internal/proto"
)

// Prompts (a name) and confirmations (y/n). Both return to the mode they
// were opened from.

type promptKind int

const (
	pRename     promptKind = iota // a session or window
	pNewSession                   // in ~
	pNewWindow                    // in the session's directory
	pDirName                      // a session for a dir, named, then attached
	pHostName                     // a host being added
	pHostRename
)

type prompt struct {
	kind  promptKind
	pill  string // NEW SESSION, RENAME, HOST NAME
	about string // what it acts on: "B:bravo", "on B in ~"
	in    input
	hint  string // what an empty ⏎ takes: "⏎ for box"
	err   string // why the last ⏎ was refused
	it    item   // the row it acts on
	names bool   // a session name: '.' and ':' become '_'
	ssh   string // a host being added: its ssh target
}

func (p *prompt) typeText(s string) {
	if p.names {
		s = tmuxName(s)
	}
	p.in.insert(s)
	p.err = ""
}

// ask opens prompt p with text prefilled.
func (m *Model) ask(p *prompt, text string) tea.Cmd {
	p.in.set(text)
	if m.mode != modePrompt && m.mode != modeConfirm && m.mode != modeAddHost {
		m.back = m.mode
	}
	m.prompt, m.mode = p, modePrompt
	return nil
}

func (m *Model) endPrompt() {
	m.prompt = nil
	m.mode = m.back
}

func (m *Model) promptKey(k tea.KeyPressMsg) tea.Cmd {
	p := m.prompt
	s := k.String()
	switch s {
	case "esc", "ctrl+c":
		m.endPrompt()
		return nil
	case "enter":
		return m.runPrompt(p)
	}
	if p.in.edit(s, true) {
		p.err = ""
		return nil
	}
	if t := typed(k); t != "" {
		p.typeText(t)
	}
	return nil
}

// askNew is n: a session on the selected host (in ~), or, in the windows
// column, a window in the selected session (in its directory).
func (m *Model) askNew() tea.Cmd {
	if m.focus == colWindows {
		e := m.curEntry()
		if e == nil || e.kind != kSession {
			return nil
		}
		if err := reachable(e.host); err != nil {
			m.setErr(err.Error())
			return nil
		}
		return m.ask(&prompt{kind: pNewWindow, pill: "NEW WINDOW", about: "in " + e.host.Name + ":" + e.sess.Name, it: *e,
			hint: "⏎ for tmux's name"}, "")
	}
	h := m.curHost()
	if h == nil {
		return nil
	}
	if err := reachable(h.host); err != nil {
		m.setErr(err.Error())
		return nil
	}
	return m.ask(&prompt{kind: pNewSession, pill: "NEW SESSION", about: "on " + h.host.Name + " in ~", it: *h, names: true,
		hint: "⏎ for tmux's number"}, "")
}

// askRename is r: a session or window's new name; a dir's session under a
// chosen name; a host's label.
func (m *Model) askRename() tea.Cmd {
	it := m.target()
	if it == nil {
		return nil
	}
	if it.kind == kHost {
		if why := m.hostEditable(it); why != "" {
			m.setErr(why)
			return nil
		}
		return m.ask(&prompt{kind: pHostRename, pill: "HOST NAME", about: "for " + it.host.Name, it: *it}, it.host.Name)
	}
	if err := reachable(it.host); err != nil {
		m.setErr(err.Error())
		return nil
	}
	switch it.kind {
	case kDir:
		return m.ask(&prompt{kind: pDirName, pill: "NEW SESSION", about: "in " + it.dir.Path, it: *it, names: true},
			freeName(it.host, dirSessionName(it.dir.Path)))
	case kWindow:
		return m.ask(&prompt{kind: pRename, pill: "RENAME", about: "window " + it.host.Name + ":" + it.sess.Name + ":" + winLabel(it), it: *it}, it.win.Name)
	}
	return m.ask(&prompt{kind: pRename, pill: "RENAME", about: "session " + it.host.Name + ":" + it.sess.Name, it: *it, names: true}, it.sess.Name)
}

// sessionTaken reports whether host h (as the view has it now) has a
// session named name other than except.
func (m *Model) sessionTaken(hostID, name, except string) bool {
	h := m.view.View.HostByID(hostID)
	if h == nil {
		return false
	}
	for _, s := range h.Sessions {
		if s.Name == name && s.ID != except {
			return true
		}
	}
	return false
}

// runPrompt applies a prompt's ⏎: a name that is refused keeps it open
// with the reason.
func (m *Model) runPrompt(p *prompt) tea.Cmd {
	text := strings.TrimSpace(p.in.String())
	it := &p.it
	switch p.kind {
	case pHostName, pHostRename:
		return m.runHostPrompt(p, text)
	case pRename:
		if text == "" {
			p.err = "a name is needed"
			return nil
		}
		if it.kind == kSession && m.sessionTaken(it.host.ID, text, it.sess.ID) {
			p.err = text + " is taken on " + it.host.Name
			return nil
		}
		m.endPrompt()
		t, host := rowTarget(it), it.host.Name
		m.setBusy("rename on " + host + "…")
		c, ctx := m.c, m.ctx
		return func() tea.Msg {
			ack, err := c.rename(ctx, t, text)
			return actMsg{op: "rename", host: host, ack: ack, err: err}
		}
	case pNewSession, pDirName:
		if text != "" && m.sessionTaken(it.host.ID, text, "") {
			p.err = text + " is taken on " + it.host.Name
			return nil
		}
		m.endPrompt()
		if p.kind == pDirName {
			if text == "" {
				text = freeName(it.host, dirSessionName(it.dir.Path))
			}
			return m.makeIn(it, text)
		}
		h := *it.host
		if n := startingNote(&h); n != "" {
			m.setBusy(n)
		} else {
			m.setBusy("new on " + h.Name + "…")
		}
		c, ctx := m.c, m.ctx
		return func() tea.Msg {
			ack, err := c.newSession(ctx, &h, text, "")
			return actMsg{op: "new", host: h.Name, ack: ack, err: err, then: selectMade}
		}
	case pNewWindow:
		m.endPrompt()
		t, host := rowTarget(it), it.host.Name
		dir := it.sess.Path
		m.setBusy("new window on " + host + "…")
		c, ctx := m.c, m.ctx
		return func() tea.Msg {
			ack, err := c.newWindow(ctx, t.ref, text, dir)
			return actMsg{op: "new window", host: host, ack: ack, err: err, then: selectMade}
		}
	}
	m.endPrompt()
	return nil
}

// selectMade selects what a new session or window made, once a view read
// has it; a new session shows "new" until it is attached.
func selectMade(m *Model, ack proto.Ack) tea.Cmd {
	if ack.Ref == nil {
		return nil
	}
	k := refKey(*ack.Ref)
	if k.Window == "" {
		m.fresh[k] = true
	}
	m.want, m.wantReads = &k, 0
	return nil
}

// --- confirm ---

// confirm is a y/n question with what is at stake.
type confirm struct {
	question string   // "kill session B:bravo?"
	stakes   []string // "2 windows · 3 panes", "detaches 1 other client"
	running  []string // non-shell commands in its panes
	panesFor rowKey   // the row whose panes were asked for
	yes      func() tea.Cmd
}

// text is the question with its stakes.
func (c *confirm) text() string {
	parts := append([]string(nil), c.stakes...)
	if n := len(c.running); n > 0 {
		r := strings.Join(c.running[:min(n, 2)], ", ")
		if n > 2 {
			r += " +" + strconv.Itoa(n-2)
		}
		parts = append(parts, r+" running")
	}
	if len(parts) == 0 {
		return c.question
	}
	return c.question + " " + strings.Join(parts, " · ")
}

func (m *Model) openConfirm(c *confirm) {
	if m.mode != modePrompt && m.mode != modeConfirm && m.mode != modeAddHost {
		m.back = m.mode
	}
	m.confirm, m.mode = c, modeConfirm
}

func (m *Model) confirmKey(k tea.KeyPressMsg) tea.Cmd {
	c := m.confirm
	m.confirm = nil
	m.mode = m.back
	if k.String() == "y" {
		return c.yes()
	}
	return nil
}

// askKill is x and ^x: kill a session or window, or take a host off the
// list, after y.
func (m *Model) askKill() tea.Cmd {
	it := m.target()
	if it == nil {
		return nil
	}
	switch it.kind {
	case kHost:
		return m.askRemoveHost(it)
	case kDir:
		m.setErr("a dir has no session to kill")
		return nil
	}
	if err := reachable(it.host); err != nil {
		m.setErr(err.Error())
		return nil
	}
	c := &confirm{yes: func() tea.Cmd { return m.killKey(it.key) }, panesFor: it.key}
	if it.kind == kWindow {
		c.question = "kill window " + it.host.Name + ":" + it.sess.Name + ":" + winLabel(it) + "?"
		c.stakes = append(c.stakes, plural(max(it.win.Panes, 1), "pane"))
		if len(m.w.windows[it.key.sessionKey()]) <= 1 {
			if peers := m.w.groupPeers(it); len(peers) > 0 {
				c.stakes = append(c.stakes, "its last window: its group goes too ("+strings.Join(append([]string{it.sess.Name}, peers...), ", ")+")")
			} else {
				c.stakes = append(c.stakes, "its last window: the session goes too")
			}
		}
	} else {
		c.question = "kill session " + it.host.Name + ":" + it.sess.Name + "?"
		panes := 0
		for _, w := range it.sess.Windows {
			panes += max(w.Panes, 1)
		}
		c.stakes = append(c.stakes, plural(len(it.sess.Windows), "window")+" · "+plural(panes, "pane"))
		if it.others > 0 {
			c.stakes = append(c.stakes, "detaches "+plural(it.others, "other client"))
		}
		if peers := m.w.groupPeers(it); len(peers) > 0 {
			c.stakes = append(c.stakes, "grouped: its windows stay with "+peers[0])
		}
		if it.cur {
			c.stakes = append(c.stakes, "you are attached to it")
		}
	}
	m.openConfirm(c)
	return m.askPanes(it)
}

// killKey kills the row k after the confirm, if it is still there.
func (m *Model) killKey(k rowKey) tea.Cmd {
	it := m.w.find(k)
	if it == nil {
		m.setErr(errGone.Error())
		return nil
	}
	return m.kill(it)
}

// askRemoveHost confirms taking a host off the list.
func (m *Model) askRemoveHost(h *item) tea.Cmd {
	if why := m.hostEditable(h); why != "" {
		m.setErr(why)
		return nil
	}
	name := h.host.Name
	c := &confirm{question: "remove host " + name + " from the list?", stakes: []string{"its sessions keep running"}}
	if h.cur {
		c.stakes = append(c.stakes, "you stay attached until you switch")
	}
	c.yes = func() tea.Cmd {
		return m.editHost("removed "+name, func(l HostList) error { return l.Remove(name) })
	}
	m.openConfirm(c)
	return nil
}

// panesMsg carries the panes of a session or window, for a confirm.
type panesMsg struct {
	key   rowKey
	panes []proto.Pane
	err   error
}

// askPanes asks towerd what runs in it's panes, so the question can say.
func (m *Model) askPanes(it *item) tea.Cmd {
	t := rowTarget(it)
	kind := proto.KindSession
	if it.kind == kWindow {
		kind = proto.KindWindow
	}
	c, ctx, key := m.c, m.ctx, it.key
	return func() tea.Msg {
		ps, err := c.panes(ctx, t.ref, kind)
		return panesMsg{key: key, panes: ps, err: err}
	}
}

func (m *Model) gotPanes(msg panesMsg) {
	if m.confirm == nil || m.confirm.panesFor != msg.key || msg.err != nil {
		return
	}
	m.confirm.running = running(msg.panes)
}

// --- host names ---

// runHostPrompt applies a host name: the rules of hosts.CheckName; an
// empty name takes the default shown.
func (m *Model) runHostPrompt(p *prompt, text string) tea.Cmd {
	hl := m.c.Hosts
	list, err := hl.Load()
	if err != nil {
		p.err = err.Error()
		return nil
	}
	aliases := hl.Aliases()
	name := hosts.Clean(text)
	if p.kind == pHostRename {
		i := hosts.Find(list, p.it.host.Name)
		if i < 0 {
			p.err = "no host named " + p.it.host.Name + " in the list"
			return nil
		}
		if err := hosts.CheckName(list, aliases, name, list[i].Target(), i); err != nil {
			p.err = err.Error()
			return nil
		}
		m.endPrompt()
		old := p.it.host.Name
		m.renameOrder(old, name)
		return m.editHost("renamed "+old+" to "+name, func(l HostList) error { return l.Rename(old, name) })
	}
	if name == "" {
		name = hosts.FreeName(list, aliases, p.ssh)
	}
	if err := hosts.CheckName(list, aliases, name, p.ssh, -1); err != nil {
		p.err = err.Error()
		return nil
	}
	m.endPrompt()
	return m.addHost(config.Host{Name: name, SSH: p.ssh})
}

// renameOrder keeps a renamed host in its place.
func (m *Model) renameOrder(old, name string) {
	for i, n := range m.order {
		if n == old {
			m.order[i] = name
		}
	}
	if m.sel.host == old {
		m.sel.host = name
	}
	if k, ok := m.sel.entry[old]; ok {
		m.sel.entry[name] = k
	}
}
