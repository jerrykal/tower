package tmux

import "strings"

// Quote makes s one argument in tmux's command language. Inside single
// quotes tmux has no escapes, so a single quote closes the string, is
// written escaped, and the string reopens; tmux joins the adjacent parts
// into one argument.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Literal doubles every # so tmux's format expansion leaves s as it is.
// Commands that expand formats in their arguments (rename-session,
// new-session -s, new-window -n, display-message) need it for names.
func Literal(s string) string {
	return strings.ReplaceAll(s, "#", "##")
}

// Arg is Quote(Literal(s)): a name for a command that expands formats.
func Arg(s string) string { return Quote(Literal(s)) }
