# ADR-0003: Package `redact/exact`, exact redaction of held secrets

- Status: Proposed
- Date: 2026-11-27

This record sets the rules and the contract of `github.com/shadewright-labs/armory/redact/exact` before any code exists (epic 44, issue 45): what a product registers, how long a value stays registered and who renews or retires it, the shortest value accepted, how a held value is found in text, and how the package fails. It is a different package from `redact` (ADR-0002): this one matches values the product holds and promises to hide them, the pattern redactor is best effort. Armory does not chain the two; the product does.

## Context

### What each product has

Sablecourt, pull request 190 (issue 180, open), branch `180-exact-secrets` at `8cd2d6b`: `internal/redact/known.go`, 806 lines, with 444 lines of tests in the package, reachability tests in three packages (`internal/runtime/coding`, `internal/runtime/docker`, `internal/runtime/k8s`) and a redaction matrix in `internal/security`. It keeps a `Registry` of values, derives quoted, JSON-escaped and line-stripped forms, builds an Aho-Corasick automaton over them and replaces each match with `[REDACTED:known_secret]`. Nineteen production files call it in 39 places, nearly all through the package-level `DefaultRegistry` and its wrapper functions; `internal/attach` also accepts an explicit `*Registry`. Sablecourt ADR-0015 makes exact match of daemon-held secrets the guarantee at its redacting boundaries and the pattern rules defence in depth.

The pull request's next revision, in progress on 2026-11-27 and not yet pushed (`internal/redact/exactjson.go`), replaces `RedactExactJSON` with one rule for every boundary. It is the main input for point 5 of this record:

- A value is checked through views: the raw text; the text with every JSON string escape undone, repeatedly, up to 16 levels, for JSON carried inside JSON strings; that text with invalid UTF-8 replaced by U+FFFD as decoders do; and each number literal written as its exact decimal and as a float64.
- Input that no view shows a held value in comes back byte for byte.
- Strictly valid JSON (one value, valid UTF-8, no duplicate names) is rewritten token by token with `encoding/json/jsontext`: each string or key that shows a held value is redacted by the same rule, recursively, and each such number becomes the marker string. Plain text gets held values replaced in its bytes by the derived forms.
- The output is checked again through the views. If any view still shows a held value, or the input is JSON that is not strictly valid, the whole value is replaced by the marker (as a JSON string when the input was JSON).

Its tests cover the four cases of the commit at `8cd2d6b` (a value ending in `"`, a value ending in `\`, a value whose replacement would span two fields, and ASCII written as `l`) and generated escape variants. The commit message of `8cd2d6b` gives the reason for decoding: matching every escaped form instead "is not possible for arbitrary escapes". Its token rewrite re-encodes every string and drops whitespace between tokens once a document has a hit: on Go 1.27.4, `{"z": 1,  "a": "Ab"}` comes back as `{"z":1,"a":"Ab"}`. Streams in that revision still match derived forms only.

Shadewright has nothing like it. Its processes hold model provider API keys and GitHub App credentials (loaded through `internal/providers`), and its only protection in output is the pattern redactor.

### What the first version got wrong

Sablecourt issue 189 lists the gaps. The ones that belong to the package, measured on 2026-11-27 by running `known.go` from `8cd2d6b` outside Sablecourt:

- Registration cost grows with the square of the registry. Every registration rebuilds the whole automaton. Registering 250, 1,000 and 4,000 task-token-shaped values one at a time took 20 ms, 353 ms and 5.6 s: 79 µs, 353 µs and 1.4 ms per value.
- Nothing is retired. Deleting a value does not remove it; only an expired value with an expiry is dropped, one hour later, and only when another value is registered. Rotation of a provider key or a logout key adds a permanent entry each time.
- Short values are dropped in silence. `Register` ignores a value under 8 trimmed bytes and returns nothing, so a 7-byte key is registered as nothing and the caller does not know (`Len` stayed 0 in the run).
- The scanner skips CR, LF and the two-byte escapes `\r` and `\n` in the text. A value that ends in a backslash, followed in the text by the letter `n`, loses its last byte to the skip and is not found. The same value followed by a space is found.
- `RedactExactJSON` decodes a document, redacts its strings and encodes it again. Keys come back sorted and whitespace is lost: `{"z": 1, "a": "<value>",   "m": [1, 2]}` became `{"a":"[REDACTED:known_secret]","m":[1,2],"z":1}`. Two object keys that are both registered values collapse into one key, and one of the two entries is lost. The decode itself has no size bound.
- State sits in the struct. `String`, `GoString` and `MarshalJSON` are on the pointer receiver, so a dereferenced `Registry` printed with `%+v` shows the map, and the map keys are the values.
- One process-wide `DefaultRegistry`, which every test in the process shares.
- `Stream.Write(raw, eof)` holds back a possible partial match with no way to flush on idle, so live output lags by up to the longest value. When the held bytes pass 64 KiB it writes one marker and starts again from the root of the automaton, so the rest of a value cut there is written in clear.

Matching time is linear and is not a defect: 1 MiB of near-misses took 3 ms and 8 MiB took 26 ms.

### Invariants touched

1. The package knows no product's secrets or owners; both are arguments.
2. Standard library only.
3. The registry holds secrets by design, so formatting, errors and stats must never show one. It reads nothing the caller did not pass.
4. It reads untrusted text, so its work is bounded by limits the caller sets and it is fuzzed.
5. A new public API.

## Decision

### 1. Boundary

- `redact/exact` replaces values the caller registered, in the views and forms of points 4 and 5. It does not look for credential shapes, does not parse configuration, and does not decide which values a product holds. Pulling the password out of a database connection string, or reading a token file, stays in the product.
- It does not import `redact` and `redact` does not import it. A product that uses both runs `exact` first, then the pattern redactor on the result.
- There is no package-level registry, default or state. A product builds its own `Registry` and passes it to every place that redacts.

### 2. Public API

```go
package exact

const (
	MinLengthFloor       = 8
	DefaultMarker        = "[REDACTED:known-secret]"
	DefaultMaxValues     = 4096
	DefaultMaxValueBytes = 8 << 10
	DefaultMaxTotalBytes = 256 << 10
	DefaultMaxFormBytes  = 4 << 20
	DefaultMaxInput      = 16 << 20
	DefaultMaxHold       = 64 << 10
)

type Options struct {
	MinLength     int              // 0: MinLengthFloor; below the floor is ErrInvalidOptions
	MaxValues     int              // (owner, value) pairs live or retained; 0: DefaultMaxValues
	MaxValueBytes int              // one value; 0: DefaultMaxValueBytes
	MaxTotalBytes int              // distinct values live or retained, each counted once; 0: DefaultMaxTotalBytes
	MaxFormBytes  int              // all derived forms of those values; 0: DefaultMaxFormBytes
	MaxInput      int              // one Redact or Contains input; 0: DefaultMaxInput
	Retention     time.Duration    // matched for at least this long after expiry or retirement; 0: not at all
	Marker        string           // "": DefaultMarker; must match \[REDACTED:[a-z][a-z0-9-]{0,31}\]
	MaxHold       int              // bytes a Stream may hold back; 0: DefaultMaxHold; below 7*MaxValueBytes is ErrInvalidOptions
	Now           func() time.Time // nil: time.Now
}

func New(o Options) (*Registry, error)

type Registry struct{ h *handle } // handle holds only `get func() *state`

func (r *Registry) Add(owner string, expires time.Time, values ...string) error
func (r *Registry) AddBytes(owner string, expires time.Time, values ...[]byte) error
func (r *Registry) Replace(owner string, expires time.Time, values ...string) error
func (r *Registry) ReplaceBytes(owner string, expires time.Time, values ...[]byte) error
func (r *Registry) Renew(owner string, expires time.Time) error
func (r *Registry) Retire(owner string) int
func (r *Registry) Stats() Stats // counts only: pairs live and retained, owners, value bytes, form bytes, tiers

type Result struct {
	Text  string // the text to use; empty only with an error
	Hits  int    // spans replaced, or 1 when Whole
	Whole bool   // the whole input was replaced by the marker
}

func (r *Registry) Redact(s string) (Result, error)
func (r *Registry) Contains(s string) (bool, error)
func (r *Registry) NewStream(w io.Writer) *Stream

type Stream struct{ h *streamHandle } // streamHandle holds only `get func() *streamState`
func (s *Stream) Write(p []byte) (int, error)
func (s *Stream) Flush() error
func (s *Stream) Close() error

var (
	ErrInvalidOptions, ErrInvalidOwner, ErrInvalidExpiry, ErrUnknownOwner error
	ErrTooShort, ErrTooLong, ErrOverlapsMarker, ErrFull                   error
	ErrTooLarge, ErrUnavailable, ErrClosed                                error
)
```

A `Registry` is safe for concurrent use. `Redact` runs without a lock on an immutable matcher. A `Stream` is not safe for concurrent use, like most writers. Callers with `[]byte` text convert it; the copy is bounded by `MaxInput`.

State layout. A `Registry` and a `Stream` keep their state behind a function value, as `githost.Token` does (ADR-0001). When a caller stores either as an unexported field of its own struct and prints that struct, `fmt` walks the fields without calling their methods; with a verb a pointer does not take, such as `%s` or `%q`, it prints the pointed-to value (checked on Go 1.27.4: a `struct{ s *state }` field printed with `%s` shows the map inside `state`). A function value prints as an address. Both types have value-receiver `String`, `GoString`, `Format`, `MarshalText`, `MarshalJSON` and `LogValue`, all returning a fixed placeholder; `gob` refuses them because they have no exported fields.

### 3. Owners, expiry and retirement

- Every value has an owner: a string of 1 to 128 printable ASCII bytes that the product chooses, such as `task:42` or `provider:anthropic`. An empty or longer owner is `ErrInvalidOwner`. The owner is never printed, logged or put in an error.
- A value has an expiry, or none when `expires` is zero. An expiry not after `Now()` is `ErrInvalidExpiry`.
- `Add`, `AddBytes`, `Replace` and `ReplaceBytes` are all or nothing on every error. When one value of the call is refused, or a cap would be passed, nothing changes: no value is added and, for `Replace`, the owner's old values stay live. The error names the value's position in the call and nothing else.
- An `Add` with no values does nothing and returns nil. A value given twice in one call counts once. Adding a value the owner already holds keeps one entry with the later of the two expiries (no expiry counts as latest).
- `Renew(owner, expires)` moves the expiry of the owner's live values that have one to `expires` when that is later. It never shortens one and leaves values with no expiry alone. An owner with no live value is `ErrUnknownOwner`. Renewing does not bring back an expired or retired value; the product adds it again.
- `Retire(owner)` ends the owner's live values now and returns how many it ended. A retired value is still matched for at least `Retention`, so text produced just before the rotation is still hidden. Retiring an unknown owner returns 0.
- `Replace(owner, expires, values...)` retires the owner's values and adds the new ones in one step. It is the call for rotation. With no values it is `Retire`.
- The same value may be registered by several owners. It is matched while any of them holds it live or retained, and one owner's `Retire` does not drop it for the others.
- An expired value is matched for at least `Retention` after its expiry. Sablecourt passes one hour, as it does today. `Retention` is the product's choice because only the product knows how long old output can still be on its way.
- Caps. Each (owner, value) pair live or retained counts against `MaxValues`; each distinct value counts once against `MaxTotalBytes`, and its derived forms against `MaxFormBytes`. Pairs past their retention are pruned before a cap is checked. A call that would pass a cap is `ErrFull` and changes nothing. The product decides what that means; for a credential it is about to hand out, the safe answer is to refuse to hand it out.

### 4. Values and forms

**Minimum length.** A value is refused with `ErrTooShort` when it is shorter than `MinLength` bytes, and also when it is shorter than `MinLength` after removing its CR and LF bytes, or after removing its two-byte texts `\r` and `\n` (the bytes the split views of point 5 skip). It is never dropped in silence. The caller may raise the minimum and may not set it below 8 bytes. The reasons for 8:

- Under 8 bytes, exact match starts to hit ordinary text: short words, numbers, ports, `admin`, `true`. A false positive replaces text a reader needs, and the rate depends on the value more than on its length, so the floor is a policy bound and not a measured threshold.
- The values the products generate are longer: Sablecourt's task tokens are 72 bytes (`sc_task_` and 64 hexadecimal), the operator token 67 (`op_` and 64), pairing codes 37 (`pair_` and 32). Commercial provider keys are 40 bytes or more. Secrets a user configures (a provider key, a DSN password, a webhook token) have no minimum today; they gain the 8-byte check where they are entered.
- A secret under 8 bytes is weak whatever redaction does. Refusing it tells the product to refuse such a credential where it is configured.
- The floor bounds how much output can grow (point 7).

**Marker overlap.** A value is refused with `ErrOverlapsMarker` when any of its forms (below), with the bytes of either split view removed, is contained in the marker or in the marker as a JSON string (`"[REDACTED:known-secret]"`), begins with a proper suffix of either, or ends with a proper prefix of either. The check runs on every form, not only on the value: `REDACTED  ` trims to `REDACTED`, `REDA\nCTED` loses its line break in a split view, and the 9-byte value whose URL-safe base64 is `known-secret` would all match inside the default marker on a second pass. Without this rule idempotence would not hold.

**Forms.** Each value is written out in the forms it is likely to appear in. They are what a `Stream` matches, and what `Redact` replaces in plain text before its views check the result (point 5). Forms are derived from `v` and, when `strings.TrimSpace(v)` differs and is at least `MinLength` bytes, from the trimmed value too (`8cd2d6b` registers an untrimmed provider key and sends it trimmed). For each of the two:

1. The value itself.
2. The content of a JSON string (without the quotes) as `encoding/json` writes it without HTML escaping; the same with HTML escaping (`json.Marshal`'s default, `<`, `>`, `&`); with every non-ASCII character as `\uXXXX`, characters above U+FFFF as a UTF-16 surrogate pair `\uD8XX\uDCXX` (what Python's `json.dumps` writes by default); and each of those three with `/` written as `\/`.
3. Each content of form 2 written again as the content of a JSON string, for JSON carried inside a JSON string. Deeper nesting is left to the views of `Redact`.
4. The content of `strconv.Quote(v)`, and `v` with `'` written as `\'`.
5. `url.QueryEscape(v)` and `url.PathEscape(v)`.
6. Hexadecimal in lower and upper case.
7. Base64 in the standard and URL alphabets, padded and unpadded, and for a value inside a larger base64 text (a `Basic` header, a base64 config file) the stable middle at offsets 1 and 2. For offset `r`, the value is encoded after `r` zero bytes; the first 0, 2 or 3 characters (for `r` 0, 1, 2) depend on the zero bytes and are dropped, and the last character and the padding are dropped when `(r + len(v)) % 3 != 0`, since they depend on what follows. What is left depends only on `v` (10 characters for an 8-byte value at every offset). Up to 4 bits of the first byte and 4 bits of the last byte stay in the neighbouring characters of the encoded text; that is a known limit.

Forms are deduplicated, and a form shorter than `MinLength`, in full or with either view's bytes removed, is not added. `AddBytes` derives the same forms from bytes; form 1 of a value that is not valid UTF-8 stays as its raw bytes. A form is at most 7 times its value (form 3 of a `\u00XX` escape), so 7 times `MaxValueBytes`; `MaxFormBytes` bounds the total, since escape-heavy values can have forms 40 to 60 times their length in all.

**Split forms.** Each form is also found when line breaks were put between any of its bytes, any number of them. The matcher reads the text in two views, each of which skips one class of bytes and rewrites nothing:

- view A skips the bytes CR and LF;
- view B skips the two-byte escapes `\r` and `\n`, reading `\\` as two fed bytes so an escaped backslash is never taken for the start of an escape.

A form is matched in a view against that form with the same class removed. A match covers the text from its first fed byte to its last, skipped bytes included. The union of the two views' matches is replaced. Every contiguous occurrence of a form is found by view A whatever surrounds it, so the backslash case of the first version cannot recur.

Limits of the split views: skipped bytes before a form's first fed byte or after its last stay in the output (a line break at the edge of a value is not hidden); and a form split by `\r` or `\n` whose own bytes run into the escape is not found, namely a form ending in `\` with an inserted escape right after a byte of it and the text going on with `n` or `r`, or a form starting with `n` or `r` right after an inserted escape. Such a form is found when it is contiguous. Also out of scope: a form split by anything other than line breaks, a value cut by a line continuation (`\` and a line break), case changes other than hexadecimal, and text written before the value was registered.

### 5. How `Redact` and `Contains` find a value

Two rules were compared, on Sablecourt's code at `8cd2d6b` and its revision in progress:

- Match the bytes as given (the first draft of this record): each form is matched in the text and nothing is decoded. Its cost is that it cannot find every escape a decoder accepts. Any ASCII byte may be written as `\u00XX` in either case of hex digits, and a value of `n` bytes has `2^n` or more such spellings, so no finite set of forms covers them; Sablecourt's test with `l` fails under it. It also cannot keep JSON valid: a raw form ending in `"` or `\` matches across the end of a JSON string, and its marker eats the closing quote. Nothing checks the result, so a miss is silent.
- Decoded views with one output check (Sablecourt's revision): a decoder can read nothing that is not in the raw text or in some level of its unescaped text, so the views over-approximate what a reader sees, and the output check makes a miss loud: the whole value goes. Its costs are a lost value when the check fails (a log line that holds a held value as `s...` in plain text is replaced whole, because a replacement in its bytes cannot remove an escaped spelling), more work per call (up to 17 levels of views instead of one pass), and a JSON tokenizer in the package.

The shared package takes decoded views, because only that rule fails closed, and both products' boundaries carry JSON. The rule of `Redact`:

1. An input over `MaxInput` bytes is `ErrTooLarge` with no text.
2. Views: the raw text; the text with every JSON string escape undone, applied again while it changes, up to 16 levels (text that still changes after 16 counts as showing a held value); each of those with invalid UTF-8 replaced by U+FFFD, per byte as `encoding/json` does and per run as `strings.ToValidUTF8` does, when some form holds U+FFFD; and each number literal written as its exact decimal (exponents beyond 1,024 count as showing a value) and as a float64 in the `f`, `g` and `e` formats, when some form is made only of number bytes. Each view is read with both split views of point 4.
3. Input that no view shows a held value in comes back byte for byte, with `Hits` 0.
4. Input that is strictly valid JSON (RFC 8259 with the I-JSON limits: one value, valid UTF-8, no duplicate names, nesting at most 512 levels) is tokenized with `encoding/json/jsontext`. Each string or key that shows a held value is decoded, redacted by this same rule, and written back in place of its token at the token's byte offsets; each number that shows one becomes the marker as a JSON string. Whitespace and every token that does not change keep their bytes. This splice is where Armory differs from Sablecourt's revision, which re-encodes the whole document.
5. Other input is plain text: each match of a form (point 4) is replaced by the marker, overlapping matches merged into one span and touching matches kept as two.
6. The result is checked through the same views. If any view still shows a held value, or the input passes `json.Valid` but failed step 4 (duplicate names, invalid UTF-8, too deep), the whole input is replaced by the marker, or by the marker as a JSON string when the input was JSON, with `Whole` true.

`Contains` reports whether any view shows a held value (step 2). Nothing is returned without passing the views, so a bug in steps 4 and 5 gives a whole-value replacement, not a leak.

Work. Each view level is shorter than the one before it or ends the loop, so one check reads at most 17 levels, each with both split views, plus the number and replacement views. Step 4 checks each string token, and a string that holds JSON recurses into a shorter text, bounded by the same 16 levels. The work of a call is linear in its input with a constant set by those bounds and by the number of tiers (below); `TestRedactStepsLinear` counts it under a test build tag.

Matcher and the cost of change:

- The matcher is a set of Aho-Corasick automata, one per tier and view, written in the package on the standard library. A pass feeds each byte to every tier.
- Tiers keep registration close to linear. A change builds a new smallest tier from the forms it adds; whenever a tier holds at least half the forms of the next larger one, the two merge into one rebuilt tier. Tier sizes then at least double from one tier to the next, so there are at most 13 tiers at 4,096 values, each form is rebuilt at most once per tier it passes through, and the total build work for N values is proportional to N log N (about 4.8 times more for 4,000 values than for 1,000).
- Dropped and retired values stay in their tier, matched, until that tier is rebuilt. A tier whose dropped forms reach half of its forms is rebuilt without them at the next change, so the matcher holds at most twice the forms the caps allow. Staying matched longer can only hide more. `Stats` and the caps stop counting a value at the end of its retention.
- The smallest tier is built lazily, at the first redaction after a change, and the matcher is published atomically. A `Redact` that starts after an `Add` returned uses a matcher that holds that value: a value registered before it can appear in output is hidden in that output.
- If a matcher cannot be built (an internal size check fails), redaction fails closed: `Redact` returns no text and `ErrUnavailable`, `Contains` returns `true` and the error, and a `Stream` writes nothing more and returns the error. The product withholds the text.

### 6. Streams

A `Stream` is for live output that cannot wait for the end of the text, such as a session's terminal output. It cannot replace a whole value, so it matches the forms of point 4 in the two split views and does not decode: a held value written with an escape not in the form list passes a `Stream`. A product sends structured data (JSON answers, tool inputs, stored records) through `Redact`, not through a `Stream`.

- A `Stream` writes to the `io.Writer` it wraps. It runs the latest matcher on each `Write` and holds back the bytes that may still become a match: the current partial match in either view, with the skipped bytes inside it.
- It also keeps, in memory only, the last fed bytes up to the longest form's length minus one, and the automaton state of each view, across `Flush` and across the hold limit. When the registry's matcher changed since the last call, it replays those fed bytes through the new matcher before reading the new ones, so a partial match survives an `Add` or a merge of tiers.
- When the held bytes would pass `MaxHold`, they are written as one marker, and the stream keeps its automaton state and fed bytes: if the value goes on in a later `Write`, the rest of it is held again and replaced by the marker. A run of line breaks after the start of a value therefore cannot make the stream hold without bound, and cannot make it forget the value either. `MaxHold` is at least the longest form, so a form with no skipped bytes always fits.
- `Flush` writes the held bytes, so the product can bound the lag in time (for example on an idle timer). A held partial match of `MinLength` fed bytes or more is written as the marker. A shorter one is written as it is. In both cases the stream keeps its state, so if the value goes on in the next `Write`, the rest of it is replaced by the marker. Flushing in the middle of a value shows at most its first `MinLength - 1` bytes and never the rest.
- `Close` is `Flush` followed by refusing further writes with `ErrClosed`. It does not close the wrapped writer.
- `Write` returns `len(p)` when every byte of `p` was written or is held. When the wrapped writer fails, `Write` returns 0 and that error, the held bytes are dropped and never written, and every later call returns the same error. `NewStream(nil)` gives a stream whose every call returns `ErrInvalidOptions`.

### 7. Behaviour guarantees

| Property | Statement | Test |
| --- | --- | --- |
| Every form hidden | For every live or retained value and every form of point 4, the form at any position in random text, contiguous or split by CR, LF, CRLF, `\n` or `\r\n` between any bytes, leaves no byte of the form in the output, apart from skipped bytes at its edges and the escape cases named in point 4. | `TestEveryFormHidden` (generated: each form, each split position, surrounding bytes from a set that includes `\`, `n`, quotes and invalid bytes), `FuzzRedact` |
| Every escape hidden | A held value inside a JSON string, with any of its bytes written as any valid JSON escape (`\uXXXX` in either case, surrogate pairs, the short escapes), up to 16 levels of nesting, is not shown by any view of the output. | `TestEveryEscapeHidden` (generated from Sablecourt's variants), `FuzzRedact` |
| JSON stays JSON | When the input is valid JSON, the output is valid JSON. With no hit it is byte for byte the input; with hits in strictly valid JSON only the changed tokens differ, member order and whitespace are kept, and no key is lost. | `TestJSONKeepsItsBytes`, `FuzzRedact` |
| Nothing else changes | Text in which no view shows a held value comes back equal. | `FuzzRedact` |
| Output checked | No output of `Redact` shows a held value in any view; when the check fails, the output is the marker alone and `Whole` is true. | `FuzzRedact`, `TestWholeOnResidue` |
| Idempotence | `Redact(Redact(s).Text).Text == Redact(s).Text`. | `FuzzRedact`, `TestMarkerOverlapRefused` |
| Linear time | The work of `Redact` is at most a constant times `len(s)`, the constant set by the 16 levels and the tier count, counted under a test build tag; time on 8 MiB stays within 10 times the time on 1 MiB for near-miss texts. | `TestRedactStepsLinear`, `TestRedactGrowth` |
| Near-linear registration | Registering N values one at a time with a redaction after each does build work proportional to N log N: at most 6 times more for 4,000 values than for 1,000 (the first version took 16 times), counted under a test build tag and checked in time as well. | `TestRegisterGrowth` |
| Bounded memory | Matcher heap per byte of forms stays under a bound measured in the code pull request, at every cap. The matcher holds at most twice the forms the caps allow. | `TestMatcherHeapPerByte` |
| Bounded input | An input over `MaxInput` gives `ErrTooLarge` and no text. | `TestMaxInput` |
| Output size | `len(out) <= len(s) + (len(marker)+4) * ceil(len(s)/3) + len(marker) + 2`: a plain-text span covers at least `MinLength` bytes, a number shows a value in at least 3 bytes (`1e7`), and a JSON marker adds its quotes. | `FuzzRedact` |
| Invalid UTF-8 | Any byte string gives no panic, and invalid bytes outside spans are copied. When the input and every registered form are valid UTF-8, the output is valid UTF-8. | `FuzzRedact`, `TestInvalidUTF8` |
| Long lines | There is no line logic. A text of `MaxInput` bytes with no line break is handled like any other, and a `Stream` holds at most `MaxHold` bytes. | `TestOneLongLine`, `TestStreamHoldBound` |
| Streams | When no `Flush` happens and the held bytes never pass `MaxHold`, a `Stream`'s output for any split into writes equals the plain-text replacement of point 5, step 5, on the whole input. After `Flush`, at most `MinLength - 1` leading bytes of any form are shown. After the hold limit fires or the matcher changes, no byte of a form that goes on is shown. | `FuzzStream`, `TestFlushShowsOnlyAShortPrefix`, `TestHoldLimitKeepsState` (`prefix`, `MaxHold+1` line breaks, the rest of the value), `TestFlushAcrossAdd` (`prefix`, `Flush`, `Add` of another value, the rest) |
| No secret in output | `Registry` and `Stream` print and format as a fixed placeholder with every `fmt` verb except `%T` (the type name) and `%p` (an address), as values, pointers and unexported fields of a caller's struct, and marshal as the placeholder through `json`, `encoding.TextMarshaler` and `slog`; `gob` refuses them. Error text is fixed and holds no value or owner. `Stats` holds counts. | `TestFormattingHidesValues` (including an outer struct with each type as an unexported field, printed with `%s`, `%q`, `%v`, `%+v`, `%#v`, `%d`, `%x`), `TestErrorsQuoteNothing` (canaries in values and owners, the whole error chain walked) |
| Expiry and retirement | With an injected clock, a value is matched at least until `expires + Retention`, and after `Retire` at least until `now + Retention`; `Stats` and the caps stop counting it then, and it is no longer matched once its tier is rebuilt. Renewal only extends; one owner's retirement leaves other owners' copies. | `TestExpiry`, `TestRenew`, `TestRetireKeepsOtherOwners`, `TestReplaceRotates` |
| All or nothing | A refused `Add` or `Replace`, for any error, changes nothing, and a refused `Replace` leaves the old values live. | `TestCaps`, `TestReplaceAtomic` |
| Fails closed | A matcher that cannot be built gives no text. | `TestUnavailableFailsClosed` |
| No global state | The package has no package-level variable besides its errors. | `TestNoPackageState` (reads the package's syntax tree) |
| Concurrency | `Add`, `Retire` and `Redact` run together under `-race`, and a value added before a redaction starts is hidden by it. | `TestConcurrentAddRedact` |

Fuzz targets: `FuzzRedact` (a value, a text, a form or escape choice and split positions), `FuzzStream` (a text and write boundaries), `FuzzAdd` (arbitrary value bytes and owner: no panic, either an error or a registered value that `Redact` then hides). Each has a seed corpus, generated at run time from split strings so no file holds a credential-shaped literal.

### 8. Dependencies

Standard library only: `bytes`, `encoding/base64`, `encoding/hex`, `encoding/json` (to write forms and for `json.Valid`), `encoding/json/jsontext` (to tokenize documents), `errors`, `fmt` (for `fmt.Formatter`), `io`, `log/slog` (for `LogValue`), `net/url`, `slices`, `strconv`, `strings`, `sync`, `sync/atomic`, `time`, `unicode`, `unicode/utf16`, `unicode/utf8`. A test fails on any other import.

`encoding/json/jsontext` builds without an experiment flag on Go 1.27.4, Armory's `go` line. This ties the package to that line, which ADR-0002 open question 1 also depends on.

## Reading of invariant 4

Invariant 4 asks for limits the caller sets. Every bound of this package is an option with a default (`MaxInput`, `MaxHold`, the caps) except three fixed ones: the 16 levels of unescaping, the nesting limit of 512 and the number exponent limit of 1,024. Each fixed bound fails closed (text past it counts as showing a value, or is replaced whole), so a caller cannot get less hiding from it, only a whole-value replacement for input no product produces.

## How each product adopts it

### Sablecourt, after 0.1.0

Sablecourt replaces `known.go` and its in-progress `exactjson.go` with `exact`. What changes:

- The daemon builds one `Registry` in `main` and passes it to every holder and redacting boundary. The package-level functions and `DefaultRegistry` go away, and every call site takes the registry as an argument.
- Owners: the operator token, each provider, each task, each upstream MCP server and the webhook secret get an owner. A task's token is added with the lease expiry, renewed with `Renew` when the lease is renewed and retired when the task ends. Provider replacement and logout key rotation use `Replace` and `ReplaceBytes` (the logout signing key is 32 random bytes, registered with `RegisterBytes` today), so they stop adding permanent entries.
- `Retention: time.Hour` is passed explicitly.
- Provider connection refuses a key under 8 bytes. Today it accepts any non-empty key, which the registry then ignores.
- `RedactExact` and `RedactExactJSON` become `Redact`, with the rule Sablecourt is building now. A boundary that writes the result as JSON checks `Whole` and handles `ErrTooLarge`.
- The marker changes from `[REDACTED:known_secret]` to `[REDACTED:known-secret]`, which fits `redact.MarkerShapes`. `redact`'s `Clean` does not take it as a marker, because `Clean` accepts only markers of its own rules' kinds, `[REDACTED]` and `Withheld`; text with `token=[REDACTED:known-secret]` is not clean to it. A product that runs `Clean` after `exact` decides how to treat it (open question 4).
- `Stream.Write(raw, eof)` becomes an `io.Writer` with `Flush` and `Close`. Session output flushes on an idle timer the daemon chooses. Escapes not in the form list pass a stream; Sablecourt records that limit under its ADR-0015.
- What stays in Sablecourt from issue 189: which values it registers (Kubernetes token files and exec plugins, the client key file, the proxy probe token, the password inside a database DSN, pairing codes), text stored before a value was registered, the reachability tests and tests that fail when a production wire is removed.

### Shadewright, in 0.8.0

Shadewright adopts `exact` for the values its processes load: model provider API keys, the GitHub App private key and its installation tokens. What changes:

- Each binary that loads one of them builds its own `Registry` and runs it before the pattern redactor at the same entry points. Installation tokens are added with their expiry and rotated with `Replace` on refresh.
- The marker can be longer than the text it replaces, unlike every marker of `redact`, and a whole-value replacement makes any value the marker. Running `exact` first can make an entry longer before `redact` applies `MaxEntry` (4 MiB) and its heap weight, so an entry close to the limit today can be withheld after the change. `PiecesWithin` and `PiecesFitting` cannot shrink `exact`'s marker. Callers that size output (those two and the read tools' answer limits) check the size after both passes.
- It is a new redaction guarantee, so Shadewright records it as an amendment of its ADR-0015 before the change, as its ADR-0048 point 4 requires.

## Alternatives considered

- Keep `known.go` in Sablecourt and fix it there. Shadewright would then write its own, and the two would drift as the pattern redactors did. The fixes are most of the package anyway.
- Exact values as extra `Pattern` options of `redact` (`regexp.QuoteMeta` of each form). Every change recompiles every expression; there is no owner, expiry or retirement; and a match would share the pattern redactor's withheld state and length rule, so the guarantee would sit inside the best-effort layer. Rejected.
- A third-party Aho-Corasick module (for example `cloudflare/ahocorasick`, BSD-3-Clause, or `petar-dambovaliev/aho-corasick`, MIT). It saves about 200 lines, but neither has the skip views, the tiers or the stream state carried across `Flush`, so the package would wrap and partly duplicate it, and invariant 2 would charge a dependency for that.
- Match the bytes as given, with derived forms only, for `Redact` too (the first draft). It keeps every byte of a document and needs no tokenizer, but it misses escapes outside the form list and can break JSON, with no check to say so (point 5). Rejected for `Redact`; kept for `Stream`, where nothing better is possible without buffering.
- Decode JSON and re-encode the document, as `RedactExactJSON` does. It finds every escape a decoder accepts, but it reorders keys, drops whitespace and collapses keys. Rejected; the token splice of point 5 keeps them.
- Re-encode the token stream, as Sablecourt's revision does. Key order and duplicates are kept, but whitespace and the spelling of every string change once a document has a hit. The splice costs more code and keeps the bytes; the output check covers a splice bug either way.
- Line-buffered streams that run `Redact` on each line. They would see every escape, at the cost of lag until a line ends and of whole-line replacement in live output. Not chosen now (open question 5).
- One base tier and one delta tier rebuilt alone (the first draft). With a redaction after every change the delta is rebuilt from scratch each time, and the total build work is still quadratic: about 16 times more for 4,000 values than for 1,000. Rejected for the tiers of point 5.
- Refuse short values, register them with a known false-positive cost, or let the caller set any minimum. Registering silently below a floor is how the first version failed. A caller minimum with a floor of 8 keeps the refusal loud and lets a product that holds only long values tighten it.
- Match by hash, so the registry holds no plaintext: a rolling hash for each distinct value length. The product holds the value in memory anyway, the cost grows with the number of distinct lengths, and derived forms and views would multiply that. Rejected.
- A process-wide default registry, as the first version has. Convenient, but tests share it, nothing shows which code registers what, and a library cannot know the process's secrets. Rejected.

## Consequences

- A product must register a value before it can appear in output, and must handle `Add` errors at the point where it obtains a credential. That is the cost of a guarantee that does not depend on the shape of the secret.
- The registry keeps several derived copies of each value in memory, bounded by `MaxFormBytes`, and the matcher holds at most twice that.
- A redaction reads several views of up to 13 tiers. Output can be longer than input (point 7), and a value whose check fails is replaced whole, so a product loses a whole log line or answer when a held value shows in a spelling the replacement cannot reach.
- Streams hide fewer spellings than `Redact`. Structured data must go through `Redact`.
- Two packages to chain in the right order in every product: `exact` first, then `redact`. A product that chains them the other way still hides the values, but the pattern redactor may have changed a value's bytes first, so the exact guarantee then holds only for values no pattern rule touches.
- The caps, `MaxInput`, the `Flush` rule and the whole-value rule become public behaviour.

## Open questions

1. The default caps (`MaxValues` 4,096, `MaxValueBytes` 8 KiB, `MaxTotalBytes` 256 KiB, `MaxFormBytes` 4 MiB, `MaxInput` 16 MiB) are proposals. The code pull request measures matcher heap, build time and redaction time at the caps and may change them before the record is accepted.
2. `Flush` shows up to `MinLength - 1` leading bytes of a value cut by the flush. The other choice is to write any held partial match as the marker, which shows nothing of a value and replaces innocent bytes in live output whenever a flush lands on the first byte of some form.
3. Base64 at offsets 1 and 2 (form 7) triples the base64 forms. Whether that is worth the memory for both products is checked with the caps measurement.
4. `redact`'s `Clean` and the `exact` marker: leave it to each product, or let `Clean` accept `[REDACTED:known-secret]` (an ADR-0002 change after the move).
5. Streams: forms only (decided here), or line-buffered `Redact` for outputs where lag until a line break is acceptable.

## Sources

Read on 2026-11-27.

- Sablecourt: pull request 190, branch `180-exact-secrets` at `8cd2d6b` (`internal/redact/known.go`, `internal/redact/known_test.go`, `internal/controlplane/reconciler_regression_test.go`, `internal/attach/`, `internal/gate/model/gate.go`, `internal/operatortoken/token.go`, `internal/approval/device.go`, `internal/providers/providers.go`, `cmd/sablecourtd/operator_auth.go`), and the revision in progress (`internal/redact/exactjson.go`, `exactjson_test.go`); issues [sablecourt#180](https://github.com/shadewright-labs/sablecourt/issues/180) and [sablecourt#189](https://github.com/shadewright-labs/sablecourt/issues/189); ADR-0015 on `main` at `ab08a63`.
- Shadewright: `main` at `80fb7abc` (`internal/redact`, `internal/providers`, `internal/modules/secretsstore`), ADR-0048.
- Armory: `AGENTS.md`, ADR-0001 and `githost/token.go` on its branch, ADR-0002, issues 44 and 45.
