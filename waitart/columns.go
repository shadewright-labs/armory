package waitart

import "unicode"

// narrowRanges lists the blocks whose assigned, non-zero-width runes every terminal draws one column
// wide. None of them holds an East Asian Wide or Fullwidth rune. Runes outside the table count as two
// columns in the status line and are refused in frames, so a terminal that draws one of them narrower
// only leaves the line shorter.
var narrowRanges = [...]struct{ lo, hi rune }{
	{0x0020, 0x007e}, // Basic Latin, printable part
	{0x00a0, 0x052f}, // Latin-1 Supplement to Cyrillic Supplement
	{0x1e00, 0x1fff}, // Latin Extended Additional, Greek Extended
	{0x2010, 0x2027}, // General Punctuation: dashes, quotation marks, ellipsis
	{0x2030, 0x205e}, // General Punctuation: per mille to vertical four dots
	{0x2070, 0x20c0}, // Superscripts and Subscripts, Currency Symbols
	{0x2100, 0x218b}, // Letterlike Symbols, Number Forms
	{0x2190, 0x22ff}, // Arrows, Mathematical Operators
	{0x2500, 0x259f}, // Box Drawing, Block Elements
	{0x25a0, 0x25fc}, // Geometric Shapes, without the two wide squares at 25FD and 25FE
	{0x2800, 0x28ff}, // Braille Patterns
}

func inNarrowTable(r rune) bool {
	for _, rg := range narrowRanges {
		if r < rg.lo {
			return false
		}
		if r <= rg.hi {
			return true
		}
	}
	return false
}

// zeroWidth reports whether r takes no column of its own: combining and enclosing marks, format
// characters (such as joiners and direction marks) and the line and paragraph separators.
func zeroWidth(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Zl, unicode.Zp)
}

// artRune reports whether r may appear in frame text: a space, or a printable rune from the narrow
// table.
func artRune(r rune) bool {
	if r == ' ' {
		return true
	}
	return !zeroWidth(r) && unicode.IsGraphic(r) && inNarrowTable(r)
}

// statusRune returns the rune drawn for r in the status line and the columns it takes; zero columns
// means r is dropped. Line breaks and tabs become a space. Control characters, zero-width marks and
// runes that are not printable (unassigned, private use, noncharacters) are dropped. A rune from the
// narrow table takes one column and any other printable rune two.
func statusRune(r rune) (rune, int) {
	switch {
	case r == '\n' || r == '\r' || r == '\t':
		return ' ', 1
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0, 0
	case zeroWidth(r) || !unicode.IsGraphic(r):
		return 0, 0
	case inNarrowTable(r):
		return r, 1
	}
	return r, 2
}
