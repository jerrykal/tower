package ui

import (
	"github.com/charmbracelet/x/ansi"
)

// style is how a cell looks. Styles are written as plain SGR sequences in
// true colour; Bubble Tea's renderer maps them to the terminal's colour
// profile. (Lip Gloss is not used: its package-level writer detects a
// colour profile as the process starts, which inside tmux runs `tmux
// info` through PATH, in every tower process whose stdout is a terminal.)
type style struct {
	fg, bg                                ansi.Color
	bold, dim, italic, underline, reverse bool
}

func fg(c ansi.Color) style { return style{fg: c} }

func (s style) Bold() style           { s.bold = true; return s }
func (s style) Underline() style      { s.underline = true; return s }
func (s style) On(c ansi.Color) style { s.bg = c; return s }

// sgr is the sequence that sets s from a reset.
func (s style) sgr() string {
	st := ansi.Style{}.Reset()
	if s.fg != nil {
		st = st.ForegroundColor(s.fg)
	}
	if s.bg != nil {
		st = st.BackgroundColor(s.bg)
	}
	if s.bold {
		st = st.Bold()
	}
	if s.dim {
		st = st.Faint()
	}
	if s.italic {
		st = st.Italic(true)
	}
	if s.underline {
		st = st.Underline(true)
	}
	if s.reverse {
		st = st.Reverse(true)
	}
	return st.String()
}

// Render styles t, resetting after it.
func (s style) Render(t string) string {
	if t == "" {
		return ""
	}
	return s.sgr() + t + "\x1b[0m"
}

// Rosé Pine (main).
var (
	cBase    = ansi.TrueColor(0x191724)
	cSurface = ansi.TrueColor(0x1f1d2e)
	cOverlay = ansi.TrueColor(0x26233a)
	cMuted   = ansi.TrueColor(0x6e6a86)
	cSubtle  = ansi.TrueColor(0x908caa)
	cText    = ansi.TrueColor(0xe0def4)
	cLove    = ansi.TrueColor(0xeb6f92)
	cGold    = ansi.TrueColor(0xf6c177)
	cRose    = ansi.TrueColor(0xebbcba)
	cPine    = ansi.TrueColor(0x31748f)
	cFoam    = ansi.TrueColor(0x9ccfd8)
	cIris    = ansi.TrueColor(0xc4a7e7)
	cHLMed   = ansi.TrueColor(0x403d52)
	cHLHigh  = ansi.TrueColor(0x524f67)
)

var (
	sPlain  = fg(cText)
	sMuted  = fg(cMuted)
	sSubtle = fg(cSubtle)
	sHit    = fg(cRose).Bold().Underline()
	sErr    = fg(cLove)
	sGold   = fg(cGold)
	sFoam   = fg(cFoam)
	sIris   = fg(cIris)
	sRose   = fg(cRose)
	sRule   = fg(cHLMed)
	sKey    = fg(cIris)
	sCap    = fg(cText).Bold().On(cOverlay) // a key in the footer: a keycap
)

// Glyphs (Nerd Font).
const (
	glyphCur      = "\U000f09df" // md-circle_small: attached here
	glyphBell     = "\U000f009e" // md-bell_ring
	glyphAct      = "\U000f0430" // md-pulse: output since last viewed
	glyphClients  = "\U000f037a" // md-monitor_multiple: other clients
	glyphGroup    = "\U000f0339" // md-link_variant: a grouped session
	glyphBranch   = "\U000f062c" // md-source_branch
	glyphWindow   = "\U000f04e9" // md-tab
	glyphSession  = ""          // cod-terminal_tmux
	glyphDir      = ""          // fa-folder_o
	glyphSplit    = ""          // cod-split_horizontal: panes
	glyphLink     = "\U000f0318" // md-lan_connect
	glyphUnlink   = "\U000f0319" // md-lan_disconnect
	glyphApple    = ""
	glyphLinux    = ""
	glyphWindows  = ""
	glyphServer   = "\U000f048b" // md-server: an OS not known yet
	glyphPillL    = ""
	glyphPillR    = ""
	glyphBar      = "▌"
	glyphMore     = "…"
	glyphPrev     = "-"
	glyphWarn     = "!"
	glyphRefused  = "✗"
	glyphSubRow   = "└"
	glyphCrumbSep = "›"
	glyphSearch   = "\U000f0349" // md-magnify
)

// spinner frames, one per spinTick.
var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// osGlyph is the logo of a host's OS (uname -s).
func osGlyph(os string) string {
	switch {
	case os == "Darwin":
		return glyphApple
	case os == "Linux":
		return glyphLinux
	case len(os) >= 5 && (os[:5] == "MINGW" || os[:5] == "MSYS_" || os[:5] == "CYGWI") || os == "Windows":
		return glyphWindows
	}
	return glyphServer
}
