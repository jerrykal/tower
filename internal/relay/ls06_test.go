package relay

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// standbyScript plays the attach shim as a standby on the session's pty:
// echo off, the ready marker, one go line read, the pty fully raw, the go
// line printed back, the go marker, then cat in place of tmux, so the
// terminal's input comes back as the session's output. (The pty is made
// raw before the go marker, as the shim restores the pty's modes before
// its marker, so nothing typed after the marker meets a cooked pty.)
const standbyScript = `stty -echo
printf '\033]7193;tower-standby-ready\007'
IFS= read -r line
stty raw -echo -isig -icanon -iexten -icrnl -inlcr -igncr -istrip -ixon -opost cs8 -parenb
printf '%s' "$line"
printf '\033]7193;tower-standby-go\007'
exec cat`

// goLine is the go line the tests give a standby.
const goLine = `{"s":"$0"}`

// startStandby runs standbyScript, takes it through ready and go, and
// returns it with a terminal to relay it onto.
func startStandby(t testing.TB) (*Session, *testTerminal) {
	t.Helper()
	s := startSession(t, sh(standbyScript), nil, cookedModes(t))
	if before, err := s.ReadUntil([]byte(MarkerReady), 5*time.Second); err != nil || len(before) != 0 {
		t.Fatalf("ready: %q %v", before, err)
	}
	if err := s.Send([]byte(goLine)); err != nil {
		t.Fatal(err)
	}
	if before, err := s.ReadUntil([]byte(MarkerGo), 5*time.Second); err != nil || string(before) != goLine {
		t.Fatalf("go: %q %v", before, err)
	}
	return s, newTestTerminal(t, 24, 80)
}

// LS06: the relay is byte-exact both ways, for everything a terminal and
// the programs in it say to each other.
func TestLS06RelayFidelity(t *testing.T) {
	s, tt := startStandby(t)
	c := collect(tt)
	ch := relay(s, tt.Terminal)

	r := rand.New(rand.NewPCG(6, 6))
	clip := make([]byte, 4800)
	for i := range clip {
		clip[i] = byte(r.IntN(256))
	}
	osc52 := base64.StdEncoding.EncodeToString(clip)
	if len(osc52) != 6400 {
		t.Fatalf("clip of %d", len(osc52))
	}
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	vectors := []struct{ name, b string }{
		{"DA1 and DA2 replies", "\x1b[?62;22;52c\x1b[>1;10;0c"},
		{"OSC 11 query", "\x1b]11;?\x1b\\"},
		{"OSC 11 reply ST", "\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\"},
		{"OSC 11 reply BEL", "\x1b]11;rgb:1e1e/1e1e/2e2e\a"},
		{"kitty keyboard query", "\x1b[?u"},
		{"kitty keyboard reply", "\x1b[?1u"},
		{"kitty CSI u keys", "\x1b[97;5u\x1b[13;2u\x1b[57399u\x1b[1;5A\x1b[27;3;9u"},
		{"kitty push and pop", "\x1b[>1u\x1b[<u"},
		{"bracketed paste", "\x1b[?2004h" + "\x1b[200~pasted\r\nline\twith tab and \x1b[A arrow\x1b[201~" + "\x1b[?2004l"},
		{"SGR mouse", "\x1b[<0;10;5M\x1b[<0;10;5m\x1b[<64;3;4M\x1b[<65;3;4M\x1b[<2;200;100M\x1b[<2;200;100m"},
		{"X10 mouse high bytes", "\x1b[M \xff\xfe\x1b[M#\xfd\xff\x1b[M`\xfe\xfd"},
		{"focus", "\x1b[?1004h\x1b[I\x1b[O"},
		{"OSC 52 set", "\x1b]52;c;" + osc52 + "\a"},
		{"OSC 52 query", "\x1b]52;c;?\a"},
		{"DEC 2026", "\x1b[?2026h\x1b[?2026l"},
		{"DECRQM 2026", "\x1b[?2026$p"},
		{"DECRQM 2026 reply", "\x1b[?2026;2$y"},
		{"UTF-8", "中文字符 日本語 한국어 😀🎉 👨‍👩‍👧‍👦 é 🏳️‍🌈"},
		{"every byte", string(all)},
	}
	for _, v := range vectors {
		go func() {
			if err := writeAll(tt.mfd, []byte(v.b)); err != nil {
				t.Errorf("%s: typing: %v", v.name, err)
			}
		}()
		if got := c.exactly(t, len(v.b), 5*time.Second); string(got) != v.b {
			t.Fatalf("%s: got\n%q\nwant\n%q", v.name, got, v.b)
		}
	}
	s.Terminate()
	if res := <-ch; res.err != nil {
		t.Fatal(res.err)
	}
}

// LS06: the loop's holds and releases go in between the relayed output's
// sequences and characters, never inside one, and the output arrives
// otherwise untouched.
func TestLS06FrameWrites(t *testing.T) {
	r := rand.New(rand.NewPCG(60, 6))
	stream := makeStream(r, 512<<10)
	if bytes.Contains(stream, []byte(SyncBegin)) || bytes.Contains(stream, []byte(SyncEnd)) {
		t.Fatal("the stream holds what the check strips")
	}
	file := filepath.Join(t.TempDir(), "stream")
	if err := os.WriteFile(file, stream, 0o600); err != nil {
		t.Fatal(err)
	}
	argv, env := helperArgv("pieces", "FILE="+file, "SEED=7", "PACE=100")
	s := startSession(t, argv, env, cookedModes(t))
	tt := newTestTerminal(t, 24, 80)
	c := collect(tt)
	ch := relay(s, tt.Terminal)

	const holds = 200
	time.Sleep(20 * time.Millisecond) // the helper starting
	for range holds {
		n := tt.Hold()
		spin(time.Duration(50+r.IntN(200)) * time.Microsecond)
		tt.Release(n)
		spin(time.Duration(50+r.IntN(200)) * time.Microsecond)
	}
	if res := <-ch; res.err != nil || res.code != 0 {
		t.Fatalf("relay: %+v", res)
	}
	got := c.exactly(t, len(stream)+2*holds*len(SyncBegin), 10*time.Second)

	var p refParser
	var rest []byte
	syncs, inside := 0, 0
	for i := 0; i < len(got); {
		if bytes.HasPrefix(got[i:], []byte(SyncBegin)) || bytes.HasPrefix(got[i:], []byte(SyncEnd)) {
			syncs++
			if !p.idle() {
				inside++
			}
			i += len(SyncBegin)
			continue
		}
		p.put(got[i])
		rest = append(rest, got[i])
		i++
	}
	if !bytes.Equal(rest, stream) {
		i := 0
		for i < min(len(rest), len(stream)) && rest[i] == stream[i] {
			i++
		}
		t.Fatalf("the output differs from the stream at %d of %d (got %d bytes)", i, len(stream), len(rest))
	}
	if syncs != 2*holds || inside != 0 {
		t.Fatalf("%d sync sequences arrived, %d inside a sequence; want %d, none inside", syncs, inside, 2*holds)
	}
	t.Logf("%d bytes, %d frame writes: %d at once, %d after waiting for a sequence to end, %d forced",
		len(stream), syncs, tt.placed.Load(), tt.waited.Load(), tt.forced.Load())
	if tt.waited.Load() == 0 {
		t.Fatal("no write waited for a sequence: the test did not exercise placement")
	}
}

// makeStream is about n bytes of what programs write to a terminal.
func makeStream(r *rand.Rand, n int) []byte {
	var b bytes.Buffer
	letters := "abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ 0123456789.,:;-_/"
	cjk := []string{"中文字符", "測試", "日本語の文", "한국어", "漢字かな交じり"}
	emoji := []string{"😀", "🎉", "👍🏽", "👨‍👩‍👧", "🏳️‍🌈", "❤️"}
	clip := make([]byte, 900)
	for b.Len() < n {
		switch r.IntN(24) {
		case 0, 1, 2, 3, 4:
			for range 1 + r.IntN(80) {
				b.WriteByte(letters[r.IntN(len(letters))])
			}
		case 5, 6:
			fmt.Fprintf(&b, "\x1b[%d;38;5;%d;48;2;%d;%d;%dm", r.IntN(10), r.IntN(256), r.IntN(256), r.IntN(256), r.IntN(256))
		case 7:
			b.WriteString("\x1b[m")
		case 8, 9:
			switch r.IntN(4) {
			case 0:
				fmt.Fprintf(&b, "\x1b[%d;%dH", 1+r.IntN(50), 1+r.IntN(200))
			case 1:
				fmt.Fprintf(&b, "\x1b[%dA", 1+r.IntN(9))
			case 2:
				b.WriteString("\x1b[K")
			case 3:
				fmt.Fprintf(&b, "\x1b[%dG", 1+r.IntN(200))
			}
		case 10:
			for i := range clip {
				clip[i] = byte(r.IntN(256))
			}
			b.WriteString("\x1b]52;c;" + base64.StdEncoding.EncodeToString(clip) + "\a")
		case 11:
			if r.IntN(2) == 0 {
				b.WriteString("\x1bP+q544e;636f6c6f7273\x1b\\")
			} else {
				b.WriteString("\x1bPq#0;2;0;0;0#1;2;100;100;0#1~~@@vv@@~~$-#0??}}GG}}??\x1b\\")
			}
		case 12:
			b.WriteString("\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\")
		case 13, 14:
			b.WriteString(cjk[r.IntN(len(cjk))])
		case 15, 16:
			b.WriteString(emoji[r.IntN(len(emoji))])
		case 17:
			b.WriteString("\x1b(B")
		case 18:
			b.WriteString("\x1b7\x1b8")
		case 19, 20:
			b.WriteString("\r\n")
		case 21:
			b.WriteString("\x1b]0;" + strings.Repeat("title ", 1+r.IntN(8)) + "\x1b\\")
		case 22:
			b.WriteString("\x1b]8;;https://example.com/" + strings.Repeat("x", r.IntN(40)) + "\alink\x1b]8;;\a")
		case 23:
			b.WriteString("\t\b")
		}
	}
	return b.Bytes()
}

// refParser is a second, plainer model of a terminal's parser for the
// checks: whether a byte stream stands between sequences and characters.
type refParser struct {
	esc, csi, inter bool
	str             byte // the string's introducer (']', 'P', 'X', '^', '_'), 0 outside one
	strEsc          bool
	more            int // UTF-8 continuation bytes to come
}

func (p *refParser) idle() bool {
	return !p.esc && !p.csi && !p.inter && p.str == 0 && p.more == 0
}

func (p *refParser) put(b byte) {
	if b == 0x18 || b == 0x1a {
		*p = refParser{}
		return
	}
	switch {
	case p.str != 0:
		switch {
		case p.strEsc && b == '\\':
			*p = refParser{}
		case p.strEsc:
			*p = refParser{esc: true}
			p.put(b)
		case b == 0x1b:
			p.strEsc = true
		case b == 0x07 && p.str == ']':
			*p = refParser{}
		}
	case p.csi:
		if b == 0x1b {
			*p = refParser{esc: true}
		} else if b >= 0x40 && b <= 0x7e {
			*p = refParser{}
		} else if b >= 0x80 {
			*p = refParser{}
			p.put(b)
		}
	case p.inter:
		if b == 0x1b {
			*p = refParser{esc: true}
		} else if b >= 0x30 && b <= 0x7e {
			*p = refParser{}
		} else if b >= 0x80 {
			*p = refParser{}
			p.put(b)
		}
	case p.esc:
		switch {
		case b == '[':
			*p = refParser{csi: true}
		case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
			*p = refParser{str: b}
		case b >= 0x20 && b <= 0x2f:
			*p = refParser{inter: true}
		case b >= 0x30 && b <= 0x7e:
			*p = refParser{}
		case b >= 0x80:
			*p = refParser{}
			p.put(b)
		}
	case p.more > 0:
		if b>>6 == 2 {
			p.more--
			return
		}
		p.more = 0
		p.put(b)
	case b == 0x1b:
		p.esc = true
	case b>>5 == 6:
		p.more = 1
	case b>>4 == 14:
		p.more = 2
	case b>>3 == 30:
		p.more = 3
	}
}
