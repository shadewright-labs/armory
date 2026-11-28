# waitart

Package `waitart` (`github.com/shadewright-labs/armory/waitart`) draws a short animated scene with a status line on a terminal while a command waits on a long event, such as a build or an approval. Real output written through the player is never split by a frame. The package uses only the standard library, reads no environment on its own, does not log and makes no network call.

## When it draws

`Config.Start(ctx, w, scene, status)` returns a `*Player` only when `Config.Enabled(w)` is true:

- `Config.LookupEnv` is set. The package reads `NO_COLOR`, `TERM`, `COLORTERM` and `COLUMNS` through it; without it nothing is drawn.
- `Config.Off`, the caller's opt-out (for example a `--no-art` flag), is false.
- `NO_COLOR` is unset or empty, and `TERM` is not `dumb`.
- `w` is a terminal at least 64 columns wide, or of unknown width. `Config.Terminal` decides this; when it is nil, an `*os.File` on a character device counts as a terminal, with its width taken from `COLUMNS` when set.

In every other case `Start` returns nil. Every method of a nil `*Player` is safe and does nothing, and `Wrap` returns the writer it was given, so a command needs no check.

## Output and the frame

Real output goes through `Player.Wrap(out)`. Before each write the frame is cleared; once the output ends a line, the frame is drawn again below it. While a line is unfinished no frame is drawn. All writers that share the terminal, such as standard output and standard error, should go through `Wrap`, because they share one record of whether a line is unfinished.

The width is checked again before each frame. When a write to the terminal fails or comes up short, or the terminal narrows below 64 columns, the player stops drawing and shows the cursor, and it does not clear the last frame, since the cursor-up count may no longer be right.

## Stopping and Ctrl-C

`Player.Stop` clears the frame and shows the cursor. It is safe to call twice. The player also stops when the context given to `Start` ends, so a context from `signal.NotifyContext(ctx, os.Interrupt)` restores the terminal on Ctrl-C before the command returns.

## Scenes and kinds

A `Scene` has a name, a palette (mask byte to `Colour`, with a 256-colour fallback for terminals that do not announce 24-bit colour in `COLORTERM`), optional `Backdrop` rows drawn under every frame, and up to 64 `Frame` values of at most 6 rows by 64 columns. Each `Row` is text and a mask with one byte per rune. `Scene.Validate` refuses a scene with a control character, an escape sequence, a rune outside the one-column table below, a mask that does not match its text or a size over the limits. Its error names the frame and row by number and never quotes the scene.

A product passes its scenes in a `Set` built with `NewSet(scenes, kinds)`, where `kinds` maps a `Kind` (a string the product chooses) to scene names. `Set.ForKind(kind, src)` picks a fitting scene with the `math/rand/v2` source the caller supplies, so a test with a seeded source gets the same scene every run. A kind with no scenes of its own gets any scene of the set.

`Builtin()` returns three neutral scenes for a product without its own: `hourglass`, `relay` and `wave`.

## The status line

The status text comes from the caller, so it is cleaned before it reaches the terminal. Line breaks and tabs become spaces. Control characters, escape sequences, zero-width marks and runes that are not printable are dropped. Only the first 1024 bytes are read. Text up to the first colon is drawn in the label colour and the rest in the detail colour (`Config.Style`, or `DefaultStyle`).

The line is cut to 64 terminal columns with an ellipsis, and a two-column rune is never cut in half. A rune counts as one column when it falls in a fixed table of blocks that terminals draw one column wide (Latin, Greek, Cyrillic, general punctuation, currency, letterlike symbols, arrows, mathematical operators, box drawing, block elements, most geometric shapes and Braille), and as two columns otherwise. A terminal that draws a rune outside the table as one column ends up with a shorter line, never a wrapped one. Ambiguous-width runes such as `°` and `…` count as one column, which is how terminals draw them outside East Asian locales.
