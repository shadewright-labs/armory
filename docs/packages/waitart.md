# waitart

Package `waitart` (`github.com/shadewright-labs/armory/waitart`) draws a short animated scene with a status line on a terminal while a command waits on a long event, such as a build or an approval. Real output written through the player is never split by a frame. The package uses only the standard library, reads no environment on its own, does not log and makes no network call.

## When it draws

`Config.Start(ctx, w, scene, status)` returns a `*Player` only when `Config.Enabled(w)` is true:

- `Config.LookupEnv` is set. The package reads `NO_COLOR`, `TERM`, `COLORTERM`, `COLUMNS`, `LC_ALL`, `LC_CTYPE` and `LANG` through it; without it nothing is drawn.
- `Config.Off`, the caller's opt-out (for example a `--no-art` flag), is false.
- `NO_COLOR` is unset or empty, and `TERM` is not `dumb`.
- The locale is UTF-8 or not set: the first of `LC_ALL`, `LC_CTYPE` and `LANG` that is set and not empty names UTF-8. A locale such as `C`, `POSIX` or `en_US.ISO-8859-1` turns the art off, because a terminal that reads 8-bit controls would take bytes inside a UTF-8 rune (the second byte of `Û` is `0x9b`, the 8-bit CSI) for a control sequence. The check is a case-insensitive substring match for `utf-8` or `utf8` in the value, so it does not identify the encoding (a value such as `en_US.ISO-8859-1@utf8` passes). It is a mitigation, not a test of the terminal: with no locale set, or a UTF-8 locale on a terminal that reads 8-bit controls, a status rune such as `Û` still reaches the terminal and its second byte can still be read as a control sequence.
- `w` is a terminal at least 64 columns wide, or of unknown width. `Config.Terminal` decides this; when it is nil, an `*os.File` on a character device counts as a terminal, with its width taken from `COLUMNS` when set.

In every other case `Start` returns nil. Every method of a nil `*Player` is safe and does nothing, and `Wrap` returns the writer it was given, so a command needs no check.

## Output and the frame

Real output goes through `Player.Wrap(out)`. Before each write the frame is cleared; once the output ends a line, the frame is drawn again below it. While a line is unfinished no frame is drawn, and while the output ends inside a UTF-8 sequence the player writes nothing at all. All writers that share the terminal, such as standard output and standard error, should go through `Wrap`, because they share one record of whether a line is unfinished.

Each wrapped write holds the player's lock for the whole write: the clear, the write to `out`, its flush and the redraw. The player never draws, clears or sends a cursor sequence while `out` is written, and writes through `Wrap`, from any goroutine and through any writer of the same player, run one at a time, so a shared `bufio.Writer` needs no lock of its own. `out` must pass each write on before it returns; a writer with a `Flush() error` method, such as a `bufio.Writer`, is flushed after each write.

A buffered `out` must start empty and be written only through the wrapper. If a `bufio.Writer` already holds a prompt when it is wrapped, the first wrapped write flushes the prompt while the frame is on the screen, the player does not know about it, and a later redraw can erase it.

The player does not support re-entry. A writer passed to `Start` or `Wrap`, and the `Config.Terminal` callback, must not call back into the same player while the player runs them, from any goroutine: no write through a writer of that player's `Wrap`, no `Stop` and no `SetStatus`, and no waiting on another goroutine that makes such a call. The call waits on the lock that write holds and may deadlock. The package catches one case, statically: `Wrap` given a writer it made for the same player, as in `p.Wrap(p.Wrap(term))`, returns a writer whose every write fails with `ErrNestedWrap`, writes nothing and has no effect on the player. Nesting through another writer, as in `p.Wrap(bufio.NewWriter(p.Wrap(term)))`, is not detected.

The writer given to `Start` must deliver each write before it returns. The player never flushes it, so a buffered writer there could still hold a frame or the cursor show after `Stop` returns.

The player tracks UTF-8 boundaries, not escape sequences. A caller must not leave a terminal escape sequence open across writes (`"\x1b[31"` in one write and `"m"` in the next) while `Stop` can run, the context can end or the terminal can narrow, because the cursor show would land inside it. A narrowing counts too: the width check after each wrapped write can find the terminal narrower and send the cursor show before the caller's next write.

Call `Start` once the output so far ends a line. The first frame is drawn from the start of the cursor's line, so a prompt printed before `Start` without a line break is drawn over. `Start` does not check this: the package cannot read the cursor position.

## When a write fails

One rule covers every failed write. After any write or flush error, or a short write, on the terminal or on a writer passed to `Wrap` (a failed zero-length write included), and after such a writer panics, the player goes inert for good: it draws no frame, clears nothing, sends no cursor sequence (not even on `Stop`) and retries nothing. The caller's own bytes still pass through `Wrap` as given, one write at a time.

The cost: a failed write can leave the cursor hidden, or part of a frame on the screen. The player does not try to repair either, because a repair write after a partial one can land inside a rune or an escape sequence and corrupt the output that follows. Only the normal path ends with a clean clear and cursor show.

A panic in a writer passed to `Start` or `Wrap`, or in the cleanup of `Stop`, reaches the caller with the player's lock given up, so a later wrapped write or `Stop` from any goroutine does not hang. A panic on the player's own frame goroutine ends the program, like any unrecovered panic.

## Width

The width is checked at these points:

- in `Enabled`, before the first frame;
- before each frame is written, after it is rendered, so after the cursor hide at start and after the clear of each redraw;
- before a frame is cleared: at each frame tick, only while no partial output line is on the screen, at the start of each non-empty wrapped write, and in `Stop`;
- after each non-empty wrapped write that succeeds, with or without a line break.

It is not checked after a frame is written, nor between the clear at the start of a wrapped write and the end of that write. A terminal that narrows then is seen at the next check, and until then the cursor stays hidden. While a partial output line is on the screen, frame ticks do not check the width, so a narrowing is seen only at the next wrapped write or in `Stop`, which can be much later than one frame delay. When the terminal is narrower than 64 columns or stops being a terminal, the player draws no more frames and shows the cursor, and it does not clear the last frame, since the cursor-up count may no longer be right. A live width needs a `Config.Terminal` callback; the default reads `COLUMNS` once.

## Stopping and Ctrl-C

`Player.Stop` clears the frame and shows the cursor, and returns once that cleanup is written. It is safe to call twice. The player also stops when the context given to `Start` ends, but that starts the cleanup on the player's goroutine without waiting for it. To restore the terminal on Ctrl-C, pass a context from `signal.NotifyContext(ctx, os.Interrupt)` and call `Stop` before the command returns.

Cleanup is best effort. What is guaranteed is the last check: the stop and the context are checked after a frame is rendered, just before it is written, and no frame is written once either is seen there. A cancel after that check, or during the write, does not call the write back; the cleanup clears that frame. There is no cleanup after a failed write (the player is inert), and when the output written through `Wrap` ends inside a UTF-8 sequence, the cursor show waits for the wrapped write that completes the rune; if that write never comes, the cursor stays hidden.

Every terminal write of the player, and every wrapped write, happens under one lock. On a writer that blocks, such as a pipe nobody reads, `Stop` and `SetStatus` wait until the blocked write returns. The package does not start a goroutine per write to avoid that.

## Scenes and kinds

A `Scene` has a name, a palette (mask byte to `Colour`, with a 256-colour fallback for terminals that do not announce 24-bit colour in `COLORTERM`), optional `Backdrop` rows drawn under every frame, and up to 64 `Frame` values of at most 6 rows by 64 columns. Each `Row` is text and a mask with one byte per rune. `Scene.Validate` refuses a scene with a control character, an escape sequence, a rune outside the one-column table below, a mask that does not match its text or a size over the limits. Its error names the frame and row by number and never quotes the scene.

A product passes its scenes in a `Set` built with `NewSet(scenes, kinds)`, where `kinds` maps a `Kind` (a string the product chooses) to scene names. `Set.ForKind(kind, src)` picks a fitting scene with the `math/rand/v2` source the caller supplies, so a test with a seeded source gets the same scene every run. A kind with no scenes of its own gets any scene of the set. `Set.Lookup`, `Set.ForKind` and `Set.Scenes` return copies, so a caller can change a scene it got (for example its frame delays) without changing the set.

`Builtin()` returns three neutral scenes for a product without its own: `hourglass`, `relay` and `wave`.

## The status line

The status text comes from the caller, so it is cleaned before it reaches the terminal. Cleaning is not redaction: printable text is shown as given, so pass only status text that is safe to show, never a token or other secret. Line breaks and tabs become spaces. Control characters, escape sequences, zero-width marks and runes that are not printable are dropped. Only the first 1024 bytes are read. Text up to the first colon is drawn in the label colour and the rest in the detail colour (`Config.Style`, or `DefaultStyle`).

The line is cut to 64 terminal columns with an ellipsis, and a two-column rune is never cut in half. A rune counts as one column when it falls in a fixed table of blocks that terminals draw one column wide (Latin, Greek, Cyrillic, general punctuation, currency, letterlike symbols, arrows, mathematical operators, box drawing, block elements, most geometric shapes and Braille), and as two columns otherwise. East Asian ambiguous runes inside the table, such as `°`, `é`, `α`, `─` and the ellipsis `…`, also count as two columns, because a terminal set up for East Asian text may draw them two wide. A terminal that draws a rune as one column ends up with a shorter line, never a wrapped one.

Frames follow a stricter rule: `Scene.Validate` accepts only runes from the table, each counted as one column, and the width checks compare the terminal with 64 columns. Ambiguous runes such as box drawing and block elements are allowed in frames. This is a known limit: on a terminal set to draw ambiguous runes two columns wide, a row is as wide as its other runes plus twice its ambiguous ones (the built-in `relay` scene reaches 102 columns there and `wave` 104). Such a row wraps only when that width is more than the terminal's columns, and then each redraw leaves stale art above the frame. On such a terminal, turn the art off with `Config.Off`.
