package waitart

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
	"unicode/utf8"
)

const (
	// Width is the most columns a frame row takes, the width the status line is cut to, and the
	// narrowest terminal a scene is drawn on.
	Width = 64
	// MaxArtRows is the most rows of art in a frame. The status line comes under them.
	MaxArtRows = 6
	// MaxHeight is the most lines a drawn scene takes: the art and the status line.
	MaxHeight = MaxArtRows + 1
	// MaxFrames is the most frames in a scene.
	MaxFrames = 64
	// MaxNameBytes is the longest scene name.
	MaxNameBytes = 64
	// MaxStatusBytes is how much of the status text is read; the rest is cut.
	MaxStatusBytes = 1024
	// DefaultDelay is how long a frame with a zero Delay stays.
	DefaultDelay = 150 * time.Millisecond
	// maxDelay keeps a scene from freezing on one frame for good.
	maxDelay = time.Minute
)

// ErrInvalidScene is returned by [Scene.Validate] and [NewSet] for a scene that breaks a rule of the
// package. The error text names the frame and row by number and never quotes the scene.
var ErrInvalidScene = errors.New("waitart: invalid scene")

// ErrInvalidSet is returned by [NewSet] for an empty set, a scene name used twice, or a kind that names
// a scene the set does not hold.
var ErrInvalidSet = errors.New("waitart: invalid set")

// Colour is one palette entry: a 24-bit colour, and the 256-colour index used when the terminal does
// not announce 24-bit colour through COLORTERM.
type Colour struct {
	// R, G and B are the 24-bit colour.
	R, G, B uint8
	// ANSI is the 256-colour index.
	ANSI uint8
}

// Row is one row of art: the text and a colour mask of the same length in runes. Each mask byte is a
// key of the scene's [Scene.Palette], or a space for a cell drawn without colour. A space in the text
// is a blank cell; in a frame drawn over a [Scene.Backdrop] it lets the backdrop show through.
type Row struct {
	// Text is the row as drawn. Every rune takes one column (see [Scene.Validate]).
	Text string
	// Mask holds one byte per rune of Text.
	Mask string
}

// Frame is one picture of a scene and how long it stays.
type Frame struct {
	// Rows are the rows of art from the top, at most [MaxArtRows].
	Rows []Row
	// Delay is how long the frame stays before the next one. Zero means [DefaultDelay].
	Delay time.Duration
	// Note says what happens in the frame. The player does not draw it.
	Note string
}

// Scene is one animated scene: its frames, which play in order and then loop, and the colours they use.
type Scene struct {
	// Name is the short name the scene is looked up by: ASCII letters, digits, '-' and '_'.
	Name string
	// Key is the index of the frame that sums the scene up, for a still preview.
	Key int
	// Palette maps each mask byte (a printable ASCII character other than space) to its colour.
	Palette map[byte]Colour
	// Backdrop rows are drawn under every frame, such as the ground. It may be empty.
	Backdrop []Row
	// Frames play in order and then loop. A scene has at least one.
	Frames []Frame
}

// artRows returns the rows of art the scene takes: its tallest frame or its backdrop.
func (s *Scene) artRows() int {
	n := len(s.Backdrop)
	for i := range s.Frames {
		n = max(n, len(s.Frames[i].Rows))
	}
	return n
}

// Height returns the lines the scene takes when drawn: its rows of art and the status line.
func (s Scene) Height() int {
	return s.artRows() + 1
}

// Validate reports whether the scene can be drawn without corrupting the terminal. It checks the name,
// the key, the palette, the delays, at most [MaxFrames] frames of at most [MaxArtRows] rows, and every
// row: valid UTF-8, at most [Width] runes, each a space or a printable rune from the one-column table
// described in the package documentation (no control characters or escape sequences), and a mask of
// the same length whose bytes are a space or a palette key. Errors match [ErrInvalidScene].
func (s Scene) Validate() error {
	if !validName(s.Name) {
		return fmt.Errorf("%w: name must be 1 to %d ASCII letters, digits, '-' or '_'", ErrInvalidScene, MaxNameBytes)
	}
	if len(s.Frames) == 0 || len(s.Frames) > MaxFrames {
		return fmt.Errorf("%w: a scene needs 1 to %d frames", ErrInvalidScene, MaxFrames)
	}
	if s.Key < 0 || s.Key >= len(s.Frames) {
		return fmt.Errorf("%w: key is not a frame index", ErrInvalidScene)
	}
	for k := range s.Palette {
		if k <= ' ' || k > '~' {
			return fmt.Errorf("%w: palette key must be printable ASCII other than space", ErrInvalidScene)
		}
	}
	if err := s.validRows(s.Backdrop); err != nil {
		return fmt.Errorf("%w: backdrop %w", ErrInvalidScene, err)
	}
	for i := range s.Frames {
		f := &s.Frames[i]
		if f.Delay < 0 || f.Delay > maxDelay {
			return fmt.Errorf("%w: frame %d: delay out of range", ErrInvalidScene, i)
		}
		if err := s.validRows(f.Rows); err != nil {
			return fmt.Errorf("%w: frame %d %w", ErrInvalidScene, i, err)
		}
	}
	return nil
}

// rowError is the reason one row is refused; it carries no text from the row.
type rowError struct {
	row    int
	reason string
}

func (e *rowError) Error() string { return fmt.Sprintf("row %d: %s", e.row, e.reason) }

func (s *Scene) validRows(rows []Row) error {
	if len(rows) > MaxArtRows {
		return &rowError{row: MaxArtRows, reason: fmt.Sprintf("more than %d rows", MaxArtRows)}
	}
	for y, r := range rows {
		// Byte bounds first, so a huge row is refused without walking it.
		if len(r.Text) > Width*utf8.UTFMax || len(r.Mask) > Width {
			return &rowError{row: y, reason: fmt.Sprintf("wider than %d columns", Width)}
		}
		if !utf8.ValidString(r.Text) {
			return &rowError{row: y, reason: "text is not valid UTF-8"}
		}
		n := 0
		for _, c := range r.Text {
			if !artRune(c) {
				return &rowError{row: y, reason: "text holds a rune that is not one printable column"}
			}
			n++
		}
		if n > Width {
			return &rowError{row: y, reason: fmt.Sprintf("wider than %d columns", Width)}
		}
		if len(r.Mask) != n {
			return &rowError{row: y, reason: "mask length differs from the text length in runes"}
		}
		for i := range len(r.Mask) {
			m := r.Mask[i]
			if m == ' ' {
				continue
			}
			if _, ok := s.Palette[m]; !ok {
				return &rowError{row: y, reason: "mask byte is not a palette key"}
			}
		}
	}
	return nil
}

func validName(name string) bool {
	if name == "" || len(name) > MaxNameBytes {
		return false
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// clone returns a copy of s whose slices and palette the caller cannot change afterwards.
func (s Scene) clone() Scene {
	out := s
	out.Palette = maps.Clone(s.Palette)
	out.Backdrop = slices.Clone(s.Backdrop)
	out.Frames = slices.Clone(s.Frames)
	for i := range out.Frames {
		out.Frames[i].Rows = slices.Clone(out.Frames[i].Rows)
	}
	return out
}
