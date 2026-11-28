# waitart

Package `waitart` (`github.com/shadewright-labs/armory/waitart`) draws a short animated scene with a status line on a terminal while a command waits on a long event, such as a build or an approval. Real output written through the player is never split by a frame. The package uses only the standard library, reads no environment on its own, does not log and makes no network call.

## When it draws

`Config.Start(ctx, w, scene, status)` returns a `*Player` only when `Config.Enabled(w)` is true:

- `Config.LookupEnv` is set. The package reads `NO_COLOR`, `TERM`, `COLORTERM`, `COLUMNS`, `LC_ALL`, `LC_CTYPE` and `LANG` through it; without it nothing is drawn.
- `Config.Off`, the caller's opt-out (for example a `--no-art` flag), is false.
- `NO_COLOR` is unset or empty, and `TERM` is not `dumb`.
- The locale is UTF-8 or not set: the first of `LC_ALL`, `LC_CTYPE` and `LANG` that is set and not empty names UTF-8. A locale such as `C`, `POSIX` or `en_US.ISO-8859-1` turns the art off, because a terminal that reads 8-bit controls would take bytes inside a UTF-8 rune (the second byte of `Û` is `0x9b`, the 8-bit CSI) for a control sequence.
- `w` is a terminal at least 64 columns wide, or of unknown width. `Config.Terminal` decides this; when it is nil, an `*os.File` on a character device counts as a terminal, with its width taken from `COLUMNS` when set.

In every other case `Start` returns nil. Every method of a nil `*Player` is safe and does nothing, and `Wrap` returns the writer it was given, so a command needs no check.

## Output and the frame

Real output goes through `Player.Wrap(out)`. Before each write the frame is cleared; once the output ends a line, the frame is drawn again below it. While a line is unfinished no frame is drawn, and while the output ends inside a UTF-8 sequence the player writes nothing at all, not even the cursor show on `Stop`: that waits for the write that completes the rune. All writers that share the terminal, such as standard output and standard error, should go through `Wrap`, because they share one record of whether a line is unfinished.

The frame is drawn as soon as `out` returns, so `out` must pass each write on before it returns. A writer with a `Flush() error` method, such as a `bufio.Writer`, is flushed after each write; when the flush fails, no frame is drawn until a later line ends.

Call `Start` once the output so far ends a line. The first frame is drawn from the start of the cursor's line, so a prompt printed before `Start` without a line break is drawn over; the package cannot read the cursor position to tell.

The width is checked again before each frame, after each wrapped write and before the clear on `Stop`. When a write to the terminal fails or comes up short, or the terminal narrows below 64 columns, the player stops drawing and shows the cursor, and it does not clear the last frame, since the cursor-up count may no longer be right. No frame is drawn once the context given to `Start` has ended, even before the player has stopped.

## Stopping and Ctrl-C

`Player.Stop` clears the frame and shows the cursor. It is safe to call twice; when the terminal took only part of the cursor show, a second `Stop` or the next wrapped write sends the rest. The player also stops when the context given to `Start` ends, so a context from `signal.NotifyContext(ctx, os.Interrupt)` restores the terminal on Ctrl-C before the command returns.

Every terminal write happens under one lock. On a terminal whose writes block, such as a pipe nobody reads, `Stop`, `SetStatus` and wrapped writes wait until the blocked write returns. The package does not start a goroutine per write to avoid that.

## Scenes and kinds

A `Scene` has a name, a palette (mask byte to `Colour`, with a 256-colour fallback for terminals that do not announce 24-bit colour in `COLORTERM`), optional `Backdrop` rows drawn under every frame, and up to 64 `Frame` values of at most 6 rows by 64 columns. Each `Row` is text and a mask with one byte per rune. `Scene.Validate` refuses a scene with a control character, an escape sequence, a rune outside the one-column table below, a mask that does not match its text or a size over the limits. Its error names the frame and row by number and never quotes the scene.

A product passes its scenes in a `Set` built with `NewSet(scenes, kinds)`, where `kinds` maps a `Kind` (a string the product chooses) to scene names. `Set.ForKind(kind, src)` picks a fitting scene with the `math/rand/v2` source the caller supplies, so a test with a seeded source gets the same scene every run. A kind with no scenes of its own gets any scene of the set. `Set.Lookup`, `Set.ForKind` and `Set.Scenes` return copies, so a caller can change a scene it got (for example its frame delays) without changing the set.

`Builtin()` returns three neutral scenes for a product without its own: `hourglass`, `relay` and `wave`.

## The status line

The status text comes from the caller, so it is cleaned before it reaches the terminal. Cleaning is not redaction: printable text is shown as given, so pass only status text that is safe to show, never a token or other secret. Line breaks and tabs become spaces. Control characters, escape sequences, zero-width marks and runes that are not printable are dropped. Only the first 1024 bytes are read. Text up to the first colon is drawn in the label colour and the rest in the detail colour (`Config.Style`, or `DefaultStyle`).

The line is cut to 64 terminal columns with an ellipsis, and a two-column rune is never cut in half. A rune counts as one column when it falls in a fixed table of blocks that terminals draw one column wide (Latin, Greek, Cyrillic, general punctuation, currency, letterlike symbols, arrows, mathematical operators, box drawing, block elements, most geometric shapes and Braille), and as two columns otherwise. East Asian ambiguous runes inside the table, such as `°`, `é`, `α`, `─` and the ellipsis `…`, also count as two columns, because a terminal set up for East Asian text may draw them two wide. A terminal that draws a rune as one column ends up with a shorter line, never a wrapped one.

Frames follow a stricter rule: `Scene.Validate` accepts only runes from the table, each counted as one column. Ambiguous runes such as box drawing and block elements are allowed in frames, so a terminal set to draw ambiguous runes two columns wide draws such a frame wider than 64 columns, and its redraw leaves stale art.
