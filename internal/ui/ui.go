// Package ui is the dashboard: tower's own terminal UI. It runs in a tmux
// popup for the client that pressed the key (Run), on the attach loop's
// terminal as its picker (Pick), and without a terminal for scripts and
// the scenario suite (Script). All three share one model and one set of
// actions, and reach towerd only through the client package.
package ui

import (
	"context"
	"errors"
	"io"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// ErrQuit is the picker closed without a choice.
var ErrQuit = errors.New("quit")

// fps is the renderer's frame rate: Bubble Tea draws on a ticker, so this
// bounds the delay from a key to its echo.
const fps = 120

// Run is the dashboard for the client c.Client, on this process's
// terminal (the popup). It returns when the dashboard closes: esc, ^c,
// or after ⏎ did its work.
func Run(ctx context.Context, c *Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := run(ctx, c, runOptions{})
	return err
}

// PickOptions are the loop's picker's.
type PickOptions struct {
	Loop string // the loop's id: its view marks current and previous
	Note string // why the picker is up ("B restarted since it was listed; pick again")
	Dash bool   // tower dash: esc means "attach to the last target"

	// The terminal; nil means stdin and stdout.
	Input  io.Reader
	Output io.Writer
}

// Choice is what the picker chose.
type Choice struct {
	Target proto.Ref
	Last   bool // esc in tower dash: the loop attaches to its last target
}

// Pick runs the picker on the loop's terminal and returns what ⏎ chose:
// a target on any host, which the loop prepares. esc returns ErrQuit, or
// a Choice with Last in tower dash; ^c with an empty query returns
// ErrQuit.
func Pick(ctx context.Context, c *client.Client, o PickOptions) (Choice, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn := &Conn{Towerd: Calls{C: c}, Loop: o.Loop, Pick: true}
	m, err := run(ctx, conn, runOptions{note: o.Note, dash: o.Dash, in: o.Input, out: o.Output})
	if err != nil {
		return Choice{}, err
	}
	switch {
	case m.choice != nil:
		return Choice{Target: *m.choice}, nil
	case m.last:
		return Choice{Last: true}, nil
	}
	return Choice{}, ErrQuit
}

type runOptions struct {
	note string
	dash bool
	in   io.Reader
	out  io.Writer
	size [2]int // tests: the terminal's size
}

// run reads the view, so the first frame has the rows, and runs the
// program until it quits.
func run(ctx context.Context, c *Conn, o runOptions) (*Model, error) {
	config.Mark("dash: connected")
	d, err := c.Towerd.View(ctx, c.viewArgs())
	config.Mark("dash: view")
	m := newModel(ctx, c, d, err == nil, config.Flag("TOWER_LIVE", true))
	m.dash = o.dash
	if err != nil {
		m.setErr("towerd: " + err.Error())
	} else if o.note != "" {
		m.setErr(o.note)
	}
	m.firstDraw = func() { config.Mark("dash: frame") }
	opts := []tea.ProgramOption{tea.WithContext(ctx), tea.WithFPS(fps)}
	if os.Getenv("TMUX") != "" {
		// Inside tmux, Bubble Tea's colour detection runs `tmux info`
		// through PATH (a version manager's shim costs 60-80ms) on the
		// popup's start. tmux maps RGB colours to what the terminal
		// outside it supports, so true colour is right here.
		p := colorprofile.TrueColor
		if os.Getenv("NO_COLOR") != "" {
			p = colorprofile.Ascii
		}
		opts = append(opts, tea.WithColorProfile(p))
	}
	if o.in != nil {
		opts = append(opts, tea.WithInput(o.in))
	}
	if o.out != nil {
		opts = append(opts, tea.WithOutput(o.out))
	}
	switch {
	case o.size[0] > 0:
		m.width, m.height = o.size[0], o.size[1]
		opts = append(opts, tea.WithWindowSize(o.size[0], o.size[1]))
	case o.out == nil:
		// The first frame is drawn before Bubble Tea reports the size.
		if w, h, err := term.GetSize(os.Stdout.Fd()); err == nil {
			m.width, m.height = w, h
		}
	}
	m.scroll()
	if _, err := tea.NewProgram(m, opts...).Run(); err != nil && !m.quitted {
		if ctx.Err() != nil {
			return m, ctx.Err()
		}
		return m, err
	}
	return m, nil
}
