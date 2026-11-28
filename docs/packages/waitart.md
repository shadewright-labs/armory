# waitart

Package `waitart` (`github.com/shadewright-labs/armory/waitart`) draws a short animated scene with a status line on a terminal while a command waits on a long event, such as a build or an approval. Real output written through the player is never split by a frame. The package uses only the standard library, reads no environment on its own, does not log and makes no network call.

## When it draws

`Config.Start(ctx, w, scene, status)` returns a `*Player` only when `Config.Enabled(w)` is true:

- `Config.LookupEnv` is set. The package reads `NO_COLOR`, `TERM`, `COLORTERM`, `COLUMNS`, `LC_ALL`, `LC_CTYPE` and `LANG` through it; without it nothing is drawn.
- `Config.Off`, the caller's opt-out (for example a `--no-art` flag), is false.
- `NO_COLOR` is unset or empty, and `TERM` is not `dumb`.
- The locale is UTF-8 or not set: the first of `LC_ALL`, `LC_CTYPE` and `LANG` that is set and not empty names UTF-8. A locale such as `C`, `POSIX` or `en_US.ISO-8859-1` turns the art off, because a terminal that reads 8-bit controls would take bytes inside a UTF-8 rune (the second byte of `Û` is `0x9b`, the 8-bit CSI) for a control sequence. This is a mitigation, not a test of the terminal: with no locale set, or a UTF-8 locale on a terminal that reads 8-bit controls, a status rune such as `Û` still reaches the terminal and its second byte can still be read as a control sequence.
- `w` is a terminal at least 64 columns wide, or of unknown width. `Config.Terminal` decides this; when it is nil, an `*os.File` on a character device counts as a terminal, with its width taken from `COLUMNS` when set.

In every other case `Start` returns nil. Every method of a nil `*Player` is safe and does nothing, and `Wrap` returns the writer it was given, so a command needs no check.

## Output and the frame

Real output goes through `Player.Wrap(out)`. Before each write the frame is cleared; once the output ends a line, the frame is drawn again below it. While a line is unfinished no frame is drawn, and while the output ends inside a UTF-8 sequence the player writes nothing at all. All writers that share the terminal, such as standard output and standard error, should go through `Wrap`, because they share one record of whether a line is unfinished.

The frame is drawn as soon as `out` returns, so `out` must pass each write on before it returns. A writer with a `Flush() error` method, such as a `bufio.Writer`, is flushed after each write.

The player's lock is not held while `out` is written or flushed, so `out` may itself write through another writer of the same player, as in `p.Wrap(bufio.NewWriter(p.Wrap(term)))`. A wrapped write that arrives while another is with its writer, nested like that or from another goroutine, passes straight through, and no frame is drawn until a later wrapped write that runs alone ends a line. The player does not order writes made through `Wrap` from several goroutines at once.

Call `Start` once the output so far ends a line. The first frame is drawn from the start of the cursor's line, so a prompt printed before `Start` without a line break is drawn over. `Start` does not check this: the package cannot read the cursor position.

## When a write fails

One rule covers every failed write. After any write or flush error, or a short write, on the terminal or on a writer passed to `Wrap`, the player goes inert for good: it draws no frame, clears nothing, sends no cursor sequence (not even on `Stop`) and retries nothing. The caller's own bytes still pass through `Wrap` as given.

The cost: a failed write can leave the cursor hidden, or part of a frame on the screen. The player does not try to repair either, because a repair write after a partial one can land inside a rune or an escape sequence and corrupt the output that follows. Only the normal path ends with a clean clear and cursor show.

## Width and cancellation

The width is checked before each clear and each frame, after the cursor hide and each clear, and after every write through `Wrap`, with or without a line break. When the terminal narrows below 64 columns or stops being a terminal, the player draws no more frames and shows the cursor, and it does not clear the last frame, since the cursor-up count may no longer be right. A live width needs a `Config.Terminal` callback; the default reads `COLUMNS` once.

No frame is drawn once the context given to `Start` has ended. The context is checked again after a frame is rendered, just before it is written, but a write already under way cannot be called back.

## Stopping and Ctrl-C

`Player.Stop` clears the frame and shows the cursor. It is safe to call twice. The player also stops when the context given to `Start` ends, so a context from `signal.NotifyContext(ctx, os.Interrupt)` restores the terminal on Ctrl-C before the command returns. When the output written through `Wrap` ends inside a UTF-8 sequence at that point, the cursor show waits for the wrapped write that completes the rune; if that write never comes, the cursor stays hidden.

Every terminal write of the player happens under one lock. On a terminal whose writes block, such as a pipe nobody reads, `Stop` and `SetStatus` wait until the blocked write returns. The package does not start a goroutine per write to avoid that.

## Scenes and kinds

A `Scene` has a name, a palette (mask byte to `Colour`, with a 256-colour fallback for terminals that do not announce 24-bit colour in `COLORTERM`), optional `Backdrop` rows drawn under every frame, and up to 64 `Frame` values of at most 6 rows by 64 columns. Each `Row` is text and a mask with one byte per rune. `Scene.Validate` refuses a scene with a control character, an escape sequence, a rune outside the one-column table below, a mask that does not match its text or a size over the limits. Its error names the frame and row by number and never quotes the scene.

A product passes its scenes in a `Set` built with `NewSet(scenes, kinds)`, where `kinds` maps a `Kind` (a string the product chooses) to scene names. `Set.ForKind(kind, src)` picks a fitting scene with the `math/rand/v2` source the caller supplies, so a test with a seeded source gets the same scene every run. A kind with no scenes of its own gets any scene of the set. `Set.Lookup`, `Set.ForKind` and `Set.Scenes` return copies, so a caller can change a scene it got (for example its frame delays) without changing the set.

`Builtin()` returns three neutral scenes for a product without its own: `hourglass`, `relay` and `wave`.

## The status line

The status text comes from the caller, so it is cleaned before it reaches the terminal. Cleaning is not redaction: printable text is shown as given, so pass only status text that is safe to show, never a token or other secret. Line breaks and tabs become spaces. Control characters, escape sequences, zero-width marks and runes that are not printable are dropped. Only the first 1024 bytes are read. Text up to the first colon is drawn in the label colour and the rest in the detail colour (`Config.Style`, or `DefaultStyle`).

The line is cut to 64 terminal columns with an ellipsis, and a two-column rune is never cut in half. A rune counts as one column when it falls in a fixed table of blocks that terminals draw one column wide (Latin, Greek, Cyrillic, general punctuation, currency, letterlike symbols, arrows, mathematical operators, box drawing, block elements, most geometric shapes and Braille), and as two columns otherwise. East Asian ambiguous runes inside the table, such as `°`, `é`, `α`, `─` and the ellipsis `…`, also count as two columns, because a terminal set up for East Asian text may draw them two wide. A terminal that draws a rune as one column ends up with a shorter line, never a wrapped one.

Frames follow a stricter rule: `Scene.Validate` accepts only runes from the table, each counted as one column. Ambiguous runes such as box drawing and block elements are allowed in frames. This is a known limit: a terminal set to draw ambiguous runes two columns wide draws such a row wider than 64 columns (the built-in `relay` and `wave` scenes reach 104 columns there), the row wraps, and each redraw leaves stale art above the frame. On such a terminal, turn the art off with `Config.Off`.
