# ADR-0005: Package `audit`, chain format, versions and tail anchor

- Status: Proposed
- Date: 2026-11-27

This record decides the format of the planned `audit` package (epic 54, issue 55): how an entry is encoded and hashed, how the chain starts, how a chain says whether it is keyed, how a verifier detects deletion of the newest entries, how a new anchor proves it extends the old one, what a verification run proves and on which snapshot, how a row says which rule it was written with, and the interfaces a product implements so its storage stays outside Armory. No code exists yet.

## Context

Sablecourt keeps a tamper-evident audit log: each `audit_log` row stores `prev_hash` and `row_hash`, and `sablectl audit verify` asks the daemon to walk the chain (Sablecourt ADR-0009). The code is `ComputeAuditRowHash` in `internal/domain/types.go` and `appendAuditTx` and `VerifyAuditChain`, duplicated in `internal/store/sqlite/sqlite.go` and `internal/store/postgres/postgres.go`. Shadewright has no audit chain; its analysis records (Shadewright ADR-0015) are not chained.

Sablecourt's chain is the only starting point, and its fields are what this package must carry. Its design has these faults, each confirmed by reading the code on `main` and listed in Sablecourt issues 157 and 104:

1. The payload joins fields with `|` and no escaping, so action `x|y` with outcome `z` hashes the same as action `x` with outcome `y|z` (issue 157; `TestComputeAuditRowHashSeparatesFields` is skipped).
2. There is no tail anchor: deleting the newest rows leaves a valid prefix, and verification passes (issue 104).
3. Verification skips rows whose `row_hash` is empty, anywhere in the table, so blanking a row's hash removes it from the check (issue 104).
4. The row `id` is not in the hash, so an edit of ids that keeps the order passes.
5. An empty previous hash is replaced by the string `genesis`, so "" and `genesis` are the same start.
6. The timestamp is hashed as formatted text, and the parse error on read is ignored.
7. Verification reads every row in one query with no bound.

Two more points from the same code matter for the interface below. The SQLite append reads the newest hash and inserts in a deferred transaction, and most callers drop the append error (`_ = store.AppendAudit(...)`; only the three calls in `internal/integrations/shadewright.go` check it), so a failed append on the other paths is silent. And the row id is an `AUTOINCREMENT` or serial column, which in PostgreSQL skips values when a transaction rolls back.

The package belongs in Armory because the format is a security rule that must not be designed twice, and its test vectors let any product, or a script outside Go, check a chain.

Invariants touched: 1 (Armory knows no product's fields or tables; storage is an interface the product implements), 2 (standard library only: `crypto/sha256`, `crypto/hmac`, `crypto/subtle`, `encoding/binary`), 3 (Armory reads no key, no key reaches output, and an error from the product's storage is replaced by a fixed one; error text never quotes a field name, a field value, a hash, a key or a chain ID), 4 (stored rows, head text and hash text are input from outside: every length is bounded, and the head and hash parsers and the verifier are fuzzed) and 5 (the format is a promise beyond the API: rows written today must verify under every later version of the package).

## Decision

### Entry and encoding, format version 1

An entry is a sequence number, a time and a list of named fields:

```go
type Entry struct {
	Seq    uint64    // position in the chain, from 1, no gaps; the row id
	Time   time.Time // when the entry was written; hashed as Unix nanoseconds UTC
	Fields []Field
}

type Field struct {
	Name  string // 1 to 64 bytes of a-z, 0-9, '_', '.', '-'; unique within the entry
	Value []byte // exact bytes; Armory does not normalise JSON or text
}
```

The hashed bytes of an entry are a fixed binary layout in which every variable part has a length prefix. All integers are big-endian.

| Part | Size | Content |
| --- | --- | --- |
| magic | 12 | ASCII `ARMORY-AUDIT` (domain separation from any other SHA-256 use) |
| format version | 1 | `0x01` |
| hash algorithm | 1 | `0x01` SHA-256, `0x02` HMAC-SHA-256 |
| chain ID length | 1 | n, 1 to 64 |
| chain ID | n | the caller's chain name, same character set as field names |
| sequence | 8 | uint64, 1 to `MaxSeq` |
| time | 8 | int64, Unix nanoseconds, UTC |
| previous hash | 32 | hash of entry `Seq-1`, or `Genesis` for `Seq` 1 |
| field count | 2 | uint16, 0 to `MaxFields` |
| each field: name length | 1 | 1 to 64 |
| each field: name | m | |
| each field: value length | 4 | uint32, 0 to `MaxValueBytes` |
| each field: value | v | |

Fields are encoded in strictly increasing byte order of their names. `Encode` sorts a copy and never changes the caller's slice, so one entry has exactly one encoding. Two entries are the same entry when they have the same sequence, the same instant and the same set of fields: the order of `Fields`, the `time.Location` and a nil against an empty `Value` do not change the encoding. A duplicate name, a name outside the character set, a value over the limit or an encoding over `MaxEntryBytes` is refused. `Encode` and `Hash` also refuse `Seq` 0, `Seq` above `MaxSeq`, `Seq` 1 with a previous hash other than `Genesis`, and `Seq` above 1 with `Genesis` as the previous hash. The hash is `SHA-256(encoding)`, or `HMAC-SHA-256(key, encoding)` for a keyed chain.

`MaxSeq` is 2^63-1. The layout keeps 8 bytes, but SQLite integers and PostgreSQL `bigint` are signed, so a larger `Seq` could not be stored in either engine.

The time must lie from `1677-09-21T00:12:43.145224192Z` to `2262-04-11T23:47:16.854775807Z` (the int64 nanosecond range), checked before `UnixNano` is called, because `UnixNano` is undefined outside it. Go's zero `time.Time` (year 1) is outside that range and is refused; a Unix time of 0 (1970) is inside it and is accepted.

What this fixes, by fault number: the layout cannot be shifted between fields because every length is fixed or prefixed (1); the sequence, which is the row id, is inside the hashed bytes (4); the time is a binary integer, not text, and a time outside the range is refused at encode time (6); the chain ID binds an entry to one chain, so an entry copied into another chain of the same product does not verify there.

The time is recorded, not checked: a verifier does not require times to increase, because a wall clock can step back. A product that wants that check makes it on the entries it reads.

### Start of the chain

`Genesis` is the all-zero `Hash` (`Hash{}`; `Hash.IsZero` tests for it). It is not an exported variable, so no code in the process can change it. Entry 1 must have `Genesis` as its previous hash, and no entry after it may (5). There is no text form of an empty previous hash.

### Keyed and unkeyed chains

A chain is either SHA-256 or HMAC-SHA-256, and the choice is explicit: `Config.Algorithm` has no usable zero value, so a `Config` that does not name one is refused with `ErrInvalidConfig`. An HMAC chain needs a `Key`; an unset `Key` with `HMACSHA256` is refused, and so is a `Key` with `SHA256`. A secret store that hands back an empty key therefore stops `New`; it never yields an unkeyed chain.

The key is an opaque `Key`, built the way ADR-0001 builds a `Token`:

- `NewKey` takes exactly 32 bytes and copies them; the caller may clear its slice afterwards. The bytes must come from a cryptographic random source (`crypto/rand`, or a secret store that generated them that way). A password or any other chosen text is not a key: HMAC is only as strong as the key's unpredictability, and Armory cannot check that, so the package doc states the rule. No product needs a longer key, and one fixed length is the simpler contract.
- No exported function or method returns the bytes. The bytes sit behind a pointer to an unexported type, so `fmt` printing a struct that holds a `Key` in an unexported field (where `fmt` cannot call the `Key`'s own methods) shows an address, not the bytes. A `Key` formats, prints and marshals as a placeholder with every `fmt` verb, through `json`, `xml`, `gob` and `slog`, as a value, pointer, exported or unexported field, slice or map element. The guarantee is that no key material appears in output in any representation: raw, hex, decimal or base64.
- Armory never reads keys from a file, the environment or a store.

The algorithm is part of every hash (the algorithm byte), of every `Head` and of its text form. `Verify` refuses a `Check` whose heads have another algorithm than the chain's, with `ErrInvalidHead`. A product that stores its anchor outside the table therefore records whether the chain is keyed, and a verifier built as SHA-256 by mistake cannot pass a keyed chain: the anchor refuses it before any row is read, and the rows would not hash anyway.

Hash comparisons in `Verify` and `Append` (row hashes, previous hashes, the start row, the tip and the anchor) use `crypto/subtle.ConstantTimeCompare` on the fixed 32 bytes.

A key change starts a new chain with a new chain ID and a new, independent key. The first entry of the new chain carries a field `prev.head` whose value is the text form of the old chain's final head; the old chain is frozen, and its key is kept for as long as its rows must be verified. Armory reserves that field name for this use and does not follow it: the product verifies the old chain to that head with the old key. The product keeps a record, in the same protected place as its anchor, of which chain ID is active; the head text names the chain, so the stored anchor is that record. Rotation inside one chain is not defined.

### Head and tail anchor

```go
type Algorithm uint8

const (
	SHA256     Algorithm = 1 // the zero Algorithm is invalid
	HMACSHA256 Algorithm = 2
)

type Head struct {
	Chain     string
	Algorithm Algorithm
	Seq       uint64 // 0 for an empty chain, at most MaxSeq
	Hash      Hash   // Genesis for an empty chain, never Genesis otherwise
}

// MarshalText encodes "armory-audit-head v1 <alg> <chain> <seq> <64 hex>", where <alg> is sha256 or
// hmac-sha256: at most 182 bytes. It refuses an invalid chain ID or algorithm, Seq above MaxSeq, Seq 0
// with a Hash other than Genesis, and Seq above 0 with Genesis.
func (h Head) MarshalText() ([]byte, error)

// UnmarshalText parses exactly that form: at most 182 bytes, single spaces, no surrounding space or
// newline, a base-10 Seq with no sign and no leading zero (except "0") and at most MaxSeq, lower-case
// hex, and the same rules as MarshalText.
func (h *Head) UnmarshalText(b []byte) error
```

The longest head is 21 bytes of prefix, 12 for `hmac-sha256` and its space, 65 for a 64-byte chain ID and its space, 20 for a 19-digit `Seq` and its space, and 64 of hex: 182.

The writer returns the new head after every append. The caller stores it outside the table that holds the chain: in a file, a log line, a signed record or another system. Verification takes that anchor and fails when the chain ends before `anchor.Seq` or when the entry at `anchor.Seq` does not hash to `anchor.Hash` (2).

#### Publishing a new anchor

A stored anchor is replaced only by a head that is proven to extend it. A higher `Seq` alone is not proof: if the anchor is row 10 and the database alone is restored to row 9, two ordinary appends write a different row 10 and a row 11, and an anchor that accepted any higher `Seq` would move to 11 and hide the rollback. The publishing step, run by the product after the append commits and outside the database lock, is:

1. Read the stored anchor `A`. If none is stored, `A` is `Start()`.
2. For the candidate head `H`: if `H.Seq < A.Seq`, stop without writing (a later head was already published, and its publication proved the rows up to it). If `H.Seq == A.Seq`, the hashes must be equal; a different hash is a conflict.
3. Otherwise run `Extends(ctx, r, A, H)`. It reads the row at `A.Seq` and requires it to match `A` exactly, then checks every row from `A.Seq+1` to `H.Seq` as `Verify` does, and requires the row at `H.Seq` to hash to `H.Hash`. This covers the rows of a crash or a delayed publication in between.
4. Replace `A` with `H` by a compare-and-swap on the complete head text (`Chain`, `Algorithm`, `Seq` and `Hash`), not on `Seq`: a file rewritten under its own lock after comparing the old content, a conditional update, or an append-only log whose writer appends `H` only when its last entry is still `A`. A lost swap starts again from step 1.

A conflict or a failed `Extends` is reported and the anchor is not moved: the chain and the anchor disagree, and that is the evidence a later audit needs. Two appenders that commit `Seq` 10 and 11 and publish in the opposite order leave anchor 11: the writer of 11 proves 9 to 11, and the writer of 10 then finds a higher anchor and stops.

What the caller must store and how often:

- The head after each committed append, or after each batch of appends, written where a party that can rewrite the table cannot also rewrite it. A file on the same disk protects against a database edit, not against someone with the account's file access.
- If the publication fails after the commit, the entry stays, and the product reports the failure through its own logs. The next successful publication covers it; until then that entry is inside the window below.
- Entries appended after the newest stored anchor can be deleted without detection. The interval between publications is that window, and each product states its own interval in its docs.
- Keeping older anchors as well lets a later check show when entries changed.

### Versions

Each stored row carries its format version in a column of its own (`Stored.Version`). The verifier knows a fixed set of versions; this record defines only version 1. Rules:

- A row whose version the verifier does not know fails with `ErrUnknownVersion`. No row is skipped for any reason, empty hash included (3).
- Versions along a chain never decrease: once a chain writes version 2 at some `Seq`, every later row is version 2 or higher. A row with a lower version than the row before fails with `ErrVersionOrder`. A suffix check takes the version of the row at its start as the floor.
- `Append` refuses to write after a tip whose version it does not know (`ErrUnknownVersion`) or whose version is higher than the one it writes (`ErrVersionOrder`), so an older writer cannot lower the version of a chain a newer writer has moved on.
- A later format version is a new ADR. The verifier keeps checking every older version it ever defined, so a chain written today verifies under every later release.

Armory does not read rows written by Sablecourt's current code. Those rows have their own payload and their own verifier, which stay in Sablecourt (see Adoption).

### Public API

```go
package audit

const MaxSeq = 1<<63 - 1

type Hash [32]byte

func (h Hash) IsZero() bool             // reports whether h is Genesis
func (h Hash) String() string           // 64 lower-case hex characters
func ParseHash(s string) (Hash, error)  // exactly 64 lower-case hex characters

// Key is an HMAC key. It never prints or marshals its bytes. The zero Key is unset.
type Key struct{ /* unexported pointer */ }

// NewKey copies b, which must be exactly 32 bytes from a cryptographic random source.
func NewKey(b []byte) (Key, error)

type Limits struct {
	MaxFields     int // default 32, at most 255
	MaxValueBytes int // default 64 KiB, at most 16 MiB
	MaxEntryBytes int // default 1 MiB, at most 16 MiB
}

type Config struct {
	Chain     string    // chain ID
	Algorithm Algorithm // required: SHA256 or HMACSHA256
	Key       Key       // required with HMACSHA256, refused with SHA256
	Limits    Limits    // a zero field takes its default
}

// Chain is immutable after New and safe for concurrent use. It formats without its key.
type Chain struct{ /* unexported */ }

// New refuses a Config with an invalid chain ID, an algorithm and key that do not match, or a negative
// or above-maximum limit, with ErrInvalidConfig.
func New(c Config) (*Chain, error)

// Start is the head of an empty chain: Seq 0, Genesis, the chain's ID and algorithm.
func (c *Chain) Start() Head

// Encode returns the version 1 encoding of e after prev.
func (c *Chain) Encode(prev Hash, e Entry) ([]byte, error)

// Hash returns the hash of e after prev.
func (c *Chain) Hash(prev Hash, e Entry) (Hash, error)

// Stored is an entry as the product keeps it.
type Stored struct {
	Entry
	Version uint8
	Prev    Hash
	Hash    Hash
}

// Appender is one write transaction of the product's store. The product begins that transaction
// already holding the chain's write lock (see "Storage stays outside"); Tip does not take it.
type Appender interface {
	// Tip returns the newest stored row of the chain, or ok false for an empty chain.
	Tip(ctx context.Context) (s Stored, ok bool, err error)
	// Insert stores s. It must fail if a row with s.Seq exists.
	Insert(ctx context.Context, s Stored) error
}

// Append reads the tip, checks it, builds the next row and inserts it through a. The caller commits.
// The tip check: its version is known and not above 1, its Seq is below MaxSeq, its Prev follows the
// Genesis rule, and its stored Hash equals the hash recomputed with this Chain's ID, algorithm and key
// (else ErrTipMismatch). A writer with the wrong algorithm, key or chain ID therefore cannot extend the
// chain. An empty chain has no tip to check, so row 1 is written with whatever the Config says.
func (c *Chain) Append(ctx context.Context, a Appender, t time.Time, fields []Field) (Head, error)

// Rows is one ordered scan of stored rows.
type Rows interface {
	// Next returns the next row, or io.EOF after the last. A row over the limits is an error, never
	// skipped, even when it is the last row.
	Next(ctx context.Context) (Stored, error)
	Close() error
}

// Reader opens scans for verification.
type Reader interface {
	// Rows scans the rows with from <= Seq <= to in increasing Seq order. Every scan opened through one
	// Reader sees one snapshot of the store.
	Rows(ctx context.Context, from, to uint64) (Rows, error)
}

// Check says what one Verify call proves.
type Check struct {
	Anchor Head   // the head stored outside the table
	To     uint64 // last Seq to read; 0 means Anchor.Seq; never below Anchor.Seq
}

// Result is what a successful Verify or Extends call proved.
type Result struct {
	From     uint64 // 0: every row from Genesis was checked; otherwise the Seq of the trusted start row
	Anchored uint64 // the Seq up to which an independent anchor confirms the rows
	Head     Head   // the head of row To
}

// Verify checks the whole chain, from Genesis, up to Check.To, on the Reader's one snapshot.
func (c *Chain) Verify(ctx context.Context, r Reader, chk Check) (Result, error)

// Extends checks that next extends prev: the row at prev.Seq matches prev exactly, every row from
// prev.Seq+1 to next.Seq is valid, and the row at next.Seq hashes to next.Hash. It proves nothing
// about the rows before prev.Seq.
func (c *Chain) Extends(ctx context.Context, r Reader, prev, next Head) (Result, error)

var (
	ErrInvalidEntry   = errors.New("audit: invalid entry")
	ErrInvalidConfig  = errors.New("audit: invalid config")
	ErrInvalidHead    = errors.New("audit: invalid head")
	ErrNoAnchor       = errors.New("audit: chain has entries but the anchor is empty")
	ErrTampered       = errors.New("audit: entry hash does not match")
	ErrBrokenLink     = errors.New("audit: previous hash does not match")
	ErrGap            = errors.New("audit: sequence gap or disorder")
	ErrTruncated      = errors.New("audit: chain ends before the expected entry")
	ErrAnchorMismatch = errors.New("audit: entry does not match the trusted head")
	ErrTipMismatch    = errors.New("audit: newest entry does not hash under this chain's configuration")
	ErrUnknownVersion = errors.New("audit: unknown format version")
	ErrVersionOrder   = errors.New("audit: format version decreased")
	ErrLimit          = errors.New("audit: limit exceeded")
	ErrStorage        = errors.New("audit: storage error")
	ErrConflict       = errors.New("audit: concurrent append, retry the transaction")
)

// VerifyError carries the sequence number where verification failed and one of the errors above.
type VerifyError struct {
	Seq  uint64
	Kind error
}

func (e *VerifyError) Error() string // fixed text per Kind and the Seq; nothing from the rows
func (e *VerifyError) Unwrap() error // Kind, so errors.Is(err, ErrTampered) holds
```

### Verification

`Verify` and `Extends` are streaming and bounded (7): they hold one row at a time, and the previous row's hash, sequence and version between rows. Memory is bounded by `MaxEntryBytes` plus what the product's driver buffers, not by the chain's length. Time is bounded by the caller's context, which is checked before each row.

The target is fixed before the first row is read: `Check.To` (or `Anchor.Seq`) for `Verify`, `next.Seq` for `Extends`. Rows appended during the call are outside the snapshot and outside the range asked of the `Reader`, so a call never chases new appends. A caller that wants the tail as well reads the tip's `Seq` once, on the same snapshot as the `Reader`, and passes it as `To`. `Result.Anchored` then says how far the independent anchor reaches; the rows between `Anchored` and `To` are checked for links and hashes but are confirmed by nothing outside the table.

Before any row is read, every head is checked: it has the chain's ID and algorithm, a `Seq` at most `MaxSeq`, and follows the Genesis rule (`Seq` 0 with `Genesis`, any other `Seq` without it), else `ErrInvalidHead`. `To` below `Anchor.Seq`, or `next.Seq` below `prev.Seq`, is `ErrInvalidHead`. For `Extends`, `prev.Seq == next.Seq` with different hashes is `ErrAnchorMismatch`.

`Verify` always starts at Genesis: its first row must be `Seq` 1 with `Prev` equal to `Genesis`. There is no resume point for a whole-chain verdict, and no checkpoint read from the table can stand in for the rows before it. When `Anchor.Seq` is 0, `Verify` asks for row 1 and fails with `ErrNoAnchor` if it exists, so a product that has written entries but never stored an anchor gets a failure instead of a check that cannot see truncation. A product that wants to look at such a chain anyway passes its newest row's head as the anchor, knowing what that proves.

`Extends` with `prev.Seq` above 0 reads the row at `prev.Seq` first. That row must exist (else `ErrTruncated`), have that `Seq`, a known version, and satisfy `recomputed hash == row.Hash == prev.Hash` with the same size and Genesis checks as any other row (else `ErrAnchorMismatch`). Checking both equalities matters: a row whose stored `Hash` alone was changed still recomputes to `prev.Hash`, and a check of the recomputed hash only would pass it. Its hash and version become the previous hash and the version floor for the rows after it. `Result.From` is `prev.Seq`.

For each row `Verify` and `Extends` check, in order: the `Reader` has not returned more rows than the range holds (`ErrLimit`); the version is known (`ErrUnknownVersion`) and not lower than the row before (`ErrVersionOrder`); `Seq` is exactly one more than the previous `Seq` (`ErrGap`); `Prev` equals the previous hash, `Genesis` for `Seq` 1 (`ErrBrokenLink`); every field and the whole encoding are within the limits (`ErrLimit`); the recomputed hash equals `Hash` (`ErrTampered`). At the anchor's `Seq` (for `Extends`, at `next.Seq`) the stored `Hash` must also equal the anchor's hash (`ErrAnchorMismatch`). The rows ending before the target is `ErrTruncated`.

`VerifyError.Seq` is the `Seq` the check expected for `ErrGap` and `ErrTruncated` (one more than the last good row), the offending row's `Seq` for every per-row error, and the head's `Seq` for an error about a head.

#### What one run proves

| Call | Proves | Does not prove |
| --- | --- | --- |
| `Verify` with an independently stored anchor | Rows 1 to `Anchor.Seq`, as they are in the snapshot, are exactly the chain the anchor commits to: no insertion, deletion, reordering, edit or truncation up to the anchor. | Anything about rows after `Anchor.Seq` beyond their links and hashes. |
| `Extends(prev, next)` | The row at `prev.Seq` is the row `prev` was taken from, and rows up to `next.Seq` follow it. | Anything about rows before `prev.Seq`: deleting row 20 after a run that reached row 100 is invisible to `Extends` from 100. Only a later `Verify` from Genesis detects it. |

A whole-chain verdict comes only from one `Verify` call on one snapshot. A call that fails with a context error, or whose snapshot is lost, proves nothing, and the next attempt starts again from Genesis. A product that cannot afford a full scan in one call (a long chain behind a request deadline) runs the full scan in the background and reports its last result with its time, or reports `Extends` from its last published anchor labelled as a check of the new rows only.

### Errors from the product's storage

`Tip`, `Insert`, `Rows` and `Next` belong to the product, and their errors can carry a field value, a connection string or a wrapped secret. Armory does not return them. A callback error that matches `context.Canceled` or `context.DeadlineExceeded` with `errors.Is` is returned as that bare sentinel; one that matches `ErrConflict` (the product's mapping of a unique-key violation, a serialisation failure or a lock timeout) is returned as `ErrConflict`; every other callback error becomes `ErrStorage`. In `Verify` and `Extends` these come inside a `VerifyError` with the `Seq` being read. The product's original error is dropped, not wrapped, so no `Unwrap` chain reaches it; this is the rule ADR-0001 applies to credential and write-check errors. A product that needs the cause logs it inside its own callback before returning.

Armory's own error text never contains a field name, a field value, a hash, a key, a chain ID or text it parsed. A `Reader` that returns rows out of order, repeats a row, or returns more than the range holds gets `ErrGap` or `ErrLimit`, not a wrong verdict.

### Storage stays outside

Armory has no SQL, no driver and no schema. The product implements `Appender` and `Reader` on its own store. What the product must provide, stated in the package doc and checked by an exported conformance test helper (`audittest.RunStore` in `audit/audittest`, a separate package so the main package imports no `testing`):

- One appender at a time per chain, with the lock taken when the transaction begins, before the tip is read. SQLite cannot upgrade a deferred transaction to `BEGIN IMMEDIATE` once it has started, so the product begins every transaction that may append audit entries in immediate mode (for example a write connection opened with an immediate transaction lock). PostgreSQL takes `LOCK TABLE ... IN SHARE ROW EXCLUSIVE MODE` before reading the tip, which works on an empty chain, or keeps one permanent head row per chain, provisioned when the chain is created, and locks it with `SELECT ... FOR UPDATE` and updates it in the same transaction. A `FOR UPDATE` on the newest audit row does not work: an empty chain has no row to lock. The lock lasts until the commit, so the product calls `Append` as the last write before it commits.
- A unique constraint on the sequence (per chain), so two appenders that slip past the lock cannot both write the same `Seq`. A chain that forks fails `Insert`, and the product maps that failure to `ErrConflict` and retries its whole transaction.
- The sequence column is written from `Stored.Seq`, never from a database serial, because a PostgreSQL sequence skips values on rollback and the verifier treats a gap as tampering. The column is a signed 64-bit integer; `MaxSeq` keeps every `Seq` within it.
- Time stored as an integer of Unix nanoseconds (or a type that keeps nanoseconds exactly), hashes as 32 bytes or as their 64-character hex form, and field values as the exact bytes hashed.
- Every `Rows` scan through one `Reader` sees one snapshot: a SQLite read transaction, or a PostgreSQL `REPEATABLE READ READ ONLY` transaction, held for the whole `Verify` or `Extends` call.
- A `Rows` that selects each column's byte length with the row, checks it against the limits before it reads the column, and returns an error for an oversized row. Filtering oversized rows out in the query is wrong: at the end of the table a filtered row looks like the end of the chain.
- The append and the product's own change commit in one transaction when the entry records that change, and an append error reaches the caller.

The conformance helper takes a factory:

```go
package audittest

// Store opens one connection to the product's store under test.
type Store interface {
	// Begin starts a write transaction that already holds the chain's append lock.
	Begin(ctx context.Context) (Tx, error)
	// Reader opens a read transaction on one snapshot; close releases it.
	Reader(ctx context.Context) (r audit.Reader, close func() error, err error)
}

// Tx is one write transaction.
type Tx interface {
	audit.Appender
	Commit() error
	Rollback() error
}

// RunStore runs the conformance suite. open is called more than once against the same database, and each
// call must return a Store on its own connection, so the lock tests run across connections.
func RunStore(t *testing.T, open func(t *testing.T) Store)
```

Separate connections matter for SQLite: a store with one pooled connection (Sablecourt sets `SetMaxOpenConns(1)`) serialises goroutines in `database/sql`, and a lock test through it passes even with a deferred `BEGIN`.

## Adoption

### Sablecourt, after 0.1.0

Sablecourt moves to a new table, for example `audit_chain (chain, seq, version, time_ns, prev_hash, row_hash, category, task_id, workload_name, action, outcome, detail_json, prev_head)` with a primary key on `(chain, seq)`, in both SQLite and PostgreSQL, and implements `Appender` and `Reader` for each store. Its six text columns become fields (`action`, `category`, `detail`, `outcome`, `task_id`, `workload_name`). `prev_head` is nullable: NULL means the entry has no `prev.head` field, and a value (only in entry 1 of a chain started by a key change) is that field's exact bytes. What changes:

- `ComputeAuditRowHash`, `appendAuditTx` and both copies of `VerifyAuditChain` stop writing; new entries go through `Chain.Append`.
- The old `audit_log` table is frozen read-only. Sablecourt keeps its own legacy check for it, since that check is the only thing standing behind old rows. Migration `0005_audit_chain.sql` added the hash columns with an empty default, so rows written before it have no hash. The tightened legacy check accepts empty hashes only in the leading run of rows before the first hashed row and refuses an empty hash after it; today both store verifiers skip an empty hash anywhere. That rule is Sablecourt's and is recorded on its issue.
- The legacy check alone does not commit to the frozen rows: an unhashed leading row can be edited, the `action` and `outcome` boundary can shift in a hashed row (fault 1), and an id can change (fault 4), all without changing the row count or the last `row_hash`. So the bridge commits to the rows themselves. At cutover Sablecourt computes a legacy digest: it runs every `audit_log` row, in id order, through an in-memory Armory SHA-256 chain with chain ID `sablecourt.legacy` (nothing is stored), one entry per row with `Seq` counting from 1 and the row's own `CreatedAt` parsed time as `Time`, and with fields `id` (decimal text), `created_at` (the exact stored text), the six legacy columns, `prev_hash` and `row_hash`. The final head of that chain is the digest, and its `Seq` is the row count. A later digest format would use another chain ID. The Armory encoding gives the framing; Sablecourt writes no second encoder. A row whose stored time does not parse fails the migration (fault 6).
- Cutover is one step with no legacy writer running: the release that adds the new table removes the old write path, and the migration runs with the daemon stopped. Entry 1 of the new chain is a bridge entry with action `legacy.bridge` and a `detail` value holding the legacy digest head text. The migration is restart-safe: if entry 1 exists, it recomputes the digest and compares it instead of writing a second bridge, and it refuses when they differ. Migration is complete only after the bridge's head is published as the first anchor.
- `sablectl audit verify` runs the legacy check, recomputes the legacy digest and compares it with the bridge entry, then verifies the new chain with Armory. This proves the frozen rows are unchanged since cutover. It cannot show edits made before cutover beyond what the legacy check catches.
- `ListAudit` reads the new table and then the frozen one, so the lookups in `internal/integrations/shadewright.go` that decide whether an incident was already escalated or a verification message already recorded still find rows from before the migration.
- Sablecourt publishes the head after each append outside the database, with the publication steps above: in sanctum mode a file in its state directory plus the daemon log line, in citadel mode a place decided in Sablecourt's own issue. The sanctum file sits in the same directory and account as the database, so it detects an edit of the database alone, not a disk rollback or a compromised account. `sablectl audit verify` asks the daemon today; it gains a way to verify against an anchor the operator supplies, decided on Sablecourt's issue.
- The daemon's verify endpoint (`GET /api/v1/audit/verify`, used by `sablectl doctor` and the web page) runs `Verify` from Genesis on one read transaction, to the stored anchor and to the tip read in that transaction, and reports `Anchored` and the tip separately. If the full scan does not fit the request deadline, the endpoint returns the last completed full result with its time instead of a partial one labelled as whole.
- Callers stamp `CreatedAt` before the append (the stores fill in the current time today when it is zero, and Armory refuses a zero time) and bound `detail` below `MaxValueBytes` (the MCP gate writes a redacted error text of any length today).
- The callers that drop the append error stop doing so, and `sablectl doctor` reports a verify request that fails with a status other than 409 instead of leaving out the audit line. It already prints a broken chain (HTTP 409).
- Issue 157 and the audit points of issue 104 close when this lands, with a test per fault in Armory and a migration test in Sablecourt.

### Shadewright 0.8.0

Nothing changes. Shadewright has no chain. If it later chains its analysis records it does so through this package and its own ADR, with storage in whatever its records live in, through the same two interfaces.

## Alternatives considered

- Fix Sablecourt's code in place. One product, no shared release, but the format and its verifier would still be designed and reviewed in a product repo, and the next product would copy it with its faults or write a third version.
- Escape the separator in the text payload. Fixes fault 1 only, and every escaping rule must be reproduced exactly by any other verifier. Length prefixes need no rule.
- Canonical JSON of a fixed struct. Readable, but key order, number formats, Unicode escaping and Go's HTML escaping in `encoding/json` must all be pinned, and a verifier in another language must match each choice. A binary layout with lengths has one reading.
- Protocol Buffers with deterministic marshalling. Deterministic output is not guaranteed across library versions, and it adds a dependency every product inherits.
- A fixed schema with Sablecourt's six fields. Simpler encoding, but Armory would know a product's types (invariant 1), and every new field would be a new format version.
- An optional `Key []byte` that selects HMAC when set. One field fewer, but a nil or empty key from a broken secret store would silently give an unkeyed chain, and a byte slice in `Config` prints with `%v` and marshals with `json`.
- Separate constructors for keyed and unkeyed chains. Also explicit, but the anchor would still not say which kind of chain it belongs to; putting the algorithm in `Head` covers both.
- An anchor that moves to any higher `Seq`. Simpler to publish, but a database rolled back below the anchor and appended to again replaces the anchor with a head of a different history.
- Resumable verification from a checkpoint with a whole-chain verdict. It would let a long scan span calls, but a checkpoint only says what the rows were when it was taken: rows deleted before it afterwards are not seen. A verifier-owned cursor tied to one snapshot would close that, at the cost of an API that hands out snapshot state. Streaming with a fixed target needs neither, and memory stays bounded.
- A page-at-a-time `Reader` returning `[]Stored`. At the permitted maxima one page could hold many gigabytes before `Verify` saw it; one row at a time bounds memory by one entry.
- A Merkle tree or a transparency log. Gives inclusion and consistency proofs and fits many writers or external auditors. The products have one writer per chain and need detection, not proofs to third parties; a tree can come later as a new ADR, and the chain's heads can be logged into one.
- Sign every entry. Needs a signing key in the writer and gives nothing a keyed hash and an external anchor do not, against an attacker who controls the writer. Signing the anchor is the product's choice.
- Use the database's serial id as the sequence. Fewer columns, but PostgreSQL leaves gaps on rollback, and a verifier that accepts gaps cannot detect a deleted row.
- No anchor, and rely on database permissions. That is the current state, and issue 104 shows the newest rows can be deleted unnoticed.

## Consequences

- The version 1 layout is permanent once one product writes it: every later release of the package must verify it. The test vectors are the contract, and a change to them fails CI.
- Armory owns no storage, so each product writes and tests its own `Appender` and `Reader`; the conformance helper keeps those honest.
- Each append hashes the tip once more to check the writer's configuration; each publication reads the rows since the last anchor.
- A whole-chain check reads every row on one snapshot, so its cost grows with the chain. Long chains need a background full check; there is no cheaper whole-chain verdict.
- A keyed chain cannot be verified without the key, and `Verify` offers no keyless mode. The anchor records whether a chain is keyed, so a product cannot switch a chain between keyed and unkeyed without starting a new chain.
- Sablecourt carries two verifiers for its own lifetime: its legacy check and digest for frozen rows and Armory's for new ones.
- The first code pull request is security-relevant and gets the cross-family security review before it opens.

### Detection boundary

With a `Reader` that returns the stored rows faithfully, one snapshot, a scan from Genesis and an anchor kept where the attacker cannot write, both modes detect insertion, deletion, reordering, tail truncation up to the anchor and any edit of the anchored rows, as far as SHA-256 holds. Physical storage order does not matter. These limits remain:

| Condition | What verification can establish |
| --- | --- |
| Rows after the newest anchor | Either mode can lose them without detection. In SHA-256 mode anyone who can write the table can also write a replacement for them. In HMAC mode a fresh valid row needs the key, but an attacker can replay a valid continuation that was once written after the same head. |
| Table and anchor restored together (a disk or VM snapshot restore, a backup restore, or an attacker who can write both) | An earlier consistent history passes in either mode. Only a later head kept outside the rollback domain (another host, write-once storage, a copy the operator keeps and passes to the verifier) shows it. |
| HMAC key compromised | The holder can write valid continuations and false entries. An anchor published earlier and kept safe still commits to its prefix. Anyone who holds the key to verify also holds the power to write valid rows. |
| HMAC key lost | Sequence and link checks over the stored rows can show ordinary gaps or broken links. Someone able to renumber rows and rewrite `Prev` and `Hash` values can satisfy those checks, so they prove nothing against an attacker. |
| Empty chain written with the wrong configuration | `Append` has no tip to check, so a writer configured with the wrong algorithm or key can write row 1. The next correctly configured append refuses it with `ErrTipMismatch`, and `Verify` refuses it. |
| Legacy Sablecourt rows | The bridge digest shows changes after cutover. Changes before cutover are caught only as far as the legacy check reaches. |

## Tests

Test files are under `audit/`.

- `TestVectors`: `testdata/vectors.json` holds entries with their expected encoding (hex) and hash, keyed and unkeyed, including zero fields, an empty value, a value of `MaxValueBytes`, `Seq` equal to `MaxSeq` and non-ASCII bytes. A change to any vector fails.
- `TestEncodingInjective` and `FuzzEncode`: two entries that differ as entries (a field, the sequence, the instant, the chain ID, the previous hash or the algorithm) never share an encoding, and entries that differ only in field order, `time.Location` or nil against empty values share one. A decoder in the test file (not exported) reads an encoding back, and the oracle is `decode(encode(e)) == canonical(e)`, where `canonical` sorts the fields, converts the time to UTC nanoseconds and maps nil values to empty. `Encode` leaves the caller's `Fields` unchanged.
- One failing-first test per fault in the Context: `TestFieldBoundaryShift` (157: `x|y`+`z` against `x`+`y|z`, and the same shift with any byte); `TestTailDeletion` (104: with the anchor at the newest entry, deleting the last 1, 2 and all rows fails with `ErrTruncated`; rows appended after an older anchor and then deleted are not detected, which the test records as the stated window); `TestEmptyHashRow` (104: a row with a zero or empty hash fails, never skipped); `TestSeqEdit` (an id edit that keeps the order fails); `TestGenesisOnlyFirst` (`Genesis` as `Prev` after entry 1 fails, nothing else is accepted for entry 1, and `Encode` refuses both cases); `TestTimeBinary` (a change of one nanosecond changes the hash; the zero time and times one nanosecond outside the range are refused, the bounds themselves and Unix time 0 are accepted); `TestVerifyStreams` (a `Reader` that records its scans shows each asks only for the fixed range; rows appended after the call starts are not read; heap use measured over chains of 10,000 and 100,000 rows does not grow with the length).
- `TestHeads`: a head of another chain or algorithm, `Seq` 0 with a non-zero hash, a non-zero `Seq` with `Genesis`, `Seq` above `MaxSeq`, and `To` below `Anchor.Seq` give `ErrInvalidHead`; an empty anchor over a non-empty chain gives `ErrNoAnchor`; the empty chain with `Start()` as anchor succeeds.
- `TestExtends`: the row at `prev.Seq` missing gives `ErrTruncated`; that row with only its stored `Hash` changed, or with a field changed, gives `ErrAnchorMismatch`; `prev.Seq == next.Seq` with different hashes gives `ErrAnchorMismatch`; a version 1 row after a `prev` at a version 2 row gives `ErrVersionOrder`; `Result.From` is `prev.Seq`. A prefix row deleted after `prev` was taken is not detected by `Extends` and is detected by `Verify`, which the test records as the stated limit.
- `TestAnchorPublication` (over the in-memory store and a reference publisher in the test file): the anchor at 10, the store restored to 9, two appends writing a different 10 and an 11: publishing 11 fails and the anchor stays at the old 10. Publications of 10 and 11 in either order leave 11. A publication across a crash gap (anchor 5, head 9) reads and checks rows 6 to 9.
- `TestAppendTip`: a `SHA256` writer on an HMAC chain, an HMAC writer with another key, a writer with another chain ID, and a tip whose stored `Hash` was edited each give `ErrTipMismatch` and insert nothing; a tip of an unknown version gives `ErrUnknownVersion`; a tip at `MaxSeq` gives `ErrLimit`.
- `TestKeyed`: `Config` with no algorithm, `HMACSHA256` with an unset `Key`, and `SHA256` with a `Key` give `ErrInvalidConfig`; `NewKey` refuses 31 and 33 bytes and keeps its own copy when the caller clears its slice; a keyed chain verified by a `SHA256` chain fails at the anchor with `ErrInvalidHead`, and fails at row 1 with `ErrTampered` when given an unkeyed anchor.
- Tamper suite over an in-memory store: edit of any field value, field name, time, version or hash; reorder of two rows; insert of a row; deletion of a middle row; a row copied from another chain; a fork with two rows at one `Seq`; an anchor from another chain; an anchor whose hash differs at its `Seq`. Each fails with the error named above, matched with `errors.Is` and `errors.As`, and the `Seq` the rules above give.
- `TestReaderMisbehaves`: out-of-order, repeated and extra rows from the `Reader` give `ErrGap` or `ErrLimit`; an oversized last row reported by `Next` as an error is not taken as the end.
- `TestStorageErrors`: each callback (`Tip`, `Insert`, `Rows`, `Next`) returns an error that nests a synthetic secret several `Unwrap` levels deep, and the returned error matches `ErrStorage` and contains no secret with `%v`, `%+v`, `%#v` or along its whole `errors.Unwrap` chain; a nested `context.Canceled` or `context.DeadlineExceeded` comes back as the bare sentinel, and a nested `ErrConflict` as `ErrConflict`, with no product text.
- `TestErrorText`: no error, through its whole `Unwrap` chain and with `%v`, `%+v` and `%#v`, contains a field name, a field value, a hash, the key or the chain ID, including the errors of `ParseHash`, `Head.UnmarshalText` and `NewKey`.
- `TestKeyNeverPrints`: `Key`, `Config` and `Chain` print and marshal no key material with any `fmt` verb or through `json`, `xml`, `gob` and `slog`, as a value, pointer, exported field, unexported field of a caller's struct, slice or map element. The check searches the output for the key in raw, hex (both cases), decimal byte list and base64 form.
- `FuzzVerify`: random stored rows through a fake `Reader` never panic. The oracle rebuilds the chain from Genesis over the same times and fields: `Verify` succeeds only when every row up to the target equals the rebuilt one and the anchor matches.
- `FuzzHeadText` and `FuzzParseHash`: strict parsing, round trip, no panic; `FuzzHeadText` seeds include a 182-byte head and a `Seq` of `MaxSeq+1`.
- `audittest.RunStore`: the conformance suite products run against their SQLite and PostgreSQL implementations, over two connections from `open`: unique `Seq`; appends serialised across connections; two first writers on an empty chain where the first rolls back and the second commits row 1; exact bytes and time round trip; a forked insert mapped to `ErrConflict`; an oversized row refused by `Next`, also as the last row; a `Reader` that keeps its snapshot while another connection appends.

## Open questions

1. Where Sablecourt stores the anchor in citadel mode (a Kubernetes object, an external log, a record signed with a device key). That is Sablecourt's decision; this record requires only that it is outside the table and that publication follows the steps above.
2. Should `Verify` accept a list of older anchors and report the first that no longer matches? It would show when a change happened, at the cost of a wider API.
3. Is a chain ID needed per tenant or per category, or is one chain per product enough? The format allows either; the products decide.
4. Key rotation inside one chain (a field that announces the next key) is left out. A new chain whose first entry carries `prev.head` covers it today.

## Sources

Read on 2026-11-27.

- Sablecourt, on `main`: `internal/domain/types.go` (`AuditRecord`, `ComputeAuditRowHash`), `internal/store/sqlite/sqlite.go` and `internal/store/postgres/postgres.go` (`appendAuditTx`, `VerifyAuditChain`, `ListAudit`), `internal/store/sqlite/migrations/0001_init.sql` and `0005_audit_chain.sql`, `internal/integrations/shadewright.go`, `cmd/sablectl/main.go` and `dx.go`, `docs/adr/0009-sanctum-citadel-audit-chain-and-crypto-approvals.md`, issues 157 and 104.
- Armory ADR-0001 (the `Token` rules this record follows for `Key`, and its rule for callback errors).
- RFC 2104 (HMAC), FIPS 180-4 (SHA-256).
- SQLite, [transactions](https://www.sqlite.org/lang_transaction.html); PostgreSQL, [explicit locking](https://www.postgresql.org/docs/current/explicit-locking.html) and [transaction isolation](https://www.postgresql.org/docs/current/transaction-iso.html).
- Go, [`fmt` printing rules](https://pkg.go.dev/fmt#hdr-Printing) (methods of unexported fields are not called).
