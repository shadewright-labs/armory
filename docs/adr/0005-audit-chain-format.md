# ADR-0005: Package `audit`, chain format, versions and tail anchor

- Status: Proposed
- Date: 2026-11-27

This record decides the format of the planned `audit` package (epic 54, issue 55): how an entry is encoded and hashed, how the chain starts, how a verifier detects deletion of the newest entries, how a row says which rule it was written with, the limits of one verification, and the interfaces a product implements so its storage stays outside Armory. No code exists yet.

## Context

Sablecourt keeps a tamper-evident audit log: each `audit_log` row stores `prev_hash` and `row_hash`, and `sablectl audit verify` walks the chain (Sablecourt ADR-0009). The code is `ComputeAuditRowHash` in `internal/domain/types.go` and `appendAuditTx` and `VerifyAuditChain`, duplicated in `internal/store/sqlite/sqlite.go` and `internal/store/postgres/postgres.go`. Shadewright has no audit chain; its analysis records (Shadewright ADR-0015) are not chained.

Sablecourt's chain is the only starting point, and its fields are what this package must carry. Its design has these faults, each confirmed by reading the code on `main` and listed in Sablecourt issues 157 and 104:

1. The payload joins fields with `|` and no escaping, so action `x|y` with outcome `z` hashes the same as action `x` with outcome `y|z` (issue 157; `TestComputeAuditRowHashSeparatesFields` is skipped).
2. There is no tail anchor: deleting the newest rows leaves a valid prefix, and verification passes (issue 104).
3. Verification skips rows whose `row_hash` is empty, so blanking a row's hash removes it from the check (issue 104).
4. The row `id` is not in the hash, so an edit of ids that keeps the order passes.
5. An empty previous hash is replaced by the string `genesis`, so "" and `genesis` are the same start.
6. The timestamp is hashed as formatted text, and the parse error on read is ignored.
7. Verification reads every row in one query with no bound.

Two more points from the same code matter for the interface below. The SQLite append reads the newest hash and inserts in a deferred transaction, and every caller drops the append error (`_ = store.AppendAudit(...)`), so a failed append is silent. And the row id is an `AUTOINCREMENT` or serial column, which in PostgreSQL skips values when a transaction rolls back.

The package belongs in Armory because the format is a security rule that must not be designed twice, and its test vectors let any product, or a script outside Go, check a chain.

Invariants touched: 1 (Armory knows no product's fields or tables; storage is an interface the product implements), 2 (standard library only: `crypto/sha256`, `crypto/hmac`, `encoding/binary`), 3 (Armory reads no key; error text never quotes a field value, a hash or a key), 4 (stored rows are input from outside: every length is bounded and the decoder and verifier are fuzzed) and 5 (the format is a promise beyond the API: rows written today must verify under every later version of the package).

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

Fields are encoded in strictly increasing byte order of their names, and `Encode` sorts them, so one entry has exactly one encoding. A duplicate name, a name outside the character set, a value over the limit or an encoding over `MaxEntryBytes` is refused. The hash is `SHA-256(encoding)`, or `HMAC-SHA-256(key, encoding)` when the chain has a key.

What this fixes, by fault number: the layout cannot be shifted between fields because every length is fixed or prefixed (1); the sequence, which is the row id, is inside the hashed bytes (4); the time is a binary integer, not text, and a time outside the int64 nanosecond range (years 1678 to 2262) or the zero time is refused at encode time (6); the chain ID binds an entry to one chain, so an entry copied into another chain of the same product does not verify there.

The time is recorded, not checked: a verifier does not require times to increase, because a wall clock can step back. A product that wants that check makes it on the entries it reads.

### Start of the chain

`Genesis` is a 32-byte all-zero hash, exported as a constant. Entry 1 must have `Genesis` as its previous hash, and no entry after it may (5). There is no text form of an empty previous hash.

### Head and tail anchor

```go
type Head struct {
	Chain string
	Seq   uint64 // 0 for an empty chain
	Hash  Hash   // Genesis for an empty chain
}

func (h Head) MarshalText() ([]byte, error)    // "armory-audit-head v1 <chain> <seq> <64 hex>"
func (h *Head) UnmarshalText(b []byte) error   // strict, at most 160 bytes
```

The writer returns the new head after every append. The caller stores it outside the table that holds the chain: in a file, a log line, a signed record or another system. Verification takes that anchor and fails when the chain ends before `anchor.Seq` or when the entry at `anchor.Seq` does not hash to `anchor.Hash` (2). Entries after the anchor are verified as a continuation and do not fail the check, because they were appended after the anchor was stored.

What the caller must store and how often:

- The head after each committed append, or after each batch of appends, written where a party that can rewrite the table cannot also rewrite it. A file on the same disk protects against a database edit, not against someone with the account's file access.
- Entries appended after the newest stored anchor can be deleted without detection. The interval between anchor writes is that window, and each product states its own interval in its docs.
- An anchor for a newer `Seq` replaces an older one. Keeping older anchors as well lets a later check show when entries changed.

### Versions

Each stored row carries its format version in a column of its own (`Stored.Version`). The verifier knows a fixed set of versions; this record defines only version 1. Rules:

- A row whose version the verifier does not know fails with `ErrUnknownVersion`. No row is skipped for any reason, empty hash included (3).
- Versions along a chain never decrease: once a chain writes version 2 at some `Seq`, every later row is version 2 or higher.
- A later format version is a new ADR. The verifier keeps checking every older version it ever defined, so a chain written today verifies under every later release.

Armory does not read rows written by Sablecourt's current code. Those rows have their own payload and their own verifier, which stay in Sablecourt (see Adoption).

### Keyed hash

A chain may have a key. Armory never reads keys from a file, the environment or a store; the caller passes the bytes, at least 32 of them. The algorithm byte is inside the encoding, so a keyed and an unkeyed chain over the same entries cannot share hashes. The key is not kept in error text, `String` output or the `Head`. A key change starts a new chain (see Adoption for how one chain points at another); rotation inside one chain is not defined.

### Public API

```go
package audit

type Hash [32]byte

var Genesis Hash // all zero

func (h Hash) String() string            // 64 lower-case hex characters
func ParseHash(s string) (Hash, error)   // exactly 64 lower-case hex characters

type Limits struct {
	MaxFields     int    // default 32, at most 255
	MaxValueBytes int    // default 64 KiB, at most 16 MiB
	MaxEntryBytes int    // default 1 MiB, at most 16 MiB
	PageSize      int    // entries asked of a Reader per call; default 256, at most 10000
	MaxEntries    uint64 // entries checked in one Verify call; default 1000000
}

type Config struct {
	Chain  string // chain ID
	Key    []byte // optional; HMAC-SHA-256 when set, at least 32 bytes
	Limits Limits
}

type Chain struct{ /* unexported */ }

func New(c Config) (*Chain, error)

// Start is the head of an empty chain: Seq 0, Genesis.
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

// Next builds the row that follows head: Seq head.Seq+1, Prev head.Hash, its hash, version 1.
func (c *Chain) Next(head Head, t time.Time, fields []Field) (Stored, Head, error)

// Appender is one write transaction of the product's store.
type Appender interface {
	// Head returns the newest entry's Seq and Hash inside the transaction, with the table locked
	// against other appenders until the transaction ends. An empty chain returns Seq 0 and Genesis.
	Head(ctx context.Context) (Head, error)
	// Insert stores s. It must fail if a row with s.Seq exists.
	Insert(ctx context.Context, s Stored) error
}

// Append reads the head, builds the next row and inserts it through a. The caller commits.
func (c *Chain) Append(ctx context.Context, a Appender, t time.Time, fields []Field) (Head, error)

// Reader reads stored rows for verification.
type Reader interface {
	// Read returns up to max rows with Seq >= from in increasing Seq order. Fewer than max rows
	// means the end of the chain.
	Read(ctx context.Context, from uint64, max int) ([]Stored, error)
}

// Verify checks the chain from a trusted start to the anchor and beyond. start is c.Start() or an
// earlier result of Verify; anchor is the head the caller stored outside the table. It returns the head
// of the newest verified entry. When MaxEntries is reached first it returns the head reached so far
// and an error matching ErrIncomplete, and the caller may continue from that head.
func (c *Chain) Verify(ctx context.Context, r Reader, start, anchor Head) (Head, error)

var (
	ErrInvalidEntry   = errors.New("audit: invalid entry")
	ErrInvalidConfig  = errors.New("audit: invalid config")
	ErrTampered       = errors.New("audit: entry hash does not match")
	ErrBrokenLink     = errors.New("audit: previous hash does not match")
	ErrGap            = errors.New("audit: sequence gap or disorder")
	ErrTruncated      = errors.New("audit: chain ends before the anchor")
	ErrAnchorMismatch = errors.New("audit: entry at the anchor does not match it")
	ErrUnknownVersion = errors.New("audit: unknown format version")
	ErrLimit          = errors.New("audit: limit exceeded")
	ErrIncomplete     = errors.New("audit: verification stopped at the entry limit")
)

// VerifyError carries the sequence number where verification failed and one of the errors above.
type VerifyError struct {
	Seq  uint64
	Kind error
}
```

### Verification

`Verify` is streaming and bounded. It asks the `Reader` for pages of `PageSize` rows from `start.Seq+1`, keeps only the previous row's hash and sequence between pages, and stops after `MaxEntries` rows (7). For each row it checks, in order: the page holds at most the rows asked for; the version is known and not lower than the row before; `Seq` is exactly one more than the previous `Seq`; `Prev` equals the previous hash (`Genesis` for `Seq` 1); every field and the whole encoding are within the limits; the recomputed hash equals `Hash`. At the anchor's `Seq` it also checks `Hash` equals `anchor.Hash`. If the rows end before `anchor.Seq` it returns `ErrTruncated`. An anchor with a different chain ID, a `Seq` lower than `start.Seq`, or a `start` that is not `Start()` and does not match the stored row at its `Seq` is refused.

`start` lets a product verify only the entries since a checkpoint it trusts, so a daily check does not reread a long chain. A checkpoint is as strong as the place it is kept, like the anchor.

A product's `Reader` that returns rows out of order, repeats a row, or returns more than it was asked for gets `ErrGap` or `ErrLimit`, not a wrong verdict.

### Storage stays outside

Armory has no SQL, no driver and no schema. The product implements `Appender` and `Reader` on its own store. What the product must provide, stated in the package doc and checked by an exported conformance test helper (`audittest.RunStore` in `audit/audittest`, a separate package so the main package imports no `testing`):

- One appender at a time per chain: `Head` locks (SQLite `BEGIN IMMEDIATE`, PostgreSQL `LOCK TABLE ... IN SHARE ROW EXCLUSIVE MODE` or `SELECT ... FOR UPDATE` on a head row).
- A unique constraint on the sequence (per chain), so two appenders that slip past the lock cannot both write the same `Seq`. A chain that forks fails `Insert` instead.
- The sequence column is written from `Stored.Seq`, never from a database serial, because a PostgreSQL sequence skips values on rollback and the verifier treats a gap as tampering.
- Time stored as an integer of Unix nanoseconds (or a type that keeps nanoseconds exactly), hashes as 32 bytes or as their 64-character hex form, and field values as the exact bytes hashed.
- The append and the product's own change commit in one transaction when the entry records that change, and an append error reaches the caller.

## Adoption

### Sablecourt, after 0.1.0

Sablecourt moves to a new table, for example `audit_chain (chain, seq, version, time_ns, prev_hash, row_hash, category, task_id, workload_name, action, outcome, detail_json)` with a primary key on `(chain, seq)`, in both SQLite and PostgreSQL, and implements `Appender` and `Reader` for each store. Its six text columns become fields (`action`, `category`, `detail`, `outcome`, `task_id`, `workload_name`). What changes:

- `ComputeAuditRowHash`, `appendAuditTx` and both copies of `VerifyAuditChain` stop writing; new entries go through `Chain.Append`.
- The old `audit_log` table is frozen read-only. Sablecourt keeps its own legacy check for it and fixes the empty-hash skip there, since that check is the only thing standing behind old rows. The first entry of the new chain carries the last legacy `row_hash` and the legacy row count as fields (`legacy.head`, `legacy.count`), so `sablectl audit verify` checks the legacy table with its own rule, compares its end with those fields, then verifies the new chain with Armory.
- Sablecourt stores the head after each append outside the database: in sanctum mode a file in its state directory plus the daemon log line, in citadel mode a place decided in Sablecourt's own issue. `sablectl audit verify` reads the anchor and reports truncation.
- Callers stop dropping the append error, and `sablectl doctor` prints an audit line when the chain is broken (issue 104).
- Issue 157 and the audit points of issue 104 close when this lands, with a test per fault in Armory and a migration test in Sablecourt.

### Shadewright 0.8.0

Nothing changes. Shadewright has no chain. If it later chains its analysis records it does so through this package and its own ADR, with storage in whatever its records live in, through the same two interfaces.

## Alternatives considered

- Fix Sablecourt's code in place. One product, no shared release, but the format and its verifier would still be designed and reviewed in a product repo, and the next product would copy it with its faults or write a third version.
- Escape the separator in the text payload. Fixes fault 1 only, and every escaping rule must be reproduced exactly by any other verifier. Length prefixes need no rule.
- Canonical JSON of a fixed struct. Readable, but key order, number formats, Unicode escaping and Go's HTML escaping in `encoding/json` must all be pinned, and a verifier in another language must match each choice. A binary layout with lengths has one reading.
- Protocol Buffers with deterministic marshalling. Deterministic output is not guaranteed across library versions, and it adds a dependency every product inherits.
- A fixed schema with Sablecourt's seven fields. Simpler encoding, but Armory would know a product's types (invariant 1), and every new field would be a new format version.
- A Merkle tree or a transparency log. Gives inclusion and consistency proofs and fits many writers or external auditors. The products have one writer per chain and need detection, not proofs to third parties; a tree can come later as a new ADR, and the chain's heads can be logged into one.
- Sign every entry. Needs a signing key in the writer and gives nothing a keyed hash and an external anchor do not, against an attacker who controls the writer. Signing the anchor is the product's choice.
- Use the database's serial id as the sequence. Fewer columns, but PostgreSQL leaves gaps on rollback, and a verifier that accepts gaps cannot detect a deleted row.
- No anchor, and rely on database permissions. That is the current state, and issue 104 shows the newest rows can be deleted unnoticed.

## Consequences

- The version 1 layout is permanent once one product writes it: every later release of the package must verify it. The test vectors are the contract, and a change to them fails CI.
- Armory owns no storage, so each product writes and tests its own `Appender` and `Reader`; the conformance helper keeps those honest.
- Truncation after the newest stored anchor is not detected. That window is the product's anchor interval, and the anchor is only as safe as where it is kept.
- A keyed chain cannot be verified without the key. A product that loses the key can still check the links (`Prev` against the previous `Hash`) and the sequence, not the entries.
- Sablecourt carries two verifiers for its own lifetime: its legacy check for frozen rows and Armory's for new ones.
- The first code pull request is security-relevant and gets the cross-family security review before it opens.

## Tests

Test files are under `audit/`.

- `TestVectors`: `testdata/vectors.json` holds entries with their expected encoding (hex) and hash, keyed and unkeyed, including zero fields, an empty value, a value of `MaxValueBytes` and non-ASCII bytes. A change to any vector fails.
- `TestEncodingInjective` and `FuzzEncode`: two different entries (any field, the sequence, the time, the chain ID, the previous hash or the algorithm differing) never share an encoding; decoding an encoding and encoding it again gives the same bytes.
- One failing-first test per fault in the Context: `TestFieldBoundaryShift` (157: `x|y`+`z` against `x`+`y|z`, and the same shift with any byte); `TestTailDeletion` (104: deleting the last 1, 2 and all rows after the anchor fails with `ErrTruncated`); `TestEmptyHashRow` (104: a row with a zero or empty hash fails, never skipped); `TestSeqEdit` (an id edit that keeps the order fails); `TestGenesisOnlyFirst` (`Genesis` as `Prev` after entry 1 fails, and nothing else is accepted for entry 1); `TestTimeBinary` (a change of one nanosecond changes the hash; a zero or out-of-range time is refused); `TestVerifyBounded` (a `Reader` that counts calls shows `Verify` never holds more than `PageSize` rows and stops at `MaxEntries` with `ErrIncomplete`, and continuing from the returned head finishes the check).
- Tamper suite over an in-memory store: edit of any field value, field name, time, version or hash; reorder of two rows; insert of a row; deletion of a middle row; a row copied from another chain; a fork with two rows at one `Seq`; an anchor from another chain; an anchor whose hash differs at its `Seq`. Each fails with the error named above and the `Seq` where it happened.
- `TestReaderMisbehaves`: out-of-order, repeated and oversized pages from the `Reader` give `ErrGap` or `ErrLimit`.
- `TestErrorText`: no error contains a field value, a hash, the key or the chain ID.
- `TestKeyNotPrinted`: `Config` and `Chain` print no key bytes with any `fmt` verb.
- `FuzzVerify`: random stored rows through a fake `Reader` never panic and never verify unless they are a valid chain built with `Next`. `FuzzHeadText` and `FuzzParseHash`: strict parsing, round trip, no panic.
- `audittest.RunStore`: the conformance suite products run against their SQLite and PostgreSQL implementations (unique `Seq`, serialised `Head`, exact bytes and time round trip, a forked insert refused).

## Open questions

1. Where Sablecourt stores the anchor in citadel mode (a Kubernetes object, an external log, a record signed with a device key). That is Sablecourt's decision; this record requires only that it is outside the table.
2. Should `Verify` accept a list of older anchors and report the first that no longer matches? It would show when a change happened, at the cost of a wider API.
3. Is a chain ID needed per tenant or per category, or is one chain per product enough? The format allows either; the products decide.
4. Key rotation inside one chain (a field that announces the next key) is left out. A new chain with a pointer to the old head covers it today.

## Sources

Read on 2026-11-27.

- Sablecourt, on `main`: `internal/domain/types.go` (`AuditRecord`, `ComputeAuditRowHash`), `internal/store/sqlite/sqlite.go` and `internal/store/postgres/postgres.go` (`appendAuditTx`, `VerifyAuditChain`, `ListAudit`), `internal/store/sqlite/migrations/0001_init.sql` and `0005_audit_chain.sql`, `docs/adr/0009-sanctum-citadel-audit-chain-and-crypto-approvals.md`, issues 157 and 104.
- RFC 2104 (HMAC), FIPS 180-4 (SHA-256).
