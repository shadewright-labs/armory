# ADR-0003: Package `redact/exact`, exact redaction of held secrets

- Status: Proposed
- Date: 2026-11-27

This record sets the rules and the contract of `github.com/shadewright-labs/armory/redact/exact` before any code exists (epic 44, issue 45): what a product registers, how long a value stays registered and who renews or retires it, the shortest value accepted, which written forms of a value are matched, and how the package fails. It is a different package from `redact` (ADR-0002): this one matches values the product holds and promises to hide them, the pattern redactor is best effort. Armory does not chain the two; the product does.

## Context

### What each product has

Sablecourt, pull request 190 (issue 180, open), branch `180-exact-secrets` at `8cd2d6b`: `internal/redact/known.go`, 806 lines, with 444 lines of tests in the package and reachability tests in four other packages. It keeps a `Registry` of values, derives quoted, JSON-escaped and line-stripped forms, builds an Aho-Corasick automaton over them and replaces each match with `[REDACTED:known_secret]`. Nineteen production files call it in 39 places, all through the package-level `DefaultRegistry` and its wrapper functions. Sablecourt ADR-0015 makes exact match of daemon-held secrets the guarantee at its redacting boundaries and the pattern rules defence in depth.

Shadewright has nothing like it. Its `internal/modules/secretsstore` stores secrets and matches nothing. Its processes hold model provider API keys and GitHub App credentials, and its only protection in output is the pattern redactor.

### What the first version got wrong

Sablecourt issue 189 lists the gaps. The ones that belong to the package, measured on 2026-11-27 by running `known.go` from the branch head outside Sablecourt:

- Registration cost grows with the square of the registry. Every registration rebuilds the whole automaton. Registering 250, 1,000 and 4,000 task-token-shaped values one at a time took 20 ms, 353 ms and 5.6 s: 79 µs, 353 µs and 1.4 ms per value.
- Nothing is retired. Deleting a value does not remove it; only an expired value with an expiry is dropped, one hour later, and only when another value is registered. Rotation of a provider key or a logout key adds a permanent entry each time.
- Short values are dropped in silence. `Register` ignores a value under 8 trimmed bytes and returns nothing, so a 7-byte key is registered as nothing and the caller does not know (`Len` stayed 0 in the run).
- The scanner skips CR, LF and the two-byte escapes `\r` and `\n` in the text. A value that ends in a backslash, followed in the text by the letter `n`, loses its last byte to the skip and is not found. The same value followed by a space is found.
- `RedactExactJSON` decodes a document, redacts its strings and encodes it again. Keys come back sorted and whitespace is lost: `{"z": 1, "a": "<value>",   "m": [1, 2]}` became `{"a":"[REDACTED:known_secret]","m":[1,2],"z":1}`. Two object keys that are both registered values collapse into one key, and one of the two entries is lost. The decode itself has no size bound.
- State sits in the struct. `String`, `GoString` and `MarshalJSON` are on the pointer receiver, so a dereferenced `Registry` printed with `%+v` shows the map, and the map keys are the values.
- One process-wide `DefaultRegistry`, which every test in the process shares.
- `Stream.Write(raw, eof)` holds back a possible partial match with no way to flush on idle, so live output lags by up to the longest value.

Matching time is linear and is not a defect: 1 MiB of near-misses took 3 ms and 8 MiB took 26 ms.

### Invariants touched

1. The package knows no product's secrets or owners; both are arguments.
2. Standard library only.
3. The registry holds secrets by design, so formatting, errors and stats must never show one. It reads nothing the caller did not pass.
4. It reads untrusted text, so its work is bounded and it is fuzzed.
5. A new public API.

## Decision

### 1. Boundary

- `redact/exact` replaces values the caller registered, in the forms listed in point 4. It does not look for credential shapes, does not parse configuration, and does not decide which values a product holds. Pulling the password out of a database connection string, or reading a token file, stays in the product.
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
	DefaultMaxHold       = 64 << 10
)

type Options struct {
	MinLength     int              // 0: MinLengthFloor; below the floor is ErrInvalidOptions
	MaxValues     int              // values live or retained; 0: DefaultMaxValues
	MaxValueBytes int              // one value; 0: DefaultMaxValueBytes
	MaxTotalBytes int              // all values live or retained; 0: DefaultMaxTotalBytes
	Retention     time.Duration    // matched for this long after expiry or retirement; 0: not at all
	Marker        string           // "": DefaultMarker; must match \[REDACTED:[a-z][a-z0-9-]{0,31}\]
	MaxHold       int              // bytes a Stream may hold back; 0: DefaultMaxHold
	Now           func() time.Time // nil: time.Now
}

func New(o Options) (*Registry, error)

type Registry struct{ s *state } // all state behind one pointer

func (r *Registry) Add(owner string, expires time.Time, values ...string) error
func (r *Registry) AddBytes(owner string, expires time.Time, values ...[]byte) error
func (r *Registry) Replace(owner string, expires time.Time, values ...string) error
func (r *Registry) Renew(owner string, expires time.Time) error
func (r *Registry) Retire(owner string) int
func (r *Registry) Stats() Stats // counts only: live, retained, owners, value bytes, form bytes

func (r *Registry) Redact(s string) (string, error)
func (r *Registry) RedactBytes(b []byte) ([]byte, error)
func (r *Registry) Contains(s string) (bool, error)
func (r *Registry) NewStream(w io.Writer) *Stream

type Stream struct{ /* unexported */ }
func (s *Stream) Write(p []byte) (int, error)
func (s *Stream) Flush() error
func (s *Stream) Close() error

var (
	ErrInvalidOptions, ErrInvalidOwner, ErrInvalidExpiry, ErrUnknownOwner error
	ErrTooShort, ErrTooLong, ErrOverlapsMarker, ErrFull                   error
	ErrUnavailable, ErrClosed                                            error
)
```

A `Registry` is safe for concurrent use. `Redact` runs without a lock on an immutable matcher.

### 3. Owners, expiry and retirement

- Every value has an owner: a string of 1 to 128 printable ASCII bytes that the product chooses, such as `task:42` or `provider:anthropic`. An empty or longer owner is `ErrInvalidOwner`. The owner is never printed, logged or put in an error.
- A value has an expiry, or none when `expires` is zero. An expiry not after `Now()` is `ErrInvalidExpiry`.
- `Add` is all or nothing. When one value of the call is refused, none is registered, and the error names the value's position in the call and nothing else.
- `Renew(owner, expires)` moves the expiry of the owner's live values that have one to `expires` when that is later. It never shortens one and leaves values with no expiry alone. An owner with no live value is `ErrUnknownOwner`. Renewing does not bring back an expired or retired value; the product adds it again.
- `Retire(owner)` ends the owner's live values now and returns how many it ended. A retired value is still matched for `Retention`, so text produced just before the rotation is still hidden, and is then dropped. Retiring an unknown owner returns 0.
- `Replace(owner, expires, values...)` retires the owner's values and adds the new ones in one step. It is the call for rotation.
- The same value may be registered by several owners. It is matched while any of them holds it live or retained, and one owner's `Retire` does not drop it for the others.
- An expired value is matched for `Retention` after its expiry, then dropped. Sablecourt passes one hour, as it does today. `Retention` is the product's choice because only the product knows how long old output can still be on its way.
- Values live or retained count against `MaxValues` and `MaxTotalBytes`. An `Add` or `Replace` that would pass either is `ErrFull` and registers nothing. The product decides what that means; for a credential it is about to hand out, the safe answer is to refuse to hand it out.

### 4. Values and forms

**Minimum length.** A value shorter than `MinLength` bytes is refused with `ErrTooShort`, never dropped in silence. The caller may raise the minimum and may not set it below 8 bytes. Length is counted on the bytes as given, with no trimming. The reasons for 8:

- Under 8 bytes, exact match starts to hit ordinary text: short words, numbers, ports, `admin`, `true`. A false positive replaces text a reader needs, and the rate depends on the value more than on its length, so the floor is a policy bound and not a measured threshold.
- Every value the two products hold today is longer: task tokens are 24 bytes or more, the operator token 67, provider keys 40 or more.
- A secret under 8 bytes is weak whatever redaction does. Refusing it tells the product to refuse such a credential where it is configured.
- With a 23-byte marker, the floor bounds how much output can grow (point 6).

**Marker overlap.** A value is refused with `ErrOverlapsMarker` when the marker contains it, when it begins with a proper suffix of the marker, or when it ends with a proper prefix of the marker. Without this rule a second pass could find a value across a marker, and idempotence would not hold.

**Forms.** Matching works on the bytes as given: the text is never decoded, unescaped, folded or normalised. Instead each value is written out in the forms it is likely to appear in, and each form is matched exactly. For a value `v`:

1. `v` itself, and `v` with leading and trailing white space removed when that differs and is still at least `MinLength` bytes.
2. The content of a JSON string as `encoding/json` writes it without HTML escaping, the same with HTML escaping (`json.Marshal`'s default, `<`, `>`, `&`), with every non-ASCII character as `\uXXXX` (what Python's `json.dumps` writes by default), and with `/` written as `\/`.
3. The JSON string content of form 2's quoted string, for JSON carried inside a JSON string. Deeper nesting is out of scope.
4. The content of `strconv.Quote(v)`, and `v` with `'` written as `\'`.
5. `url.QueryEscape(v)` and `url.PathEscape(v)`.
6. Hexadecimal in lower and upper case.
7. Base64 in the standard and URL alphabets, padded and unpadded, and the stable middle of the encoding at offsets 0, 1 and 2, so a value inside a larger base64 text (a `Basic` header, a base64 config file) is found.

Forms are deduplicated, and a form shorter than `MinLength` is not added. `AddBytes` derives the same forms from bytes; form 1 of a value that is not valid UTF-8 stays as its raw bytes.

**Split forms.** Each form is also found when line breaks were put between any of its bytes, any number of them. The matcher reads the text in two views, each of which skips one class of bytes and rewrites nothing:

- view A skips the bytes CR and LF;
- view B skips the escapes `\r` and `\n`, reading `\\` as two fed bytes so an escaped backslash is never taken for the start of an escape.

A form is matched in a view against that form with the same class removed. A match covers the text from its first fed byte to its last, skipped bytes included. The union of the two views' matches is replaced. Every contiguous occurrence of a form is found by view A whatever surrounds it, so the backslash case above cannot recur: view B may skip a byte of the value, view A does not.

Out of scope: forms not listed, a form split by anything other than line breaks, a value cut by a line continuation (`\` and a line break), case changes other than hexadecimal, and text written before the value was registered.

### 5. Matching and the cost of change

- Overlapping matches merge into one span; touching matches stay two. Each span is replaced by the marker. Bytes outside the spans are copied unchanged.
- The matcher is an Aho-Corasick automaton per view, written in the package on the standard library.
- A change (add, replace, retire, drop) does not rebuild everything. The matcher has a base and a delta. New forms go into the delta, which is rebuilt alone; the delta is merged into the base when it holds more than a quarter of the base's bytes. Dropped and retired values leave the base at the next merge, or when they hold more than a quarter of its bytes. Until then they are still matched, which can only hide more. So the amortised cost of a change is proportional to the bytes it adds or drops, not to the size of the registry. A redaction reads at most two views of two automata: four linear passes.
- The matcher is built lazily, at the first redaction after a change, and is published atomically. A `Redact` that starts after an `Add` returned uses a matcher that holds that value: a value registered before it can appear in output is hidden in that output.
- If a matcher cannot be built (an internal size check fails), redaction fails closed: `Redact` and `RedactBytes` return no text and `ErrUnavailable`, `Contains` returns `true` and the error, and a `Stream` writes nothing more and returns the error. The product withholds the text.

### 6. Streams

- A `Stream` writes to the `io.Writer` it wraps. It runs the latest matcher on each `Write` and holds back the bytes that may still become a match: the current partial match in either view, with the skipped bytes inside it.
- Without `Flush`, what a `Stream` writes, for any split of the input into `Write` calls, is the same as `Redact` of the whole input.
- When the held bytes would pass `MaxHold`, they are replaced by one marker and written. This fails closed: a run of line breaks after the start of a value cannot make the stream hold without bound.
- `Flush` writes the held bytes, so the product can bound the lag in time (for example on an idle timer). A held partial match of `MinLength` fed bytes or more is written as the marker. A shorter one is written as it is, and the stream keeps its matcher state, so if the value goes on in the next `Write`, the rest of it is replaced by the marker. Flushing in the middle of a value shows at most its first `MinLength - 1` bytes and never the rest.
- `Close` is `Flush` followed by refusing further writes with `ErrClosed`. It does not close the wrapped writer.

### 7. Behaviour guarantees

| Property | Statement | Test |
| --- | --- | --- |
| Every form hidden | For every live or retained value and every form of point 4, the form at any position in random text, contiguous or split by CR, LF, CRLF, `\n` or `\r\n` between any bytes, leaves no byte of the form in the output. | `TestEveryFormHidden` (generated: each form, each split position, surrounding bytes from a set that includes `\`, `n`, quotes and invalid bytes), `FuzzRedact` |
| Nothing else changes | Bytes outside the spans are copied unchanged; text with no match comes back equal. | `FuzzRedact` |
| Idempotence | `Redact(Redact(s)) == Redact(s)`. | `FuzzRedact`, `TestMarkerOverlapRefused` |
| Linear time | Each pass makes at most `2*len(s)` automaton transitions (goto and failure), counted under a test build tag; time on 8 MiB stays within 10 times the time on 1 MiB for near-miss texts. | `TestRedactStepsLinear`, `TestRedactGrowth` |
| Linear registration | Registering N values one at a time with a redaction after each takes total time linear in N: at most 6 times longer for 4,000 values than for 1,000 (the first version took 16 times). | `TestRegisterGrowthLinear` |
| Bounded memory | Matcher heap per byte of forms stays under a bound measured in the code pull request, at every cap. | `TestMatcherHeapPerByte` |
| Output size | `len(out) <= max(len(s), len(s)*len(marker)/MinLength)`, since every span covers at least `MinLength` bytes. | `FuzzRedact` |
| Invalid UTF-8 | Any byte string gives no panic and invalid bytes outside spans are copied. When the input and every registered form are valid UTF-8, the output is valid UTF-8. | `FuzzRedact`, `TestInvalidUTF8` |
| Long lines | There is no line logic. A 16 MiB text with no line break is handled like any other, and a `Stream` holds at most `MaxHold` bytes. | `TestOneLongLine`, `TestStreamHoldBound` |
| Streams | Output for any split into writes equals `Redact` of the whole; after `Flush`, at most `MinLength - 1` leading bytes of any form are shown. | `FuzzStream`, `TestFlushShowsOnlyAShortPrefix` |
| No secret in output | `Registry` and `Stream` print, format and marshal as a fixed placeholder with every `fmt` verb, as values and pointers, and through `json`, `xml`, `gob` and `slog`; a dereferenced `Registry` shows only a pointer. Error text is fixed and holds no value or owner. `Stats` holds counts. | `TestFormattingHidesValues`, `TestErrorsQuoteNothing` (canaries in values and owners, the whole error chain walked) |
| Expiry and retirement | With an injected clock, a value is matched until `expires + Retention` and after `Retire` until `now + Retention`; `Stats` stops counting it then. Renewal only extends; one owner's retirement leaves other owners' copies. | `TestExpiry`, `TestRenew`, `TestRetireKeepsOtherOwners`, `TestReplaceRotates` |
| Caps | An `Add` past a cap is `ErrFull` and registers nothing. | `TestCaps` |
| Fails closed | A matcher that cannot be built gives no text. | `TestUnavailableFailsClosed` |
| No global state | The package has no package-level variable besides its errors. | `TestNoPackageState` (reads the package's syntax tree) |
| Concurrency | `Add`, `Retire` and `Redact` run together under `-race`, and a value added before a redaction starts is hidden by it. | `TestConcurrentAddRedact` |

Fuzz targets: `FuzzRedact` (a value, a text, a form index and split positions), `FuzzStream` (a text and write boundaries), `FuzzAdd` (arbitrary value bytes and owner: no panic, either an error or a registered value that `Redact` then hides). Each has a seed corpus, generated at run time from split strings so no file holds a credential-shaped literal.

### 8. Dependencies

Standard library only: `bytes`, `encoding/base64`, `encoding/hex`, `encoding/json` (to write forms, never to read text), `errors`, `io`, `log/slog` (for `LogValue`), `net/url`, `slices`, `strconv`, `strings`, `sync`, `sync/atomic`, `time`, `unicode`, `unicode/utf8`. A test fails on any other import.

## How each product adopts it

### Sablecourt, after 0.1.0

Sablecourt replaces `known.go` with `exact`. What changes:

- The daemon builds one `Registry` in `main` and passes it to every holder and redacting boundary. The package-level functions and `DefaultRegistry` go away, and the 39 call sites take the registry as an argument.
- Owners: the operator token, each provider, each task, each upstream MCP server and the webhook secret get an owner. A task's token is added with the lease expiry, renewed with `Renew` when the lease is renewed and retired when the task ends. Provider replacement and logout key rotation use `Replace`, so they stop adding permanent entries.
- `Retention: time.Hour` is passed explicitly.
- Provider connection refuses a key under 8 bytes. Today it accepts any non-empty key, which the registry then ignores.
- `RedactExactJSON` goes; JSON-escaped forms are matched in place, and documents keep their bytes.
- The marker changes from `[REDACTED:known_secret]` to `[REDACTED:known-secret]`, which fits the marker shape of `redact`, so the pattern redactor's `Clean` reads it as a marker.
- `Stream.Write(raw, eof)` becomes an `io.Writer` with `Flush` and `Close`. Session output flushes on an idle timer the daemon chooses.
- What stays in Sablecourt from issue 189: which values it registers (Kubernetes token files and exec plugins, the client key file, the proxy probe token, the password inside a database DSN, pairing codes), text stored before a value was registered, the reachability tests and tests that fail when a production wire is removed.

### Shadewright, in 0.8.0

Shadewright adopts `exact` for the values its processes load: model provider API keys, the GitHub App private key and its installation tokens. What changes:

- Each binary that loads one of them builds its own `Registry` and runs it before the pattern redactor at the same entry points. Installation tokens are added with their expiry and rotated with `Replace` on refresh.
- The marker can be longer than the value it replaces, unlike every marker of `redact`. Callers that size output (`PiecesWithin`, `PiecesFitting`, the read tools' answer limits) apply `exact` first and check the size after both passes.
- It is a new redaction guarantee, so Shadewright records it as an amendment of its ADR-0015 before the change, as its ADR-0048 point 4 requires.

## Alternatives considered

- Keep `known.go` in Sablecourt and fix it there. Shadewright would then write its own, and the two would drift as the pattern redactors did. The fixes are most of the package anyway.
- Exact values as extra `Pattern` options of `redact` (`regexp.QuoteMeta` of each form). Every change recompiles every expression; there is no owner, expiry or retirement; and a match would share the pattern redactor's withheld state and length rule, so the guarantee would sit inside the best-effort layer. Rejected.
- A third-party Aho-Corasick module (for example `cloudflare/ahocorasick`, BSD-3-Clause, or `petar-dambovaliev/aho-corasick`, MIT). It saves about 200 lines, but neither has the skip views, the base and delta split or the stream state carried across `Flush`, so the package would wrap and partly duplicate it, and invariant 2 would charge a dependency for that.
- Normalise the text before matching, as the first version does. Simpler, and it loses bytes of literal values, as measured above.
- Decode JSON and match its strings, as `RedactExactJSON` does. It finds every escape a decoder accepts, but it reorders keys, drops whitespace, collapses keys and needs its own size bound. Derived forms keep the document's bytes.
- Refuse short values, register them with a known false-positive cost, or let the caller set any minimum. Registering silently below a floor is how the first version failed. A caller minimum with a floor of 8 keeps the refusal loud and lets a product that holds only long values tighten it.
- Match by hash, so the registry holds no plaintext: a rolling hash for each distinct value length. The product holds the value in memory anyway, the cost grows with the number of distinct lengths, and derived forms would multiply that. Rejected.
- A process-wide default registry, as the first version has. Convenient, but tests share it, nothing shows which code registers what, and a library cannot know the process's secrets. Rejected.

## Consequences

- A product must register a value before it can appear in output, and must handle `Add` errors at the point where it obtains a credential. That is the cost of a guarantee that does not depend on the shape of the secret.
- The registry keeps several derived copies of each value in memory, up to about 20 times its length for a value with many characters to escape.
- A redaction is up to four linear passes. Output can be longer than input, by at most the marker's length for every `MinLength` bytes.
- Two packages to chain in the right order in every product: `exact` first, then `redact`. A product that chains them the other way still hides the values, but the pattern redactor may have changed a value's bytes first, so the exact guarantee then holds only for values no pattern rule touches.
- The caps and the `Flush` rule become public behaviour.

## Open questions

1. The default caps (`MaxValues` 4,096, `MaxValueBytes` 8 KiB, `MaxTotalBytes` 256 KiB) are proposals. The code pull request measures matcher heap and build time at the caps and may change them before the record is accepted.
2. `Flush` shows up to `MinLength - 1` leading bytes of a value cut by the flush. The other choice is to write any held partial match as the marker, which shows nothing of a value and replaces innocent bytes in live output whenever a flush lands on the first byte of some form.
3. Base64 at every offset (form 7) triples the base64 forms. Whether that is worth the memory for both products is checked with the caps measurement.

## Sources

Read on 2026-11-27.

- Sablecourt: pull request 190, branch `180-exact-secrets` at `8cd2d6b` (`internal/redact/known.go`, `internal/redact/known_test.go`); issues [sablecourt#180](https://github.com/shadewright-labs/sablecourt/issues/180) and [sablecourt#189](https://github.com/shadewright-labs/sablecourt/issues/189); ADR-0015 on `main` at `ab08a63`.
- Shadewright: `main` at `80fb7abc` (`internal/modules/secretsstore`, `internal/redact`), ADR-0048.
- Armory: `AGENTS.md`, ADR-0002, issues 44 and 45.
