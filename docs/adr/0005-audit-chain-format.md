# ADR-0005: Package `audit`, chain format, versions and tail anchor

- Status: Proposed
- Date: 2026-11-27

This record decides the format of the planned `audit` package (epic 54, issue 55): how an entry is encoded and hashed, how the chain starts, how a chain says whether it is keyed, how a verifier detects deletion of the newest entries, how a row says which rule it was written with, the limits of one verification, and the interfaces a product implements so its storage stays outside Armory. No code exists yet.

## Context

Sablecourt keeps a tamper-evident audit log: each `audit_log` row stores `prev_hash` and `row_hash`, and `sablectl audit verify` asks the daemon to walk the chain (Sablecourt ADR-0009). The code is `ComputeAuditRowHash` in `internal/domain/types.go` and `appendAuditTx` and `VerifyAuditChain`, duplicated in `internal/store/sqlite/sqlite.go` and `internal/store/postgres/postgres.go`. Shadewright has no audit chain; its analysis records (Shadewright ADR-0015) are not chained.

Sablecourt's chain is the only starting point, and its fields are what this package must carry. Its design has these faults, each confirmed by reading the code on `main` and listed in Sablecourt issues 157 and 104:

1. The payload joins fields with `|` and no escaping, so action `x|y` with outcome `z` hashes the same as action `x` with outcome `y|z` (issue 157; `TestComputeAuditRowHashSeparatesFields` is skipped).
2. There is no tail anchor: deleting the newest rows leaves a valid prefix, and verification passes (issue 104).
3. Verification skips rows whose `row_hash` is empty, so blanking a row's hash removes it from the check (issue 104).
4. The row `id` is not in the hash, so an edit of ids that keeps the order passes.
5. An empty previous hash is replaced by the string `genesis`, so "" and `genesis` are the same start.
6. The timestamp is hashed as formatted text, and the parse error on read is ignored.
7. Verification reads every row in one query with no bound.

Two more points from the same code matter for the interface below. The SQLite append reads the newest hash and inserts in a deferred transaction, and most callers drop the append error (`_ = store.AppendAudit(...)`; only the three calls in `internal/integrations/shadewright.go` check it), so a failed append on the other paths is silent. And the row id is an `AUTOINCREMENT` or serial column, which in PostgreSQL skips values when a transaction rolls back.

The package belongs in Armory because the format is a security rule that must not be designed twice, and its test vectors let any product, or a script outside Go, check a chain.

Invariants touched: 1 (Armory knows no product's fields or tables; storage is an interface the product implements), 2 (standard library only: `crypto/sha256`, `crypto/hmac`, `crypto/subtle`, `encoding/binary`), 3 (Armory reads no key, and no key reaches output; error text never quotes a field name, a field value, a hash, a key or a chain ID), 4 (stored rows, head text and hash text are input from outside: every length is bounded, and the head and hash parsers and the verifier are fuzzed) and 5 (the format is a promise beyond the API: rows written today must verify under every later version of the package).

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
| sequence | 8 | uint64, at least 1 |
| time | 8 | int64, Unix nanoseconds, UTC |
| previous hash | 32 | hash of entry `Seq-1`, or `Genesis` for `Seq` 1 |
| field count | 2 | uint16, 0 to `MaxFields` |
| each field: name length | 1 | 1 to 64 |
| each field: name | m | |
| each field: value length | 4 | uint32, 0 to `MaxValueBytes` |
| each field: value | v | |

Fields are encoded in strictly increasing byte order of their names. `Encode` sorts a copy and never changes the caller's slice, so one entry has exactly one encoding. Two entries are the same entry when they have the same sequence, the same instant and the same set of fields: the order of `Fields`, the `time.Location` and a nil against an empty `Value` do not change the encoding. A duplicate name, a name outside the character set, a value over the limit or an encoding over `MaxEntryBytes` is refused. `Encode` and `Hash` also refuse `Seq` 0, `Seq` 1 with a previous hash other than `Genesis`, and `Seq` above 1 with `Genesis` as the previous hash. The hash is `SHA-256(encoding)`, or `HMAC-SHA-256(key, encoding)` for a keyed chain.

The time must lie from `1677-09-21T00:12:43.145224192Z` to `2262-04-11T23:47:16.854775807Z` (the int64 nanosecond range), checked before `UnixNano` is called, because `UnixNano` is undefined outside it. Go's zero `time.Time` (year 1) is outside that range and is refused; a Unix time of 0 (1970) is inside it and is accepted.

What this fixes, by fault number: the layout cannot be shifted between fields because every length is fixed or prefixed (1); the sequence, which is the row id, is inside the hashed bytes (4); the time is a binary integer, not text, and a time outside the range is refused at encode time (6); the chain ID binds an entry to one chain, so an entry copied into another chain of the same product does not verify there.

The time is recorded, not checked: a verifier does not require times to increase, because a wall clock can step back. A product that wants that check makes it on the entries it reads.

### Start of the chain

`Genesis` is the all-zero `Hash` (`Hash{}`; `Hash.IsZero` tests for it). It is not an exported variable, so no code in the process can change it. Entry 1 must have `Genesis` as its previous hash, and no entry after it may (5). There is no text form of an empty previous hash.

### Keyed and unkeyed chains

A chain is either SHA-256 or HMAC-SHA-256, and the choice is explicit: `Config.Algorithm` has no usable zero value, so a `Config` that does not name one is refused with `ErrInvalidConfig`. An HMAC chain needs a `Key`; an unset `Key` with `HMACSHA256` is refused, and so is a `Key` with `SHA256`. A secret store that hands back an empty key therefore stops `New`; it never yields an unkeyed chain.

The key is an opaque `Key`, built the way ADR-0001 builds a `Token`:

- `NewKey` copies the bytes it is given and refuses fewer than 32 or more than 1024. The caller may clear its slice afterwards.
- No exported function or method returns the bytes. A `Key` formats, prints and marshals as a placeholder with every `fmt` verb, through `json`, `xml`, `gob` and `slog`, as a value, pointer, field, slice or map element.
- Armory never reads keys from a file, the environment or a store.

The algorithm is part of every hash (the algorithm byte), of every `Head` and of its text form. `Verify` refuses a `start` or an anchor whose algorithm differs from the chain's, with `ErrInvalidHead`. A product that stores its anchor outside the table therefore records whether the chain is keyed, and a verifier built as SHA-256 by mistake cannot pass a keyed chain: the anchor refuses it before any row is read, and the rows would not hash anyway.

Hash comparisons in `Verify` (row hashes, previous hashes, the start row and the anchor) use `crypto/subtle.ConstantTimeCompare`.

A key change starts a new chain. The first entry of the new chain carries a field `prev.head` whose value is the text form of the old chain's final head; the old chain is frozen, and its key is kept for as long as its rows must be verified. Armory reserves that field name for this use and does not follow it: the product verifies the old chain to that head with the old key. Rotation inside one chain is not defined.

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
	Seq       uint64 // 0 for an empty chain
	Hash      Hash   // Genesis for an empty chain, never Genesis otherwise
}

// MarshalText encodes "armory-audit-head v1 <alg> <chain> <seq> <64 hex>", where <alg> is sha256 or
// hmac-sha256: at most 183 bytes. It refuses an invalid chain ID or algorithm, Seq 0 with a Hash other
// than Genesis, and Seq above 0 with Genesis.
func (h Head) MarshalText() ([]byte, error)

// UnmarshalText parses exactly that form: at most 183 bytes, single spaces, no surrounding space or
// newline, a base-10 Seq with no sign and no leading zero (except "0"), lower-case hex, and the same
// rules as MarshalText.
func (h *Head) UnmarshalText(b []byte) error
```

The longest head is 21 bytes of prefix, 12 for `hmac-sha256` and its space, 65 for a 64-byte chain ID and its space, 21 for a 20-digit `Seq` and its space, and 64 of hex: 183.

The writer returns the new head after every append. The caller stores it outside the table that holds the chain: in a file, a log line, a signed record or another system. Verification takes that anchor and fails when the chain ends before `anchor.Seq` or when the entry at `anchor.Seq` does not hash to `anchor.Hash` (2). Entries after the anchor are verified as a continuation and do not fail the check, because they were appended after the anchor was stored.

What the caller must store and how often:

- The head after each committed append, or after each batch of appends, written where a party that can rewrite the table cannot also rewrite it. A file on the same disk protects against a database edit, not against someone with the account's file access.
- The anchor only moves forward. The anchor write happens after the commit and outside the database lock, and it replaces the stored anchor only when the new `Seq` is higher (a compare-and-swap, a file written under its own lock, or an append-only log whose highest entry wins). Two appenders that commit `Seq` 10 and 11 and write their anchors in the opposite order leave anchor 11.
- If the anchor write fails after the commit, the entry stays, and the product reports the failure through its own logs. The next successful anchor write covers it; until then that entry is inside the window below.
- Entries appended after the newest stored anchor can be deleted without detection. The interval between anchor writes is that window, and each product states its own interval in its docs.
- Keeping older anchors as well lets a later check show when entries changed.

The anchor does not help when the table and the anchor are rolled back together to an earlier consistent state: a disk or VM snapshot restore, a backup restore, or an attacker who can write both. Only an anchor held outside that rollback domain (another host, write-once storage, a copy the operator keeps and passes to the verifier) detects it.

### Versions

Each stored row carries its format version in a column of its own (`Stored.Version`). The verifier knows a fixed set of versions; this record defines only version 1. Rules:

- A row whose version the verifier does not know fails with `ErrUnknownVersion`. No row is skipped for any reason, empty hash included (3).
- Versions along a chain never decrease: once a chain writes version 2 at some `Seq`, every later row is version 2 or higher. A row with a lower version than the row before fails with `ErrVersionOrder`. When `Verify` starts from a checkpoint it reads the row at the checkpoint and takes its version as the floor.
- A later format version is a new ADR. The verifier keeps checking every older version it ever defined, so a chain written today verifies under every later release.

Armory does not read rows written by Sablecourt's current code. Those rows have their own payload and their own verifier, which stay in Sablecourt (see Adoption).

### Public API

```go
package audit

type Hash [32]byte

func (h Hash) IsZero() bool             // reports whether h is Genesis
func (h Hash) String() string           // 64 lower-case hex characters
func ParseHash(s string) (Hash, error)  // exactly 64 lower-case hex characters

// Key is an HMAC key. It never prints or marshals its bytes. The zero Key is unset.
type Key struct{ /* unexported */ }

// NewKey copies b. It refuses fewer than 32 or more than 1024 bytes.
func NewKey(b []byte) (Key, error)

type Limits struct {
	MaxFields     int    // default 32, at most 255
	MaxValueBytes int    // default 64 KiB, at most 16 MiB
	MaxEntryBytes int    // default 1 MiB, at most 16 MiB
	PageSize      int    // entries asked of a Reader per call; default 256, at most 10000
	MaxEntries    uint64 // entries checked in one Verify call; default 1000000, at most 100000000
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

// Next builds the row that follows head: Seq head.Seq+1, Prev head.Hash, its hash, version 1. It
// refuses a head of another chain or algorithm, a head that breaks the Genesis rule (ErrInvalidHead),
// and head.Seq equal to the largest uint64 (ErrLimit). It does not change fields.
func (c *Chain) Next(head Head, t time.Time, fields []Field) (Stored, Head, error)

// Appender is one write transaction of the product's store.
type Appender interface {
	// Head returns the newest entry's Seq and Hash inside the transaction, with the table locked
	// against other appenders until the transaction ends. An empty chain returns Seq 0 and Genesis.
	// Append fills in the chain ID and algorithm and checks the Genesis rule.
	Head(ctx context.Context) (Head, error)
	// Insert stores s. It must fail if a row with s.Seq exists.
	Insert(ctx context.Context, s Stored) error
}

// Append reads the head, builds the next row and inserts it through a. The caller commits. An error
// from a is returned unchanged.
func (c *Chain) Append(ctx context.Context, a Appender, t time.Time, fields []Field) (Head, error)

// Reader reads stored rows for verification.
type Reader interface {
	// Read returns up to max rows with Seq >= from in increasing Seq order. Fewer than max rows
	// means the end of the chain. It must refuse a row larger than the limits before building it.
	Read(ctx context.Context, from uint64, max int) ([]Stored, error)
}

// Verify checks the chain from a trusted start to the anchor and beyond, and returns the head of the
// newest verified entry. start is c.Start() or an earlier result of Verify; anchor is the head the
// caller stored outside the table, with anchor.Seq >= start.Seq. When MaxEntries rows are checked it
// returns the head h reached so far and an error matching ErrIncomplete; the caller continues with
// Verify(ctx, r, h, anchor) when anchor.Seq > h.Seq, and with Verify(ctx, r, h, h) otherwise.
func (c *Chain) Verify(ctx context.Context, r Reader, start, anchor Head) (Head, error)

var (
	ErrInvalidEntry   = errors.New("audit: invalid entry")
	ErrInvalidConfig  = errors.New("audit: invalid config")
	ErrInvalidHead    = errors.New("audit: invalid head")
	ErrNoAnchor       = errors.New("audit: chain has entries but the anchor is empty")
	ErrTampered       = errors.New("audit: entry hash does not match")
	ErrBrokenLink     = errors.New("audit: previous hash does not match")
	ErrGap            = errors.New("audit: sequence gap or disorder")
	ErrTruncated      = errors.New("audit: chain ends before the anchor")
	ErrAnchorMismatch = errors.New("audit: entry at the anchor does not match it")
	ErrUnknownVersion = errors.New("audit: unknown format version")
	ErrVersionOrder   = errors.New("audit: format version decreased")
	ErrLimit          = errors.New("audit: limit exceeded")
	ErrIncomplete     = errors.New("audit: verification stopped at the entry limit")
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

`Verify` is streaming and bounded (7). Before it reads a row it checks both heads: `start` and `anchor` must have the chain's ID and algorithm and follow the Genesis rule (`Seq` 0 with `Genesis`, any other `Seq` without it), and `anchor.Seq` must not be lower than `start.Seq`; otherwise it returns `ErrInvalidHead`. When `start.Seq == anchor.Seq` their hashes must be equal, or it returns `ErrAnchorMismatch`.

When `start.Seq` is above 0, `Verify` reads the row at `start.Seq` first, outside the `MaxEntries` count. That row must exist (else `ErrTruncated`), have that `Seq`, a known version and a recomputed hash equal to `start.Hash` (else `ErrAnchorMismatch`). Its hash and version become the previous hash and the version floor for the rows after it. A checkpoint is as strong as the place it is kept, like the anchor.

Then it asks the `Reader` for pages from `start.Seq+1`, each of `min(PageSize, rows left before MaxEntries)` rows, keeps only the previous row's hash, sequence and version between pages, and checks `ctx` between pages. For each row it checks, in order: the page holds at most the rows asked for (`ErrLimit`); the version is known (`ErrUnknownVersion`) and not lower than the row before (`ErrVersionOrder`); `Seq` is exactly one more than the previous `Seq` (`ErrGap`); `Prev` equals the previous hash, `Genesis` for `Seq` 1 (`ErrBrokenLink`); every field and the whole encoding are within the limits (`ErrLimit`); the recomputed hash equals `Hash` (`ErrTampered`). At the anchor's `Seq` it also checks that `Hash` equals `anchor.Hash` (`ErrAnchorMismatch`).

The end of the check:

- The rows end before `anchor.Seq`: `ErrTruncated`.
- `anchor.Seq` is 0 and the chain has at least one row: `ErrNoAnchor`. An empty anchor is accepted only for an empty chain, so a product that has written entries but never stored an anchor gets a failure instead of a check that cannot see truncation. A product that wants to look at such a chain anyway passes its newest row's head as the anchor, knowing what that proves.
- `MaxEntries` rows were checked and the last page was full: the head reached and `ErrIncomplete`. The next call may find no more rows and succeed.
- Otherwise: the head of the last row and nil.

`VerifyError.Seq` is the `Seq` the check expected for `ErrGap` and `ErrTruncated` (one more than the last good row), the offending row's `Seq` for every per-row error, and `start.Seq` or `anchor.Seq` for an error about that head. `ErrIncomplete` and `ErrNoAnchor` are wrapped the same way. `start.Seq` equal to the largest uint64 reads no rows after it.

Errors from the `Reader` are returned unchanged, and `context.Canceled` and `context.DeadlineExceeded` keep their identity; neither is wrapped as a tamper error. Armory's own error text never contains a field name, a field value, a hash, a key, a chain ID or text it parsed.

A product's `Reader` that returns rows out of order, repeats a row, or returns more than it was asked for gets `ErrGap` or `ErrLimit`, not a wrong verdict.

### Storage stays outside

Armory has no SQL, no driver and no schema. The product implements `Appender` and `Reader` on its own store. What the product must provide, stated in the package doc and checked by an exported conformance test helper (`audittest.RunStore` in `audit/audittest`, a separate package so the main package imports no `testing`):

- One appender at a time per chain: `Head` locks (SQLite `BEGIN IMMEDIATE`, PostgreSQL `LOCK TABLE ... IN SHARE ROW EXCLUSIVE MODE` or `SELECT ... FOR UPDATE` on a head row). The lock lasts until the commit, so the product calls `Append` as the last write before it commits.
- A unique constraint on the sequence (per chain), so two appenders that slip past the lock cannot both write the same `Seq`. A chain that forks fails `Insert` instead.
- The sequence column is written from `Stored.Seq`, never from a database serial, because a PostgreSQL sequence skips values on rollback and the verifier treats a gap as tampering.
- Time stored as an integer of Unix nanoseconds (or a type that keeps nanoseconds exactly), hashes as 32 bytes or as their 64-character hex form, and field values as the exact bytes hashed.
- A `Reader` that bounds each column when it scans (for example a length check in the query) and refuses an oversized row before building it, since `Verify` sees a row only after `Read` has allocated it.
- The append and the product's own change commit in one transaction when the entry records that change, and an append error reaches the caller.

The conformance helper takes a factory:

```go
package audittest

// Store opens one connection to the product's store under test.
type Store interface {
	Begin(ctx context.Context) (Tx, error)
	Reader() audit.Reader
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

Sablecourt moves to a new table, for example `audit_chain (chain, seq, version, time_ns, prev_hash, row_hash, category, task_id, workload_name, action, outcome, detail_json)` with a primary key on `(chain, seq)`, in both SQLite and PostgreSQL, and implements `Appender` and `Reader` for each store. Its six text columns become fields (`action`, `category`, `detail`, `outcome`, `task_id`, `workload_name`). What changes:

- `ComputeAuditRowHash`, `appendAuditTx` and both copies of `VerifyAuditChain` stop writing; new entries go through `Chain.Append`.
- The old `audit_log` table is frozen read-only. Sablecourt keeps its own legacy check for it, since that check is the only thing standing behind old rows. Migration `0005_audit_chain.sql` added the hash columns with an empty default, so rows written before it have no hash; the legacy check accepts empty hashes only in the leading run of rows before the first hashed row and refuses an empty hash after it. That rule is Sablecourt's and is recorded on its issue.
- Entry 1 of the new chain is a bridge entry with action `legacy.bridge` and a `detail` value that holds the last legacy `row_hash` and the legacy row count (an empty hash and 0 when `audit_log` is empty), so it fits the six columns. `sablectl audit verify` checks the legacy table with its own rule, compares its end with the bridge entry, then verifies the new chain with Armory.
- `ListAudit` reads the new table and then the frozen one, so the lookups in `internal/integrations/shadewright.go` that decide whether an incident was already escalated or a verification message already recorded still find rows from before the migration.
- Sablecourt stores the head after each append outside the database, with the forward-only rule above: in sanctum mode a file in its state directory plus the daemon log line, in citadel mode a place decided in Sablecourt's own issue. The sanctum file sits in the same directory and account as the database, so it detects an edit of the database alone, not a disk rollback or a compromised account. `sablectl audit verify` asks the daemon today; it gains a way to verify against an anchor the operator supplies, decided on Sablecourt's issue.
- The daemon's verify endpoint (`GET /api/v1/audit/verify`, used by `sablectl doctor` and the web page) continues on `ErrIncomplete` or verifies from a stored checkpoint, so a chain longer than `MaxEntries` does not show as broken.
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
- A Merkle tree or a transparency log. Gives inclusion and consistency proofs and fits many writers or external auditors. The products have one writer per chain and need detection, not proofs to third parties; a tree can come later as a new ADR, and the chain's heads can be logged into one.
- Sign every entry. Needs a signing key in the writer and gives nothing a keyed hash and an external anchor do not, against an attacker who controls the writer. Signing the anchor is the product's choice.
- Use the database's serial id as the sequence. Fewer columns, but PostgreSQL leaves gaps on rollback, and a verifier that accepts gaps cannot detect a deleted row.
- No anchor, and rely on database permissions. That is the current state, and issue 104 shows the newest rows can be deleted unnoticed.

## Consequences

- The version 1 layout is permanent once one product writes it: every later release of the package must verify it. The test vectors are the contract, and a change to them fails CI.
- Armory owns no storage, so each product writes and tests its own `Appender` and `Reader`; the conformance helper keeps those honest.
- Truncation after the newest stored anchor is not detected. That window is the product's anchor interval, and the anchor is only as safe as where it is kept.
- A rollback of the table and the anchor together is not detected unless the verifier is given an anchor kept outside what was rolled back.
- A keyed chain cannot be verified without the key, and `Verify` offers no keyless mode. A product that loses the key can check the links (`Prev` against the previous `Hash`) and the sequence with its own code over the stored rows, which shows order and deletions but not edits.
- The anchor records whether a chain is keyed, so a product cannot switch a chain between keyed and unkeyed without starting a new chain.
- Sablecourt carries two verifiers for its own lifetime: its legacy check for frozen rows and Armory's for new ones.
- The first code pull request is security-relevant and gets the cross-family security review before it opens.

## Tests

Test files are under `audit/`.

- `TestVectors`: `testdata/vectors.json` holds entries with their expected encoding (hex) and hash, keyed and unkeyed, including zero fields, an empty value, a value of `MaxValueBytes` and non-ASCII bytes. A change to any vector fails.
- `TestEncodingInjective` and `FuzzEncode`: two entries that differ as entries (a field, the sequence, the instant, the chain ID, the previous hash or the algorithm) never share an encoding, and entries that differ only in field order, `time.Location` or nil against empty values share one. A decoder in the test file (not exported) reads an encoding back, and encoding the result again gives the same bytes. `Encode` leaves the caller's `Fields` unchanged.
- One failing-first test per fault in the Context: `TestFieldBoundaryShift` (157: `x|y`+`z` against `x`+`y|z`, and the same shift with any byte); `TestTailDeletion` (104: with the anchor at the newest entry, deleting the last 1, 2 and all rows fails with `ErrTruncated`; rows appended after an older anchor and then deleted are not detected, which the test records as the stated window); `TestEmptyHashRow` (104: a row with a zero or empty hash fails, never skipped); `TestSeqEdit` (an id edit that keeps the order fails); `TestGenesisOnlyFirst` (`Genesis` as `Prev` after entry 1 fails, nothing else is accepted for entry 1, and `Encode` refuses both cases); `TestTimeBinary` (a change of one nanosecond changes the hash; the zero time and times one nanosecond outside the range are refused, the bounds themselves and Unix time 0 are accepted); `TestVerifyBounded` (a `Reader` that records its calls shows each asks for at most `PageSize` rows and never past `MaxEntries`; `Verify` stops at `MaxEntries` with `ErrIncomplete`, and continuing as its doc says finishes the check, including the case of exactly `MaxEntries` rows; heap use measured over chains of 10,000 and 100,000 rows does not grow with the length).
- `TestHeads`: `start` or anchor of another chain or algorithm, `Seq` 0 with a non-zero hash, a non-zero `Seq` with `Genesis`, and `anchor.Seq < start.Seq` give `ErrInvalidHead`; `start.Seq == anchor.Seq` with equal hashes and no rows after succeeds, with different hashes gives `ErrAnchorMismatch`; an empty anchor over a non-empty chain gives `ErrNoAnchor`; a `start` whose stored row is missing or differs is refused; a resume from a checkpoint at a version 2 row refuses a version 1 row after it; `Next` refuses `head.Seq` at the largest uint64.
- `TestKeyed`: `Config` with no algorithm, `HMACSHA256` with an unset `Key`, and `SHA256` with a `Key` give `ErrInvalidConfig`; `NewKey` refuses 31 and 1025 bytes and keeps its own copy when the caller clears its slice; a keyed chain verified by a `SHA256` chain fails at the anchor with `ErrInvalidHead`, and fails at row 1 with `ErrTampered` when given an unkeyed start and anchor.
- Tamper suite over an in-memory store: edit of any field value, field name, time, version or hash; reorder of two rows; insert of a row; deletion of a middle row; a row copied from another chain; a fork with two rows at one `Seq`; an anchor from another chain; an anchor whose hash differs at its `Seq`. Each fails with the error named above, matched with `errors.Is` and `errors.As`, and the `Seq` the rules above give.
- `TestReaderMisbehaves`: out-of-order, repeated and oversized pages from the `Reader` give `ErrGap` or `ErrLimit`; a `Reader` error and a cancelled context come back with their identity.
- `TestErrorText`: no error, through its whole `Unwrap` chain and with `%v`, `%+v` and `%#v`, contains a field name, a field value, a hash, the key or the chain ID, including the errors of `ParseHash`, `Head.UnmarshalText` and `NewKey`.
- `TestKeyNeverPrints`: `Key`, `Config` and `Chain` print and marshal no key bytes with any `fmt` verb or through `json`, `xml`, `gob` and `slog`, as a value, pointer, field, slice or map element.
- `FuzzVerify`: random stored rows through a fake `Reader` never panic. The oracle rebuilds the chain from `start` with `Next` over the same times and fields: `Verify` succeeds only when every row equals the rebuilt one and the anchor matches, and the empty chain with `Start()` as start and anchor succeeds.
- `FuzzHeadText` and `FuzzParseHash`: strict parsing, round trip, no panic; `FuzzHeadText` seeds include a 183-byte head.
- `audittest.RunStore`: the conformance suite products run against their SQLite and PostgreSQL implementations, over two connections from `open` (unique `Seq`, `Head` serialised across connections, exact bytes and time round trip, a forked insert refused, an oversized row refused by the `Reader`).

## Open questions

1. Where Sablecourt stores the anchor in citadel mode (a Kubernetes object, an external log, a record signed with a device key). That is Sablecourt's decision; this record requires only that it is outside the table and moves only forward.
2. Should `Verify` accept a list of older anchors and report the first that no longer matches? It would show when a change happened, at the cost of a wider API.
3. Is a chain ID needed per tenant or per category, or is one chain per product enough? The format allows either; the products decide.
4. Key rotation inside one chain (a field that announces the next key) is left out. A new chain whose first entry carries `prev.head` covers it today.

## Sources

Read on 2026-11-27.

- Sablecourt, on `main`: `internal/domain/types.go` (`AuditRecord`, `ComputeAuditRowHash`), `internal/store/sqlite/sqlite.go` and `internal/store/postgres/postgres.go` (`appendAuditTx`, `VerifyAuditChain`, `ListAudit`), `internal/store/sqlite/migrations/0001_init.sql` and `0005_audit_chain.sql`, `internal/integrations/shadewright.go`, `cmd/sablectl/main.go` and `dx.go`, `docs/adr/0009-sanctum-citadel-audit-chain-and-crypto-approvals.md`, issues 157 and 104.
- Armory ADR-0001 (the `Token` rules this record follows for `Key`).
- RFC 2104 (HMAC), FIPS 180-4 (SHA-256).
