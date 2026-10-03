package relay

import (
	"math/rand/v2"
	"testing"
)

func TestTrackerBoundaries(t *testing.T) {
	zwjSeq := "👨‍👩‍👧"
	cases := []struct {
		name string
		in   string
		at   bool // ends at a boundary
	}{
		{"empty", "", true},
		{"text", "hello, world\r\n", true},
		{"c0 controls", "\a\b\t\x00\x18\x1a", true},
		{"esc", "\x1b", false},
		{"esc esc", "\x1b\x1b", false},
		{"esc del", "\x1b\x7f", false},
		{"esc final", "\x1b7", true},
		{"save restore", "\x1b7\x1b8", true},
		{"keypad", "\x1b=", true},
		{"esc intermediate", "\x1b(", false},
		{"charset", "\x1b(B", true},
		{"decaln", "\x1b#8", true},
		{"esc can", "\x1b\x18", true},
		{"esc 8-bit", "\x1b\xc3", false}, // ends the ESC, starts a character
		{"csi", "\x1b[", false},
		{"csi params", "\x1b[38;5;123", false},
		{"sgr", "\x1b[38;5;123m", true},
		{"private mode", "\x1b[?2026h", true},
		{"decrqm", "\x1b[?2026$p", true},
		{"csi intermediates", "\x1b[0 q", true},
		{"csi kitty", "\x1b[>1u", true},
		{"csi control inside", "\x1b[1;2\r", false},
		{"csi control then final", "\x1b[1;2\rH", true},
		{"csi can", "\x1b[12\x18", true},
		{"csi sub", "\x1b[12\x1a", true},
		{"csi esc restarts", "\x1b[12\x1b", false},
		{"csi esc restarts done", "\x1b[12\x1b[m", true},
		{"csi 8-bit aborts", "\x1b[1\x80", true},
		{"osc", "\x1b]11;?", false},
		{"osc bel", "\x1b]11;?\a", true},
		{"osc st", "\x1b]11;rgb:0000/0000/0000\x1b\\", true},
		{"osc esc", "\x1b]0;title\x1b", false},
		{"osc utf8", "\x1b]0;中文", false},
		{"osc utf8 bel", "\x1b]0;中文\a", true},
		{"osc can", "\x1b]0;t\x18", true},
		{"osc other esc starts sequence", "\x1b]0;t\x1b[", false},
		{"osc other esc then sgr", "\x1b]0;t\x1b[m", true},
		{"osc then sos", "\x1b]0;t\x1bX", false},
		{"dcs", "\x1bPq#0;2;0;0;0", false},
		{"dcs bel does not end", "\x1bP+q544e\a", false},
		{"dcs st", "\x1bP+q544e\x1b\\", true},
		{"dcs sub", "\x1bPq\x1a", true},
		{"apc", "\x1b_Gf=24;AAAA", false},
		{"apc bel does not end", "\x1b_Gf=24;AAAA\a", false},
		{"apc st", "\x1b_Gf=24;AAAA\x1b\\", true},
		{"pm st", "\x1b^note\x1b\\", true},
		{"sos st", "\x1bXnote\x1b\\", true},
		{"utf8 2", "é", true},
		{"utf8 2 cut", "é"[:1], false},
		{"utf8 3", "中", true},
		{"utf8 3 cut", "中"[:2], false},
		{"utf8 4", "😀", true},
		{"utf8 4 cut", "😀"[:3], false},
		{"utf8 cut short", "\xe4\xb8A", true},
		{"utf8 cut by esc", "\xe4\xb8\x1b", false},
		{"stray continuation", "\x80\xbf", true},
		{"invalid lead", "\xc0\xc1\xf5\xff", true},
		{"zwj glues", "👨‍", false},
		{"zwj sequence", zwjSeq, true},
		{"zwj then ascii", "👨‍x", true},
		{"zwj then esc", "👨‍\x1b", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var a, b tracker
			a.feed([]byte(c.in))
			for i := 0; i < len(c.in); i++ {
				b.step(c.in[i])
			}
			if a.ground() != c.at || b.ground() != c.at {
				t.Fatalf("%q: feed ground=%v, step ground=%v, want %v", c.in, a.ground(), b.ground(), c.at)
			}
			if !a.same(b) {
				t.Fatalf("%q: feed %+v differs from step %+v", c.in, a, b)
			}
		})
	}
}

func TestTrackerInSequence(t *testing.T) {
	for in, want := range map[string]bool{
		"":            false,
		"a":           false,
		"👨‍":          false, // a joiner is no sequence the terminal is inside
		"\x1b":        true,
		"\x1b]0;t":    true,
		"中"[:1]:       true,
		"\x1b[31m":    false,
		"\x1bP\x1b\\": false,
	} {
		var tr tracker
		tr.feed([]byte(in))
		if tr.inSequence() != want {
			t.Errorf("%q: inSequence %v, want %v", in, tr.inSequence(), want)
		}
	}
}

func TestTrackerFeedToGround(t *testing.T) {
	var tr tracker
	if n := tr.feedToGround([]byte("abc")); n != 0 {
		t.Fatalf("at a boundary: took %d", n)
	}
	tr.feed([]byte("\x1b[3"))
	if n := tr.feedToGround([]byte("1mabc")); n != 2 || !tr.ground() {
		t.Fatalf("took %d (ground %v), want 2", n, tr.ground())
	}
	tr.feed([]byte("\x1b]52;c;"))
	if n := tr.feedToGround([]byte("QUJD")); n != 4 || tr.ground() {
		t.Fatalf("took %d (ground %v), want all 4 and inside", n, tr.ground())
	}
}

// same reports whether two trackers are in the same state (what is left
// of a character only counts inside one).
func (t tracker) same(o tracker) bool {
	if t.st == sUTF8 || o.st == sUTF8 {
		return t == o
	}
	return t.st == o.st
}

// TestTrackerFeedMatchesStep checks feed's fast paths against the plain
// byte-at-a-time state machine, on random mixes of sequences and bytes.
func TestTrackerFeedMatchesStep(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	atoms := []string{"a", "xyz ", "\r\n", "\x1b", "[", "]", "P", "_", "\\", "\a", "\x18",
		"\x1b[31m", "\x1b]0;", "中", "😀", "‍", "\x80", "\xff", ";", "m", "\x7f", "("}
	for range 2000 {
		var in []byte
		for range r.IntN(40) {
			if r.IntN(5) == 0 {
				in = append(in, byte(r.IntN(256)))
			} else {
				in = append(in, atoms[r.IntN(len(atoms))]...)
			}
		}
		var a, b tracker
		// Feed in random pieces, to cross every state at a cut.
		for p := in; len(p) > 0; {
			n := 1 + r.IntN(len(p))
			a.feed(p[:n])
			p = p[n:]
		}
		for _, c := range in {
			b.step(c)
		}
		if !a.same(b) {
			t.Fatalf("%q: feed %+v, step %+v", in, a, b)
		}
	}
}

func BenchmarkTrackerFeed(b *testing.B) {
	p := []byte(colouredLine(1) + colouredLine(2) + "中文字符测试😀 \x1b]52;c;QUJDREVGR0g=\a")
	for len(p) < 32<<10 {
		p = append(p, p...)
	}
	b.SetBytes(int64(len(p)))
	var tr tracker
	for b.Loop() {
		tr.feed(p)
	}
}

// Text without a single escape: every byte is followed.
func BenchmarkTrackerFeedPlainUTF8(b *testing.B) {
	var p []byte
	for len(p) < 32<<10 {
		p = append(p, "plain text, 中文字符 日本語の文 한국어 😀 and more\r\n"...)
	}
	b.SetBytes(int64(len(p)))
	var tr tracker
	for b.Loop() {
		tr.feed(p)
	}
}

func BenchmarkTrackerFeedWide(b *testing.B) {
	r := rand.New(rand.NewPCG(8, 8))
	words := []string{"中文字符", "日本語", "한국어", "😀", "👨‍👩‍👧", "漢字"}
	var p []byte
	for len(p) < 32<<10 {
		if r.IntN(3) == 0 {
			for range 1 + r.IntN(64) {
				p = append(p, byte(r.IntN(256)))
			}
		} else {
			p = append(p, words[r.IntN(len(words))]...)
		}
	}
	b.SetBytes(int64(len(p)))
	var tr tracker
	for b.Loop() {
		tr.feed(p)
	}
}
