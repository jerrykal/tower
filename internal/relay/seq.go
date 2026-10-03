package relay

// tracker follows a terminal's parser over the bytes written to it, far
// enough to tell whether they end between whole escape sequences and
// characters: the only places the loop's own writes can go without
// changing what the relayed output means.
//
// It models a UTF-8 terminal after the DEC/ANSI parser terminals share
// (vt100.net's state machine): 8-bit C1 controls are not recognised, since
// in UTF-8 those bytes are parts of characters.
type tracker struct {
	st   seqState
	need uint8 // continuation bytes still to come in a UTF-8 character
	cp   rune  // the character decoded so far
}

type seqState uint8

const (
	sGround   seqState = iota // between sequences and characters
	sJoin                     // after a zero-width joiner: the next character joins its cluster
	sUTF8                     // inside a multi-byte character
	sEsc                      // after ESC
	sEscInter                 // ESC and intermediates (ESC ( B, ESC # 8)
	sCSI                      // ESC [ and its parameters
	sOSC                      // ESC ] …, ends with BEL or ST
	sStr                      // DCS, SOS, PM or APC (ESC P, X, ^, _), ends with ST
)

const (
	bBEL = 0x07
	bCAN = 0x18
	bSUB = 0x1a
	bESC = 0x1b
	bDEL = 0x7f
)

// zwj is U+200D ZERO WIDTH JOINER, which glues the characters either side
// of it into one cluster (emoji ZWJ sequences).
const zwj = 0x200d

// ground reports whether the bytes so far end at a boundary.
func (t *tracker) ground() bool { return t.st == sGround }

// inSequence reports whether the bytes so far end inside an escape
// sequence or a character; the terminal's parser then takes what comes
// next as part of it. A pending joiner does not count: a terminal parses
// what follows it normally.
func (t *tracker) inSequence() bool { return t.st != sGround && t.st != sJoin }

func (t *tracker) reset() { *t = tracker{} }

// feed follows all of p.
func (t *tracker) feed(p []byte) {
	// ESC, CAN and SUB leave a terminal's parser in the same state from
	// any state (an ESC inside a string ends it: ESC \ is ST, and any
	// other ESC starts a new sequence), so only what follows the last of
	// them counts. In terminal output that is a short tail.
	for k := len(p) - 1; k >= 0; k-- {
		if c := p[k]; c == bESC || c == bCAN || c == bSUB {
			*t = tracker{}
			if c == bESC {
				t.st = sEsc
			}
			p = p[k+1:]
			break
		}
	}
	for i := 0; i < len(p); {
		switch t.st {
		case sGround:
			i = skipText(p, i)
		case sOSC, sStr:
			// String contents run up to a control.
			for i < len(p) && p[i] >= 0x20 {
				i++
			}
		}
		if i < len(p) {
			t.step(p[i])
			i++
		}
	}
}

// skipText returns the index of the first byte from i on that may take
// the parser away from ground: ESC, a character cut off by the end of p
// or malformed, or a zero-width joiner. Text, C0 controls and whole
// characters leave it at ground.
func skipText(p []byte, i int) int {
	for i < len(p) {
		c := p[i]
		if c < 0x80 {
			if c == bESC {
				return i
			}
			i++
			continue
		}
		var n int
		switch {
		case c >= 0xc2 && c <= 0xdf:
			n = 2
		case c >= 0xe0 && c <= 0xef:
			n = 3
		case c >= 0xf0 && c <= 0xf4:
			n = 4
		default:
			i++ // a byte UTF-8 never starts with: one replacement character
			continue
		}
		if i+n > len(p) {
			return i
		}
		for _, b := range p[i+1 : i+n] {
			if b&0xc0 != 0x80 {
				return i
			}
		}
		if n == 3 && c == 0xe2 && p[i+1] == 0x80 && p[i+2] == 0x8d {
			return i // a joiner
		}
		i += n
	}
	return i
}

// feedToGround follows p up to the first boundary and returns how many
// bytes it took: 0 when already at one, len(p) when p ends before one.
func (t *tracker) feedToGround(p []byte) int {
	i := 0
	for i < len(p) && t.st != sGround {
		t.step(p[i])
		i++
	}
	return i
}

// step follows one byte.
func (t *tracker) step(b byte) {
	switch t.st {
	case sGround, sJoin:
		t.start(b)
	case sUTF8:
		if b&0xc0 != 0x80 {
			// A character cut short: the terminal ends it and takes b afresh.
			t.start(b)
			return
		}
		t.cp = t.cp<<6 | rune(b&0x3f)
		if t.need--; t.need == 0 {
			if t.cp == zwj {
				t.st = sJoin
			} else {
				t.st = sGround
			}
		}
	case sEsc:
		switch {
		case b == bCAN || b == bSUB:
			t.st = sGround
		case b < 0x20 || b == bDEL: // C0 controls act in passing; ESC ESC restarts
		case b < 0x30:
			t.st = sEscInter
		case b == '[':
			t.st = sCSI
		case b == ']':
			t.st = sOSC
		case b == 'P' || b == 'X' || b == '^' || b == '_':
			t.st = sStr
		case b < bDEL:
			t.st = sGround // final byte
		default:
			t.start(b)
		}
	case sEscInter:
		switch {
		case b == bESC:
			t.st = sEsc
		case b == bCAN || b == bSUB:
			t.st = sGround
		case b < 0x30 || b == bDEL: // controls in passing, more intermediates
		case b < bDEL:
			t.st = sGround
		default:
			t.start(b)
		}
	case sCSI:
		switch {
		case b == bESC:
			t.st = sEsc
		case b == bCAN || b == bSUB:
			t.st = sGround
		case b < 0x40 || b == bDEL: // controls in passing, parameters, intermediates
		case b < bDEL:
			t.st = sGround // final byte
		default:
			t.start(b)
		}
	case sOSC, sStr:
		switch b {
		case bESC:
			// ST, or the string cut off by a new sequence: either way the
			// byte after decides, as after any ESC.
			t.st = sEsc
		case bCAN, bSUB:
			t.st = sGround
		case bBEL:
			if t.st == sOSC {
				t.st = sGround
			}
		}
	}
}

// start follows b from a boundary.
func (t *tracker) start(b byte) {
	switch {
	case b == bESC:
		t.st = sEsc
	case b < 0x80:
		t.st = sGround
	case b >= 0xc2 && b <= 0xdf:
		t.st, t.need, t.cp = sUTF8, 1, rune(b&0x1f)
	case b >= 0xe0 && b <= 0xef:
		t.st, t.need, t.cp = sUTF8, 2, rune(b&0x0f)
	case b >= 0xf0 && b <= 0xf4:
		t.st, t.need, t.cp = sUTF8, 3, rune(b&0x07)
	default:
		// A stray continuation byte or one UTF-8 never uses: the terminal
		// shows a replacement character for it alone.
		t.st = sGround
	}
}
