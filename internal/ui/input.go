package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// input is a line of text with a text cursor (a rune index): a column's
// query, the finder's, a prompt's, the add-host picker's.
type input struct {
	text []rune
	cur  int
}

func (in *input) String() string { return string(in.text) }

func (in *input) set(s string) {
	in.text = []rune(s)
	in.cur = len(in.text)
}

// insert puts s at the cursor.
func (in *input) insert(s string) {
	rs := []rune(s)
	in.cur = min(max(in.cur, 0), len(in.text))
	t := make([]rune, 0, len(in.text)+len(rs))
	t = append(t, in.text[:in.cur]...)
	t = append(t, rs...)
	in.text = append(t, in.text[in.cur:]...)
	in.cur += len(rs)
}

// edit applies an editing key and reports whether it was one: ← → ^b
// ^f ^a ^e move the cursor (when moves), ⌫ ^d ^u delete back, forward
// and to the start.
func (in *input) edit(k string, moves bool) bool {
	in.cur = min(max(in.cur, 0), len(in.text))
	switch k {
	case "backspace":
		if in.cur > 0 {
			in.text = append(in.text[:in.cur-1:in.cur-1], in.text[in.cur:]...)
			in.cur--
		}
	case "ctrl+u":
		in.text = append([]rune(nil), in.text[in.cur:]...)
		in.cur = 0
	case "delete", "ctrl+d":
		if !moves {
			return false
		}
		if in.cur < len(in.text) {
			in.text = append(in.text[:in.cur:in.cur], in.text[in.cur+1:]...)
		}
	case "left", "ctrl+b":
		if !moves {
			return false
		}
		in.cur = max(in.cur-1, 0)
	case "right", "ctrl+f":
		if !moves {
			return false
		}
		in.cur = min(in.cur+1, len(in.text))
	case "home", "ctrl+a":
		if !moves {
			return false
		}
		in.cur = 0
	case "end", "ctrl+e":
		if !moves {
			return false
		}
		in.cur = len(in.text)
	default:
		return false
	}
	return true
}

// typed is the text a key press types, or "" for a key that types
// nothing (a control key, alt, a named key).
func typed(k tea.KeyPressMsg) string {
	if k.Text == "" || k.Mod&(tea.ModCtrl|tea.ModAlt) != 0 {
		return ""
	}
	return k.Text
}

// oneLine is pasted text as one line: line breaks as spaces.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// tmuxName turns what tmux forbids in a session name into '_'.
func tmuxName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '.' || r == ':' {
			return '_'
		}
		return r
	}, s)
}
