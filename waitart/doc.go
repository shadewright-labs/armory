// Package waitart draws a short animated scene with a status line on a terminal while a command
// waits on a long event, and keeps real output intact while it does.
//
// A [Scene] is data: a list of [Frame] values made of coloured rows, at most [MaxArtRows] rows by
// [Width] columns, with a status line drawn under them. A product passes its own scenes in a [Set],
// which maps each [Kind] of wait (a string the product chooses) to the scenes that fit it, and picks
// one with a random source the caller supplies, so a test gets the same scene every time. [Builtin]
// returns a small neutral set for a product that has none of its own.
//
// [Config.Start] plays a scene on a writer, normally standard error, and returns a [*Player]. It draws
// only when [Config.Enabled] allows it: the writer is a terminal at least [Width] columns wide, the
// caller did not turn the art off with [Config.Off], NO_COLOR is not set to a non-empty value and TERM
// is not "dumb". The package reads no environment on its own: [Config.LookupEnv] supplies the
// variables (NO_COLOR, TERM, COLORTERM and COLUMNS), and without it nothing is drawn. When Start draws
// nothing it returns nil, and every method of a nil Player is safe to call and does nothing, so a
// caller needs no check.
//
// The player redraws the scene in place. Real output goes through [Player.Wrap]: before each write the
// frame is cleared, and once the output ends a line the frame is drawn again below it. While a line is
// unfinished no frame is drawn, so no frame byte lands inside a line of output.
//
// The player stops, clears the frame and shows the cursor again when [Player.Stop] is called or the
// context passed to Start ends. To restore the terminal on Ctrl-C, pass a context from
// [os/signal.NotifyContext]: the interrupt cancels it and the player cleans up before the command
// returns.
//
// The status line is caller text drawn on a terminal, so it is cleaned before it is drawn: line breaks
// and tabs become spaces, control characters, escape sequences, zero-width marks and runes that are not
// printable are dropped, and the text is cut to the frame width in terminal columns with an ellipsis.
// Only the first [MaxStatusBytes] bytes are read. A rune counts as one column when it is in a short
// table of scripts and symbol blocks that every terminal draws one column wide, and as two columns
// otherwise, so the line can come out shorter than the frame but never wraps. Frame text has a
// stricter rule: [Scene.Validate] accepts only runes from that table, so a scene that would corrupt the
// screen is refused instead of drawn.
//
// The package does not log, reads no file and makes no network call.
package waitart
