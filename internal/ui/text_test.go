package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/jerrykal/tower/internal/proto"
)

func TestFit(t *testing.T) {
	gs := graphemes("sweep-lr-warmup-cosine-v2")
	if got := fitMiddle(gs, 20, -1).String(); got != "sweep-lr-w…cosine-v2" || width(got) != 20 {
		t.Errorf("middle: %q", got)
	}
	// A hit the cut would hide: the visible part slides to it.
	if got := fitMiddle(gs, 12, 15).String(); !strings.HasPrefix(got, "…") || !strings.Contains(got, "cos") || width(got) > 12 {
		t.Errorf("slide: %q", got)
	}
	if got := fitEnd(graphemes("train-llm"), 6).String(); got != "train…" {
		t.Errorf("end: %q", got)
	}
	// Wide characters count two cells.
	if got := fitMiddle(graphemes("日本語のセッション名"), 9, -1); got.width() > 9 {
		t.Errorf("wide: %q is %d cells", got.String(), got.width())
	}
	// A worktree's repo part shrinks first, then the branch's middle.
	wt := graphemes("dotfiles_feat-tower-prototype")
	for room, want := range map[int]string{
		29: "dotfiles_feat-tower-prototype",
		26: "dotf…_feat-tower-prototype",
		23: "d…_feat-tower-prototype",
		16: "d…_feat-t…totype",
	} {
		if got := fitWorktree(wt, 8, room, nil).String(); got != want {
			t.Errorf("worktree in %d: %q, want %q", room, got, want)
		}
	}
	if got := fitWorktree(wt, 8, 16, []int{0, 1}).String(); got != "dotfiles_feat-t…" {
		t.Errorf("worktree, hits in the repo part: %q", got)
	}
	// Paths shorten as fish's prompt does.
	p := "~/work/clients/acme/services/backend-api-gateway"
	for room, want := range map[int]string{
		60: p,
		35: "~/w/c/a/s/backend-api-gateway",
		25: "…/backend-api-gateway",
		12: "…/backe…eway",
	} {
		if got := fitPath(p, room, nil).String(); got != want {
			t.Errorf("path in %d: %q, want %q", room, got, want)
		}
	}
	if got := fitPath("~/.config/tmux/scripts", 18, nil).String(); got != "~/.c/t/scripts" {
		t.Errorf("dotdir: %q", got)
	}
	// Hits carry over to what remains.
	f := fitPath(p, 35, []int{30})
	if i := slices.IndexFunc(f, func(p piece) bool { return p.at == 30 }); i < 0 {
		t.Errorf("hit lost: %+v", f)
	}
}

func TestInputView(t *testing.T) {
	text := []rune("a-long-query-typed-into-a-small-box")
	got, at := inputView(text, len(text), 12)
	if !strings.HasPrefix(got, "…") || width(got) > 12 || at > 11 || !strings.HasSuffix(got, "box") {
		t.Errorf("end: %q cursor %d", got, at)
	}
	got, at = inputView(text, 0, 12)
	if !strings.HasPrefix(got, "a-long") || at != 0 {
		t.Errorf("start: %q cursor %d", got, at)
	}
	if got, at := inputView([]rune("abc"), 1, 12); got != "abc" || at != 1 {
		t.Errorf("short: %q %d", got, at)
	}
}

func TestCanvas(t *testing.T) {
	cv := newCanvas(10, 2)
	cv.put(0, 0, "日本語", sPlain, -1)
	cv.put(1, 0, "x", sPlain, -1) // over the second half of 日
	if got := strings.Split(cv.Plain(), "\n")[0]; got != " x本語" {
		t.Errorf("wide overwrite: %q", got)
	}
	cv.putANSI(0, 1, "\x1b[1;38;2;1;2;3mab\x1b[0m\x1b]0;title\x07c\x1b[31md", 3)
	if got := strings.Split(cv.Plain(), "\n")[1]; got != "abc" {
		t.Errorf("ansi line: %q", got)
	}
	if st := cv.at(0, 1).st; !st.bold || st.fg == nil {
		t.Errorf("sgr not kept: %+v", st)
	}
	if st := cv.at(2, 1).st; st.bold {
		t.Errorf("reset not applied: %+v", st)
	}
	out := cv.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "\x1b[") {
		t.Errorf("string: %q", out)
	}
}

func TestMatchWords(t *testing.T) {
	ws := splitQuery([]rune("charlie:train-llm:2 tens"))
	if len(ws) != 4 || ws[2].num != 2 || !ws[2].index || ws[0].index {
		t.Fatalf("split: %+v", ws)
	}
	if ws := splitQuery([]rune("train 2")); ws[1].num != 2 || ws[1].index {
		t.Fatalf("a plain number: %+v", ws)
	}
	exact, _, _, _ := wordMatch(foldText("s50"), foldText("s50"))
	inside, _, _, _ := wordMatch(foldText("50"), foldText("s150"))
	prefix, _, _, _ := wordMatch(foldText("bra"), foldText("bravo"))
	word, _, _, _ := wordMatch(foldText("logs"), foldText("bravo-logs"))
	mid, _, _, _ := wordMatch(foldText("bra"), foldText("cobra"))
	scat, pos, sub, ok := wordMatch(foldText("tlm"), foldText("train-llm"))
	if !(exact > prefix && prefix > word && word > mid && mid > scat && inside > scat) || sub || !ok || len(pos) != 3 {
		t.Fatalf("scores: exact %d prefix %d word %d mid %d scattered %d", exact, prefix, word, mid, scat)
	}
	if _, _, _, ok := wordMatch(foldText("abc"), foldText("a----------b----------c")); ok {
		t.Fatal("a scattered match wider than three times the word")
	}
	if pos, ok := inOrder(foldQuery([]rune("b BRA")), foldText("b bravo")); !ok || !slices.Equal(pos, []int{0, 2, 3, 4}) {
		t.Fatalf("in order: %v %v", pos, ok)
	}
}

func TestLayout(t *testing.T) {
	d := testDash()
	long := strings.Repeat("x", 80)
	for i := range 20 {
		d.View.Hosts[0].Sessions = append(d.View.Hosts[0].Sessions, proto.Session{ID: "$x" + itoa(i), Name: "s" + itoa(i), Ago: 8_000_000})
	}
	d.View.Hosts[0].Sessions = append(d.View.Hosts[0].Sessions, proto.Session{ID: "$5", Name: long, Ago: 9_000_000})
	m, _, _ := newTestModel(t, d, false)
	press(t, m, "ctrl+l")
	m.width, m.height = 150, 30
	g := m.layout()
	if g.cols[3].w < minPreview || g.cols[1].w > maxSess+widen || g.cols[0].w < minHosts {
		t.Fatalf("wide: %+v", g.cols)
	}
	// One long name does not widen the column for everyone (the 90th
	// percentile), and it is cut in its middle.
	if g.cols[1].w > minSess+widen {
		t.Fatalf("sessions %d wide for one long name", g.cols[1].w)
	}
	// Below 120 columns the preview goes.
	m.width = 100
	if g := m.layout(); g.cols[3].w != 0 || g.cols[2].x+g.cols[2].w != 100 {
		t.Fatalf("narrow: %+v", g.cols)
	}
	// Widths only grow while the dashboard is open.
	before := m.widths
	m.view.View.Hosts[0].Sessions = m.view.View.Hosts[0].Sessions[:1]
	m.rebuild()
	if m.widths[0] < before[0] || m.widths[1] < before[1] || m.widths[2] < before[2] {
		t.Fatalf("widths shrank: %v → %v", before, m.widths)
	}
	// Every line fits, at every size.
	for _, size := range [][2]int{{150, 40}, {120, 30}, {86, 23}, {60, 12}, {40, 8}} {
		m.width, m.height = size[0], size[1]
		for _, l := range strings.Split(strings.TrimRight(screen(m), "\n"), "\n") {
			if width(l) > size[0] {
				t.Errorf("%v: %q", size, l)
			}
		}
	}
}
