package waitart

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// ownSeq matches every escape sequence the package writes in a frame: colours, reset and erase to the
// end of the line.
var ownSeq = regexp.MustCompile(`\x1b\[(?:38;5;\d{1,3}|38;2;\d{1,3};\d{1,3};\d{1,3}|0)m|\x1b\[K`)

// checkDrawn checks a drawn frame: only the package's own escape sequences, no other control byte
// apart from the \r\n that ends each row, valid UTF-8, the expected number of rows and none wider than
// Width columns.
func checkDrawn(t *testing.T, cv canvas, wantRows int) {
	t.Helper()
	for _, tc := range []bool{false, true} {
		out := ownSeq.ReplaceAllString(cv.ansi(tc), "")
		if !utf8.ValidString(out) {
			t.Fatalf("drawn frame is not valid UTF-8: %q", out)
		}
		rows := strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n")
		if len(rows) != wantRows {
			t.Fatalf("drawn frame has %d rows, want %d: %q", len(rows), wantRows, out)
		}
		for _, row := range rows {
			for _, r := range row {
				if r < 0x20 || (r >= 0x7f && r < 0xa0) {
					t.Fatalf("control rune U+%04X in a drawn row: %q", r, row)
				}
			}
			if w := columns(row); w > Width {
				t.Fatalf("drawn row is %d columns wide: %q", w, row)
			}
		}
	}
}

func FuzzStatus(f *testing.F) {
	for _, s := range []string{
		"",
		"Waiting",
		"Round 2: 3 of 5 answered",
		"\x1b[2J\x1b[H",
		"\x1b]0;title\x07",
		"a\nb\rc\td",
		"\u009b31m",
		"漢字" + strings.Repeat("x", 70),
		strings.Repeat("\U0001F600", 40),
		"e\u0301\u200d\u2028\ufeff",
		"\xff\xfe\xc0",
		strings.Repeat("a", MaxStatusBytes+10),
	} {
		f.Add(s)
	}
	sc, _ := Builtin().Lookup("relay")
	f.Fuzz(func(t *testing.T, status string) {
		cells := statusCells(&DefaultStyle, 0, status)
		if len(cells) > Width {
			t.Fatalf("status takes %d cells, want at most %d", len(cells), Width)
		}
		for i, c := range cells {
			if c.r == wideTail && (i == 0 || cells[i-1].r == wideTail) {
				t.Fatalf("a second column without its rune at cell %d", i)
			}
			if c.r != wideTail {
				if _, w := statusRune(c.r); w == 0 && c.r != '…' {
					t.Fatalf("dropped rune U+%04X drawn", c.r)
				}
				if _, w := statusRune(c.r); w == 2 && (i+1 >= len(cells) || cells[i+1].r != wideTail) {
					t.Fatalf("two-column rune U+%04X at cell %d has no second cell", c.r, i)
				}
			}
		}
		checkDrawn(t, render(&sc, &DefaultStyle, 0, status), sc.Height())
	})
}

func FuzzFrame(f *testing.F) {
	for _, s := range [][2]string{
		{testArt, "x x"},
		{"\x1b[2J", "    "},
		{"漢", " "},
		{"e\u0301", "xx"},
		{strings.Repeat("▁", Width), strings.Repeat("x", Width)},
		{"abc", "x"},
		{"a\xff", "  "},
	} {
		f.Add(s[0], s[1], s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, text, mask, back, backMask string) {
		sc := Scene{
			Name:     "fuzz",
			Palette:  map[byte]Colour{'x': {R: 1, ANSI: 1}, 'y': {G: 2, ANSI: 2}},
			Backdrop: []Row{{Text: back, Mask: backMask}},
			Frames:   []Frame{{Rows: []Row{{Text: text, Mask: mask}, {Text: back, Mask: backMask}}}},
		}
		if err := sc.Validate(); err != nil {
			return
		}
		cv := render(&sc, &DefaultStyle, 0, "status")
		checkDrawn(t, cv, sc.Height())
		if got, _, _ := strings.Cut(cv.plain(), "\n"); got != strings.TrimRight(overlay(back, text), " ") {
			t.Fatalf("first row %q, want the frame over the backdrop", got)
		}
	})
}

// overlay draws top over bottom, rune by rune, with spaces in top transparent.
func overlay(bottom, top string) string {
	b, tp := []rune(bottom), []rune(top)
	for len(b) < len(tp) {
		b = append(b, ' ')
	}
	for i, r := range tp {
		if r != ' ' {
			b[i] = r
		}
	}
	return string(b)
}
