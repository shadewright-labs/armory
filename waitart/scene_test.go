package waitart

import (
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestBuiltinScenes checks every built-in scene: valid, 10 to 11 frames, at most MaxHeight lines by
// Width columns when drawn with a long status, and every frame the same height.
func TestBuiltinScenes(t *testing.T) {
	names := make([]string, 0, 3)
	for _, sc := range Builtin().Scenes() {
		names = append(names, sc.Name)
		if err := sc.Validate(); err != nil {
			t.Errorf("%s: %v", sc.Name, err)
		}
		if n := len(sc.Frames); n < 10 || n > 11 {
			t.Errorf("%s: %d frames, want 10 or 11", sc.Name, n)
		}
		if sc.Height() > MaxHeight {
			t.Errorf("%s: %d lines, limit %d", sc.Name, sc.Height(), MaxHeight)
		}
		for i, f := range sc.Frames {
			if f.Delay <= 0 || f.Note == "" {
				t.Errorf("%s frame %d: delay %v, note %q", sc.Name, i, f.Delay, f.Note)
			}
			plain := render(&sc, &DefaultStyle, i, strings.Repeat("long status ", 10)).plain()
			lines := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")
			if len(lines) != sc.Height() {
				t.Errorf("%s frame %d: %d lines, want %d", sc.Name, i, len(lines), sc.Height())
			}
			for _, l := range lines {
				if n := utf8.RuneCountInString(l); n > Width {
					t.Errorf("%s frame %d: line of %d columns", sc.Name, i, n)
				}
			}
		}
	}
	if want := []string{"hourglass", "relay", "wave"}; !slices.Equal(names, want) {
		t.Errorf("built-in scenes %v, want %v", names, want)
	}
	if MaxHeight > 7 || Width > 64 {
		t.Errorf("frame limit is %dx%d, want at most 7x64", MaxHeight, Width)
	}
}

const (
	testName = "test"
	testArt  = "a ▲"
)

func validScene() Scene {
	return Scene{
		Name:    testName,
		Palette: map[byte]Colour{'x': {R: 1, G: 2, B: 3, ANSI: 4}},
		Frames: []Frame{
			{Rows: []Row{{Text: testArt, Mask: "x x"}}, Delay: time.Millisecond},
			{Rows: []Row{{Text: " a", Mask: " x"}}},
		},
	}
}

func TestValidateRefuses(t *testing.T) {
	if err := validScene().Validate(); err != nil {
		t.Fatalf("valid scene refused: %v", err)
	}
	row := func(text, mask string) func(*Scene) {
		return func(s *Scene) { s.Frames[0].Rows = []Row{{Text: text, Mask: mask}} }
	}
	for _, tc := range []struct {
		name   string
		change func(*Scene)
	}{
		{"empty name", func(s *Scene) { s.Name = "" }},
		{"name with a space", func(s *Scene) { s.Name = "a b" }},
		{"name with an escape", func(s *Scene) { s.Name = "a\x1b" }},
		{"long name", func(s *Scene) { s.Name = strings.Repeat("a", MaxNameBytes+1) }},
		{"no frames", func(s *Scene) { s.Frames = nil }},
		{"too many frames", func(s *Scene) { s.Frames = make([]Frame, MaxFrames+1) }},
		{"negative key", func(s *Scene) { s.Key = -1 }},
		{"key past the frames", func(s *Scene) { s.Key = 2 }},
		{"negative delay", func(s *Scene) { s.Frames[0].Delay = -1 }},
		{"delay over a minute", func(s *Scene) { s.Frames[0].Delay = time.Minute + 1 }},
		{"palette key space", func(s *Scene) { s.Palette[' '] = Colour{} }},
		{"palette key control", func(s *Scene) { s.Palette['\x1b'] = Colour{} }},
		{"palette key high byte", func(s *Scene) { s.Palette[0x9b] = Colour{} }},
		{"escape sequence", row("\x1b[2J", "    ")},
		{"bell", row("a\a", "  ")},
		{"newline", row("a\nb", "   ")},
		{"carriage return", row("a\rb", "   ")},
		{"tab", row("a\tb", "   ")},
		{"delete", row("a\x7f", "  ")},
		{"C1 control", row("a\u009b", "  ")},
		{"wide rune", row("a漢", "  ")},
		{"emoji", row("\U0001F600", " ")},
		{"combining mark", row("e\u0301", "  ")},
		{"zero-width joiner", row("a\u200db", "   ")},
		{"line separator", row("a\u2028", "  ")},
		{"private use", row("", " ")},
		{"invalid UTF-8", row("a\xff", "  ")},
		{"mask shorter", row("abc", "  ")},
		{"mask longer", row("a", "  ")},
		{"mask not in palette", row("a", "y")},
		{"too wide", row(strings.Repeat("a", Width+1), strings.Repeat(" ", Width+1))},
		{"too many bytes", row(strings.Repeat("a", 4*Width+1), "")},
		{"too many rows", func(s *Scene) { s.Frames[0].Rows = make([]Row, MaxArtRows+1) }},
		{"bad backdrop", func(s *Scene) { s.Backdrop = []Row{{Text: "\x1b", Mask: " "}} }},
		{"too tall backdrop", func(s *Scene) { s.Backdrop = make([]Row, MaxArtRows+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := validScene()
			tc.change(&s)
			err := s.Validate()
			if !errors.Is(err, ErrInvalidScene) {
				t.Fatalf("Validate = %v, want ErrInvalidScene", err)
			}
			if strings.ContainsAny(err.Error(), "\x1b\a\n\r\t\x7f") || strings.Contains(err.Error(), "漢") {
				t.Fatalf("error text quotes the scene: %q", err.Error())
			}
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	s := validScene()
	s.Backdrop = []Row{{}, {Text: strings.Repeat("▁", Width), Mask: strings.Repeat("x", Width)}}
	s.Frames[0].Rows = make([]Row, MaxArtRows)
	s.Frames[0].Rows[0] = Row{Text: "┌─┐●⠋…°×é▓", Mask: "xxxxxxxxxx"}
	s.Frames[0].Delay = time.Minute
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	if s.Height() != MaxHeight {
		t.Fatalf("Height = %d, want %d", s.Height(), MaxHeight)
	}
}

func TestBackdropShowsThroughSpaces(t *testing.T) {
	s := validScene()
	s.Backdrop = []Row{{Text: "....", Mask: "xxxx"}}
	s.Frames[0].Rows = []Row{{Text: " ab", Mask: " xx"}}
	got, _, _ := strings.Cut(render(&s, &DefaultStyle, 0, "").plain(), "\n")
	if got != ".ab." {
		t.Fatalf("first row %q, want %q", got, ".ab.")
	}
}

func TestNewSet(t *testing.T) {
	a, b, c := validScene(), validScene(), validScene()
	b.Name, c.Name = "b", "c"
	s, err := NewSet([]Scene{a, b, c}, map[Kind][]string{"build": {"b"}, "council": {testName, "c"}, "empty": nil})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if got := s.ForKind("build", rand.NewPCG(1, 2)).Name; got != "b" {
			t.Fatalf("ForKind(build) = %q, want b", got)
		}
	}
	seen := map[string]bool{}
	src := rand.NewPCG(3, 4)
	for range 50 {
		name := s.ForKind("council", src).Name
		if name != testName && name != "c" {
			t.Fatalf("ForKind(council) = %q", name)
		}
		seen[name] = true
	}
	if len(seen) != 2 {
		t.Errorf("ForKind(council) chose only %v", seen)
	}
	seen = map[string]bool{}
	for range 100 {
		seen[s.ForKind("unknown", src).Name] = true
		seen[s.ForKind("empty", src).Name] = true
	}
	if len(seen) != 3 {
		t.Errorf("a kind with no scenes chose only %v", seen)
	}
	if got := s.ForKind("council", nil).Name; got != testName {
		t.Errorf("ForKind with a nil source = %q, want the first fitting scene", got)
	}
	if got := s.ForKind("unknown", nil).Name; got != testName {
		t.Errorf("ForKind of an unknown kind with a nil source = %q, want the first scene", got)
	}
	// The same seed gives the same choices.
	x, y := rand.NewPCG(9, 9), rand.NewPCG(9, 9)
	for range 20 {
		if s.ForKind("", x).Name != s.ForKind("", y).Name {
			t.Fatal("the same source state chose different scenes")
		}
	}
	if sc, ok := s.Lookup("c"); !ok || sc.Name != "c" {
		t.Errorf("Lookup(c) = %q, %v", sc.Name, ok)
	}
	if _, ok := s.Lookup("missing"); ok {
		t.Error("Lookup found a missing scene")
	}
}

func TestNewSetCopiesScenes(t *testing.T) {
	a := validScene()
	s, err := NewSet([]Scene{a}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Frames[0].Rows[0] = Row{Text: "\x1b", Mask: " "}
	a.Palette['x'] = Colour{ANSI: 99}
	got, _ := s.Lookup(testName)
	if got.Frames[0].Rows[0].Text != testArt || got.Palette['x'].ANSI != 4 {
		t.Fatal("changing the caller's scene changed the set")
	}
	list := s.Scenes()
	list[0].Name = "changed"
	if _, ok := s.Lookup(testName); !ok {
		t.Fatal("changing the slice from Scenes changed the set")
	}
}

func TestNewSetRefuses(t *testing.T) {
	bad := validScene()
	bad.Frames[0].Rows = []Row{{Text: "\x1b", Mask: " "}}
	for _, tc := range []struct {
		name   string
		scenes []Scene
		kinds  map[Kind][]string
		want   error
	}{
		{"no scenes", nil, nil, ErrInvalidSet},
		{"duplicate name", []Scene{validScene(), validScene()}, nil, ErrInvalidSet},
		{"kind names a missing scene", []Scene{validScene()}, map[Kind][]string{"k": {"missing"}}, ErrInvalidSet},
		{"invalid scene", []Scene{validScene(), bad}, nil, ErrInvalidScene},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewSet(tc.scenes, tc.kinds)
			if s != nil || !errors.Is(err, tc.want) {
				t.Fatalf("NewSet = %v, %v; want nil, %v", s, err, tc.want)
			}
		})
	}
}

func lastLine(cv canvas) string {
	lines := strings.Split(strings.TrimSuffix(cv.plain(), "\n"), "\n")
	return lines[len(lines)-1]
}

// columns counts the terminal columns of a status line by the package rule.
//
// Frame rows hold only runes of the narrow table and count one column per rune; see artColumns.
func columns(s string) int {
	n := 0
	for _, r := range s {
		_, w := statusRune(r)
		n += w
	}
	return n
}

// artColumns counts the columns of a frame row: one per rune, since frames hold only runes of the
// narrow table.
func artColumns(s string) int { return utf8.RuneCountInString(s) }

func TestStatusIsCleanedAndCut(t *testing.T) {
	sc := scene(t, "relay")
	last := lastLine(render(&sc, &DefaultStyle, 0, "Round 2:\x1b[2J\x07 seat\nanswered\u009b "+strings.Repeat("x", 100)))
	if strings.ContainsAny(last, "\x1b\x07\n\u009b") {
		t.Fatalf("control characters reached the status line: %q", last)
	}
	if !strings.HasPrefix(last[len("⠋ "):], "Round 2:[2J seat answered ") {
		t.Fatalf("status text not kept: %q", last)
	}
	if n := columns(last); n != Width || !strings.HasSuffix(last, "…") {
		t.Fatalf("long status not cut to %d columns with an ellipsis: %d %q", Width, n, last)
	}
}

// TestStatusFitsInColumns checks that two-column runes count twice, so the status row never wraps and
// the cursor-up count stays right, and that a two-column rune is never cut in half.
func TestStatusFitsInColumns(t *testing.T) {
	sc := scene(t, "relay")
	for _, status := range []string{
		"Waiting for confirmation: " + strings.Repeat("漢", 30),
		strings.Repeat("\U0001F600", 40),
		"a" + strings.Repeat("漢", 40), // the cut falls inside a wide rune
		"e\u0301" + strings.Repeat("e\u0301", 70),
		strings.Repeat("°", 90),
		strings.Repeat("　", 40),
		strings.Repeat("\xff", 60), // invalid UTF-8 draws as U+FFFD, counted as two columns
	} {
		cv := render(&sc, &DefaultStyle, 0, status)
		lines := strings.Split(strings.TrimSuffix(stripSeq(cv.ansi(false)), "\r\n"), "\r\n")
		for i, line := range lines {
			w := artColumns(line)
			if i == len(lines)-1 {
				w = columns(line)
			}
			if w > Width {
				t.Errorf("line is %d columns wide, want at most %d: %q", w, Width, line)
			}
			if !utf8.ValidString(line) {
				t.Errorf("line is not valid UTF-8: %q", line)
			}
		}
		if last := lastLine(cv); columns(last) < Width-2 && len(status) > 2*Width {
			t.Errorf("status cut too short: %d columns: %q", columns(last), last)
		}
	}
}

func TestStatusReadsAtMostMaxStatusBytes(t *testing.T) {
	sc := scene(t, "relay")
	// A status of zero-width marks keeps nothing, so the cut by bytes is what bounds the work.
	huge := strings.Repeat("\u0301", 1<<20) + "tail"
	if last := lastLine(render(&sc, &DefaultStyle, 0, huge)); strings.Contains(last, "tail") {
		t.Fatalf("text past MaxStatusBytes was read: %q", last)
	}
	if got := statusCells(&DefaultStyle, 0, strings.Repeat("a", MaxStatusBytes-1)+"漢"); len(got) != Width {
		t.Fatalf("status of %d cells, want %d", len(got), Width)
	}
}

func TestColumnRule(t *testing.T) {
	// Runes from the East Asian Wide and Fullwidth classes take two columns.
	for _, r := range []rune{
		0x1100, 0x231a, 0x2329, 0x23f0, 0x25fd, 0x2614, 0x26a1, 0x2705, 0x2728, 0x2b50, 0x2e80, 0x3000,
		0x3041, 0x30a2, 0x4e00, 0xac00, 0xf900, 0xfe30, 0xff01, 0xffe0, 0x1f300, 0x1f600, 0x1f9d0, 0x20000,
	} {
		if out, w := statusRune(r); w != 2 || out != r {
			t.Errorf("U+%04X: rune %q, %d columns; want 2", r, out, w)
		}
	}
	// Runes from the narrow table that are East Asian ambiguous take two columns.
	for _, r := range []rune{0xa1, 0xb0, 0xe9, 0x142, 0x3b1, 0x416, 0x2014, 0x2026, 0x20ac, 0x2122, 0x2192,
		0x2200, 0x2500, 0x2588, 0x25b2, 0x25cf, 0x25ef} {
		if out, w := statusRune(r); w != 2 || out != r {
			t.Errorf("U+%04X: rune %q, %d columns; want 2", r, out, w)
		}
	}
	// The other runes from the narrow table take one column.
	for _, r := range []rune{'a', '~', 0xa0, 0xa2, 0xe7, 0x105, 0x17c, 0x3ac, 0x452, 0x1e9e,
		0x2011, 0x2017, 0x20a4, 0x2101, 0x219a, 0x2201, 0x254c, 0x2590, 0x25ab, 0x25fc, 0x2800, 0x28ff} {
		if out, w := statusRune(r); w != 1 || out != r {
			t.Errorf("U+%04X: rune %q, %d columns; want 1", r, out, w)
		}
	}
	// Control characters, zero-width marks and runes that are not printable are dropped.
	for _, r := range []rune{0, 0x07, 0x1b, 0x7f, 0x80, 0x9b, 0x9f, 0x300, 0x20dd, 0x200b, 0x200d,
		0x200e, 0x2028, 0x2029, 0xfe0f, 0xfeff, 0xe000, 0xfffe, 0x0378, 0x10ffff} {
		if _, w := statusRune(r); w != 0 {
			t.Errorf("U+%04X kept with %d columns, want dropped", r, w)
		}
	}
	for _, r := range "\n\r\t" {
		if out, w := statusRune(r); out != ' ' || w != 1 {
			t.Errorf("%q: %q, %d; want a space", r, out, w)
		}
	}
	// Every rune frames may hold takes one column in the status line too, or two when it is ambiguous.
	for r := rune(0); r <= 0x2fff; r++ {
		if artRune(r) {
			want := 1
			if ambiguous(r) {
				want = 2
			}
			if _, w := statusRune(r); w != want {
				t.Fatalf("U+%04X is allowed in frames but takes %d columns, want %d", r, w, want)
			}
		}
	}
	// The ambiguous table lies inside the narrow table and is sorted.
	for i, rg := range ambiguousRanges {
		if !inNarrowTable(rg.lo) || !inNarrowTable(rg.hi) || rg.lo > rg.hi ||
			(i > 0 && ambiguousRanges[i-1].hi >= rg.lo) {
			t.Fatalf("ambiguous range %d (U+%04X to U+%04X) is out of order or outside the narrow table", i, rg.lo, rg.hi)
		}
	}
}

// stripSeq removes the escape sequences the package writes: colour, erase and cursor moves.
func stripSeq(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
