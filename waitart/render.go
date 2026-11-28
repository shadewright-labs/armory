package waitart

import (
	"strconv"
	"strings"
)

// Style holds the colours of the status line.
type Style struct {
	// Spinner is the colour of the spinner at the start of the line.
	Spinner Colour
	// Label is the colour of the status text up to its first colon.
	Label Colour
	// Detail is the colour of the rest of the status text, from the first colon on.
	Detail Colour
}

// DefaultStyle is the status line style used when [Config.Style] is nil.
var DefaultStyle = Style{
	Spinner: Colour{R: 0x4f, G: 0xc3, B: 0xe0, ANSI: 74},
	Label:   Colour{R: 0xe6, G: 0xe6, B: 0xe6, ANSI: 254},
	Detail:  Colour{R: 0x8a, G: 0x8f, B: 0x98, ANSI: 245},
}

// cell is one terminal column of a drawn scene.
type cell struct {
	r   rune // wideTail for the second column of a two-column rune
	set bool // col applies; false draws with the terminal's own colour
	col Colour
}

// wideTail marks the second column of a two-column rune; it prints nothing.
const wideTail rune = -1

type canvas [][Width]cell

var spinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// render draws frame idx of scene sc (looping) with the spinner at step idx and the status text on the
// last line. The scene must be valid.
func render(sc *Scene, st *Style, idx int, status string) canvas {
	rows := sc.artRows()
	cv := make(canvas, rows+1)
	for y := range cv {
		for x := range cv[y] {
			cv[y][x] = cell{r: ' '}
		}
	}
	layer := func(art []Row) {
		for y, row := range art {
			x := 0
			for _, r := range row.Text {
				if r != ' ' && x < Width {
					c := cell{r: r}
					if m := row.Mask[x]; m != ' ' {
						c.set, c.col = true, sc.Palette[m]
					}
					cv[y][x] = c
				}
				x++
			}
		}
	}
	layer(sc.Backdrop)
	layer(sc.Frames[idx%len(sc.Frames)].Rows)
	copy(cv[rows][:], statusCells(st, idx, status))
	return cv
}

// statusCells returns the status line: the spinner, a space and the cleaned status text, at most Width
// cells, one per terminal column by the status rule, cut with an ellipsis when it is longer.
func statusCells(st *Style, idx int, status string) []cell {
	if len(status) > MaxStatusBytes {
		status = status[:MaxStatusBytes]
	}
	line := make([]cell, 0, Width+2)
	line = append(line, cell{r: spinner[idx%len(spinner)], set: true, col: st.Spinner}, cell{r: ' '})
	col := st.Label
	cut := false
	for _, r := range status {
		if r == ':' {
			col = st.Detail
		}
		out, w := statusRune(r)
		if w == 0 {
			continue
		}
		if len(line)+w > Width {
			cut = true
			break
		}
		line = append(line, cell{r: out, set: true, col: col})
		if w == 2 {
			line = append(line, cell{r: wideTail, set: true, col: col})
		}
	}
	if cut {
		// Make room for the ellipsis in the last two columns, removing a two-column rune whole. The
		// ellipsis is East Asian ambiguous, so it counts as two columns like any other such rune.
		for len(line) > Width-2 {
			if line[len(line)-1].r == wideTail {
				line = line[:len(line)-1]
			}
			line = line[:len(line)-1]
		}
		line = append(line, cell{r: '…', set: true, col: st.Detail}, cell{r: wideTail, set: true, col: st.Detail})
	}
	return line
}

// plain returns the canvas as text without colour, trailing spaces trimmed.
func (cv canvas) plain() string {
	var b strings.Builder
	for _, row := range cv {
		var l strings.Builder
		for _, c := range row {
			if c.r != wideTail {
				l.WriteRune(c.r)
			}
		}
		b.WriteString(strings.TrimRight(l.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func sgr(b *strings.Builder, c Colour, truecolor bool) {
	if truecolor {
		b.WriteString("\x1b[38;2;")
		b.WriteString(strconv.Itoa(int(c.R)))
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(int(c.G)))
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(int(c.B)))
	} else {
		b.WriteString("\x1b[38;5;")
		b.WriteString(strconv.Itoa(int(c.ANSI)))
	}
	b.WriteByte('m')
}

const reset = "\x1b[0m"

// ansi renders the canvas with colour codes. Every row resets the colour, clears to its end and ends
// with a carriage return and a line feed, so a redraw leaves nothing behind and each row starts at
// column 0 even on a terminal in raw mode.
func (cv canvas) ansi(truecolor bool) string {
	var b strings.Builder
	for _, row := range cv {
		end := len(row)
		for end > 0 && row[end-1].r == ' ' {
			end--
		}
		cur, curSet := Colour{}, false
		for _, c := range row[:end] {
			if c.r != ' ' && (c.set != curSet || c.col != cur) {
				if c.set {
					sgr(&b, c.col, truecolor)
				} else {
					b.WriteString(reset)
				}
				cur, curSet = c.col, c.set
			}
			if c.r != wideTail {
				b.WriteRune(c.r)
			}
		}
		b.WriteString(reset + "\x1b[K\r\n")
	}
	return b.String()
}
