package waitart

import (
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// The built-in scenes are original to this package and fall under its licence. They are made of plain
// shapes with no characters or lore, so any product can show them.

// builtinPalette holds the colours of the built-in scenes.
var builtinPalette = map[byte]Colour{
	'w': {R: 0xe6, G: 0xe6, B: 0xe6, ANSI: 254}, // white
	'g': {R: 0x8a, G: 0x8f, B: 0x98, ANSI: 245}, // grey
	'd': {R: 0x4b, G: 0x50, B: 0x58, ANSI: 239}, // dim
	'c': {R: 0x4f, G: 0xc3, B: 0xe0, ANSI: 74},  // cyan
	'a': {R: 0xe8, G: 0xb0, B: 0x4b, ANSI: 179}, // amber
	'n': {R: 0x6c, G: 0xc6, B: 0x7a, ANSI: 114}, // green
}

var builtin = sync.OnceValue(func() *Set {
	s, err := NewSet([]Scene{
		{Name: "hourglass", Key: 4, Palette: builtinPalette, Frames: hourglassFrames()},
		{Name: "relay", Key: 6, Palette: builtinPalette, Frames: relayFrames()},
		{Name: "wave", Key: 0, Palette: builtinPalette, Frames: waveFrames()},
	}, nil)
	if err != nil {
		panic("waitart: built-in scenes are invalid") // caught by TestBuiltinScenes
	}
	return s
})

// Builtin returns the built-in set: three neutral scenes, "hourglass" (sand running through a glass),
// "relay" (a packet sent between two boxes and its answer coming back) and "wave" (a rolling wave). It
// maps no kinds, so [Set.ForKind] picks any of them. A product can add its own kinds with
// NewSet(Builtin().Scenes(), kinds).
func Builtin() *Set { return builtin() }

// grid is a scratch canvas for building frames: text and mask, one entry per column.
type grid struct {
	text [][]rune
	mask [][]byte
}

func newGrid(rows, cols int) *grid {
	g := &grid{text: make([][]rune, rows), mask: make([][]byte, rows)}
	for y := range rows {
		g.text[y] = []rune(strings.Repeat(" ", cols))
		g.mask[y] = []byte(strings.Repeat(" ", cols))
	}
	return g
}

// put writes s at column x of row y in colour c; spaces in s are skipped.
func (g *grid) put(x, y int, s string, c byte) {
	for i, r := range []rune(s) {
		if r != ' ' {
			g.text[y][x+i] = r
			g.mask[y][x+i] = c
		}
	}
}

// rows returns the grid as rows with trailing blanks cut. A blank text cell always has a blank mask, so
// text and mask are cut by the same columns.
func (g *grid) rows() []Row {
	out := make([]Row, len(g.text))
	for y := range g.text {
		out[y] = Row{
			Text: strings.TrimRight(string(g.text[y]), " "),
			Mask: strings.TrimRight(string(g.mask[y]), " "),
		}
	}
	return out
}

func hourglassFrames() []Frame {
	const x0 = 27
	// Sand cells as (row, column) inside the glass: the top bulb drains from the centre of its upper row,
	// and the bottom bulb fills from the centre of its lower row.
	type pos struct{ y, x int }
	order := func(y, from, to int) []pos {
		mid := (from + to) / 2
		ps := []pos{{y, mid}}
		for d := 1; mid-d >= from; d++ {
			ps = append(ps, pos{y, mid - d}, pos{y, mid + d})
		}
		return ps
	}
	top := slices.Concat(order(1, 1, 7), order(2, 2, 6))
	bottom := slices.Concat(order(4, 1, 7), order(3, 2, 6))
	sand := len(top)
	frames := make([]Frame, 0, 11)
	for i := range 11 {
		moved := min(sand, (i*sand+4)/9)
		g := newGrid(6, x0+9)
		g.put(x0, 0, "▁▁▁▁▁▁▁▁▁", 'g')
		g.put(x0, 1, "╲       ╱", 'g')
		g.put(x0, 2, " ╲     ╱ ", 'g')
		g.put(x0, 3, " ╱     ╲ ", 'g')
		g.put(x0, 4, "╱       ╲", 'g')
		g.put(x0, 5, "▔▔▔▔▔▔▔▔▔", 'g')
		for _, p := range top[moved:] {
			g.put(x0+p.x, p.y, "█", 'a')
		}
		for _, p := range bottom[:moved] {
			g.put(x0+p.x, p.y, "█", 'a')
		}
		note := "sand runs from the top bulb to the bottom"
		if moved < sand {
			if g.text[3][x0+4] == ' ' {
				g.put(x0+4, 3, "┆", 'a')
			}
		} else {
			note = "the top bulb is empty; the glass turns over"
		}
		if i == 0 {
			note = "the top bulb is full"
		}
		frames = append(frames, Frame{Rows: g.rows(), Delay: 160 * time.Millisecond, Note: note})
	}
	return frames
}

func relayFrames() []Frame {
	const (
		left, right = 4, 52
		cols        = right + 5
	)
	base := func(leftCol, rightCol byte) *grid {
		g := newGrid(3, cols)
		for _, b := range []struct {
			x int
			c byte
		}{{left, leftCol}, {right, rightCol}} {
			g.put(b.x, 0, "┌───┐", b.c)
			g.put(b.x, 1, "│ ▪ │", b.c)
			g.put(b.x, 2, "└───┘", b.c)
		}
		g.put(left+6, 1, strings.Repeat("┄", right-left-7), 'd')
		return g
	}
	frames := make([]Frame, 0, 11)
	for k := range 6 {
		g := base('c', 'g')
		g.put(12+k*7, 1, "●", 'c')
		note := "a packet travels to the far box"
		if k == 0 {
			note = "the near box sends a packet"
		}
		frames = append(frames, Frame{Rows: g.rows(), Delay: 120 * time.Millisecond, Note: note})
	}
	frames = append(frames, Frame{
		Rows: base('g', 'n').rows(), Delay: 220 * time.Millisecond, Note: "the far box takes the packet",
	})
	for k := range 4 {
		g := base('g', 'g')
		g.put(45-k*11, 1, "◂", 'n')
		frames = append(frames, Frame{Rows: g.rows(), Delay: 120 * time.Millisecond, Note: "the answer comes back"})
	}
	return frames
}

func waveFrames() []Frame {
	const (
		x0, cols = 8, 48
		period   = 20
		step     = 2
	)
	blocks := []rune(" ▁▂▃▄▅▆▇█")
	frames := make([]Frame, 0, period/step)
	for f := range period / step {
		g := newGrid(2, x0+cols)
		for x := range cols {
			phase := 2 * math.Pi * float64(x+f*step) / period
			level := 1 + int(math.Round(7.5+7.5*math.Sin(phase)))
			level = max(1, min(16, level))
			g.put(x0+x, 1, string(blocks[min(level, 8)]), 'c')
			if level > 8 {
				g.put(x0+x, 0, string(blocks[level-8]), 'w')
			}
		}
		frames = append(frames, Frame{Rows: g.rows(), Delay: 110 * time.Millisecond, Note: "the wave rolls on"})
	}
	return frames
}
