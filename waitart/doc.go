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
// caller did not turn the art off with [Config.Off], NO_COLOR is not set to a non-empty value, TERM
// is not "dumb" and the locale is UTF-8 or not set. The package reads no environment on its own:
// [Config.LookupEnv] supplies the variables (NO_COLOR, TERM, COLORTERM, COLUMNS, LC_ALL, LC_CTYPE and
// LANG), and without it nothing is drawn. When Start draws
// nothing it returns nil, and every method of a nil Player is safe to call and does nothing, so a
// caller needs no check.
//
// The player redraws the scene in place. Real output goes through [Player.Wrap]: before each write the
// frame is cleared, and once the output ends a line the frame is drawn again below it. While a line is
// unfinished no frame is drawn, so no frame byte lands inside a line of output, and while the output
// ends inside a UTF-8 sequence the player sends no cursor sequence. Start is called once the output so
// far ends a line, since the first frame is drawn from the start of the cursor's line; Start does not
// check this.
//
// Each wrapped write holds the player's lock for the whole write, from the clear through the write and
// flush of the wrapped writer to the redraw. The player's own drawing never runs between the caller's
// bytes, and writes through Wrap, from any goroutine and through any writer of the same player, run one
// at a time. A writer passed to Wrap passes each write on before it returns, or has a Flush() error
// method that Wrap calls. It must not write through the same player: Wrap refuses a writer it made for
// that player, and a nested write it sees only when it happens, as in
// p.Wrap(bufio.NewWriter(p.Wrap(term))), fails with [ErrNestedWrap] and makes the player inert, instead
// of waiting on the lock its own goroutine holds.
//
// The writer given to Start must deliver each write before it returns: the player never flushes it, so
// a buffered writer could hold a frame or the cursor show after Stop has returned. The player tracks
// UTF-8 boundaries, not escape sequences, so a caller must not leave a terminal escape sequence open
// across writes (such as "\x1b[31" in one write and "m" in the next) while Stop can run or the context
// can end: the cursor show would land inside it.
//
// One rule covers failed writes: after any write or flush error, or a short write, on the terminal or
// on a writer passed to Wrap (a zero-length write included), and after a refused nested write, the
// player goes inert for good. It draws no frame, clears nothing, sends
// no cursor sequence and retries nothing; the caller's own bytes still pass through Wrap as given. The
// cost is that a failed write can leave the cursor hidden, or part of a frame on the screen. Only the
// normal path ends with a clean clear and cursor show.
//
// The width is checked at these points: in Enabled, before the first frame; before each frame is
// written, after it is rendered (so after the cursor hide at start and after the clear of each redraw);
// before a frame is cleared, at each frame tick while no partial line is on the screen, at the start of
// each non-empty wrapped write and in Stop; and after each non-empty wrapped write that succeeds, with
// or without a line break. It is not checked after a frame is written, nor between the clear at the
// start of a wrapped write and the end of that write: a terminal that narrows then is seen at the next
// check, and until then the cursor stays hidden, a whole frame delay when no output comes. When the
// terminal is narrower than [Width] columns or stops being a terminal, the player draws no more frames,
// does not clear the last one (the cursor-up count may no longer hold) and shows the cursor.
//
// Every terminal write of the player happens under its lock, so a terminal or wrapped writer whose
// writes block (a pipe nobody reads) blocks Stop and [Player.SetStatus] until the write returns. The
// package starts no goroutine per write to get around that.
//
// The player stops, clears the frame and shows the cursor again when [Player.Stop] is called or the
// context passed to Start ends. Cleanup is best effort. The guarantee is the last check: the stop and
// the context are checked after a frame is rendered, just before it is written, and no frame is
// written once either is seen there; a cancel after that check, or during the write, does not call the
// write back, and the cleanup clears that frame. Ending the context starts the cleanup on the player's
// goroutine but does not wait for it, so to restore the terminal on Ctrl-C, pass a context from
// [os/signal.NotifyContext] and call Stop before the command returns: Stop returns once the cleanup is
// written. Cleanup is skipped after a failed write (the player is inert), and when the output ends
// inside a UTF-8 sequence the cursor show waits for the wrapped write that completes it, so the cursor
// stays hidden if that write never comes.
//
// The status line is caller text drawn on a terminal, so it is cleaned before it is drawn: line breaks
// and tabs become spaces, control characters, escape sequences, zero-width marks and runes that are not
// printable are dropped, and the text is cut to the frame width in terminal columns with an ellipsis.
// Only the first [MaxStatusBytes] bytes are read. A rune counts as one column when it is in a short
// table of scripts and symbol blocks that terminals draw one column wide and is not East Asian
// ambiguous, and as two columns otherwise, so the line can come out shorter than the frame but never
// wraps. Cleaning is not redaction: printable text is shown as given, so a caller passes only status
// text that is safe to show, never a token or other secret.
//
// Frame text has a stricter rule: [Scene.Validate] accepts only runes from that table, so a scene that
// would corrupt the screen is refused instead of drawn. Frames may use ambiguous runes such as box
// drawing and block elements, counted as one column each, and the width checks compare the terminal
// with [Width]. This is a known limit: on a terminal set to draw ambiguous runes two columns wide, a
// row is as wide as its other runes plus twice its ambiguous ones (the built-in "relay" scene reaches
// 102 columns there and "wave" 104). Such a row wraps only when that width is more than the terminal's
// columns, and then each redraw leaves stale art above the frame. On such a terminal, turn the art off
// with [Config.Off].
//
// The locale check is a case-insensitive substring match for "utf-8" or "utf8" in the locale value: a
// mitigation, not a test of the terminal or of the encoding (a value such as en_US.ISO-8859-1@utf8
// passes). With no locale set, or with a UTF-8 locale on a terminal that reads 8-bit controls, a status
// rune such as U+00DB still reaches the terminal as UTF-8 bytes, and its second byte (0x9b) can be read
// as a control sequence.
//
// The package does not log, reads no file and makes no network call.
package waitart
