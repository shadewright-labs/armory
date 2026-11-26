# ADR-0001: Git host clients move into armory as package `githost`

- Status: Proposed
- Date: 2026-11-26

Nothing in this ADR is built. Statements about Azure DevOps cite the Microsoft Learn pages listed under "Sources", read on 2026-11-26. Statements about GitHub cite either the Shadewright adapter as it is on `main` today or the GitHub REST pages listed under "Sources". Anything the pages do not say is marked **spike**: a stage must confirm it against a test repository before the code that depends on it is built.

## Context

Two products of the organisation talk to Git hosts over HTTP.

**Shadewright** has a hardened GitHub adapter, `internal/providers/github`, of about 4,240 production lines: `provider.go` (1,690), `client.go` (549), `feedback.go` (519), `handoff.go` (341), `validate.go` (111), `ancestry.go` (73), `doc.go` (83), the `auth` subpackage (779: App JWT, installation token cache, repository URL rules, a `Token` type that prints as a placeholder and has an explicit `Reveal`) and `ratelimit` (92), plus `internal/providers/retryafter` (43). It implements `GitProvider` (26 methods in `internal/providers/git.go`), `FeedbackSource` and `AncestryChecker`. Its rules:

- one repository and one target branch per instance, fixed at construction. Branch-ref mutations go only to `shadewright/<one safe segment>`; other writes target a commit (check runs) or a pull request (comments, and the finalizer's close of an owned pull request on any base);
- one host, `https://api.github.com`, pinned in code; no redirects; TLS 1.2 or later; every body read with a bound. The default JSON bound is 2 MiB; a feedback page may stream up to 30 items of 400 KiB, and `auth` reads at most 64 KiB;
- blobs and trees built with the Git Data API and checked (the blob hash is verified; tree and commit IDs are checked for shape only). The commit request carries no author, committer or dates, so the host chooses the commit identity. The branch moves with a GraphQL `updateRefs` compare-and-swap on the expected head, addressed by the repository's node ID, never forced. A new branch is created after a target ancestry check with `POST /git/refs`; a 422 is explained by reading the branch afterwards;
- error statuses classed from status code and headers, not from the body, with two exceptions: a 403 body is searched with a bound for a secondary rate limit, and a GraphQL answer with status 200 is classed by its typed `RATE_LIMITED` error. Errors carry fixed text, the status code and a class where there is one, never a URL, header, body, repository, branch or token;
- retries with backoff and jitter, by default at most 4 attempts, and a delay GitHub asks for waited in place up to 15 seconds by default (both configurable); a longer one is returned as a `RetryableError` so the caller requeues. Branch creation has its own 422 loop; a comment is sent once;
- a write checkpoint (the kill switch, `providers.CheckWrite`) called right before every write request and every retry of one, after the token and the body are ready. Minting the installation token is outside the checkpoint on purpose;
- the App private key never enters the process: the JWT digest is signed through a `Signer` interface backed by Key Vault ([ADR-0006](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0006-github-app-key-in-key-vault.md)). `Signer.Algorithm()` returns the named type `providers.SignatureAlgorithm`.

Shadewright's [ADR-0032](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0032-azure-devops-as-a-second-git-provider.md) (Proposed) maps most `GitProvider` methods to Azure DevOps (there is no dedicated row for `CheckBetween`, `CountAhead`, `ListChangeRequests` or `ReviewCheckRun`) and picks a federated Entra identity with no stored key.

**Sablecourt** (the second product) has three small pieces: a `git.open_pr` action that posts to `/repos/{owner}/{repo}/pulls` with a token from its secret store (owner and repository come from the action input and the API base URL is configurable; the shipped wiring uses `http.DefaultClient`, so redirects are followed and a successful answer is decoded without a size bound, although the type accepts another client); a GitHub OAuth device flow with its own client, whose default scope is `repo`; and a webhook handler that checks `X-Hub-Signature-256`. A local `git` CLI wrapper runs each task in a worktree on a branch `sablecourt/<task>` from a recorded `Base`. When a task ends it may add zero, one or two snapshot commits on top of whatever the harness committed, records `Tip` and removes the checkout. Nothing makes the branch one commit on the base today. Sablecourt now needs two things it does not have: pushing a finished task branch to the host with the approval pinned to what is pushed, and reading the checks of a pull request it opened and merging it on approval at the reviewed commit, never bypassing branch rules.

Without a shared package Sablecourt would build push, merge and check reads on its own client, and later a second Azure DevOps client. Shadewright's [ADR-0048](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0048-redaction-in-a-shared-module.md) (accepted) sets the rules armory releases are consumed under: tags on a protected `main`, checksum database, attestation check on bumps, reviewed exact-version bumps, a dependency ceiling, no product dependency. None of it is operational for armory yet: armory has no `go.mod`, no release and no tag ruleset.

## Decision

1. Armory gets a package `githost`: a host-neutral client for one repository, a GitHub implementation now and an Azure DevOps implementation later. It shares what both products need: the transport, credentials, rate limits, the write gate, refs, commits, pull requests, commit-bound checks and merge. Logic only one product needs stays in that product.
2. Armory holds no long-lived credential and makes no authorization decision. A product passes a credential source and a write gate. Every write is described by an immutable, operation-specific value (a `githost.Operation`) that the product approves; the client derives each request from it and asks the gate before each send. Unknown operations are denied.
3. The HTTP rules of the Shadewright adapter become the rules of the package, for every host and every caller.
4. A host that cannot enforce an approved expectation before state changes returns `ErrUnsupported` before it sends anything. There is no publish-then-roll-back path.
5. Migration runs in three stages. Stage A builds the package with GitHub and Sablecourt adopts it for the push and merge work. Stage B moves Shadewright's transport, App authentication and its ref, commit, pull request and check-run HTTP onto it; Shadewright's own rules, feedback, comments and receipts stay in its adapter on top of the shared transport. Stage C adds Azure DevOps.
6. The package uses only the Go standard library. Azure identity libraries stay in the products, behind the credential interface.

### Package layout

```text
githost/                     core types, Operation, Reader, Writer, Repo, Credential, WriteGate, errors, Token
githost/github/              GitHub client: githost.Repo, GitHub-only methods, bounded repository reads (Get)
githost/github/app/          GitHub App credential: JWT signed through githost.Signer, installation token cache
githost/azuredevops/         Azure DevOps client (stage C)
githost/githosttest/         in-memory fake, contract suite (RunContract) and the recorded-fixture server, all
                             exported so a test in another module can run them
githost/internal/transport/  pinned-origin HTTP client, the one send path, bounded reads, retry, Retry-After,
                             error scrubbing
```

The recorded-fixture server lives in `githosttest` (not under `internal/`) because a Shadewright test must be able to start it for the stage B differential test. It is the only way a client is pointed at a non-production URL, through a constructor that exists only in `githosttest`; production constructors take no base URL.

### The exported core

```go
// Package githost is a client for one repository on a Git host. It decides nothing about what a caller
// may do: the product approves each Operation and supplies a Credential and a WriteGate.
package githost

import (
	"context"
	"io"
	"time"
)

// Host names a Git host.
type Host string

const (
	GitHub      Host = "github"      // https://api.github.com only
	AzureDevOps Host = "azuredevops" // https://dev.azure.com only
)

// ObjectID is a full Git object ID: 40 lowercase hexadecimal characters (SHA-1). SHA-256 repositories
// are ErrUnsupported until a product needs them.
type ObjectID string

// Valid reports whether id is a full object ID. Branch names and short IDs are never accepted.
func (id ObjectID) Valid() bool

// RepoID is the host's immutable repository ID: the GitHub numeric ID, or the Azure DevOps GUID.
type RepoID string

// Binding is the one repository a client is built for. Every field comes from the product's trusted
// configuration; none is learned from the host. On every read that resolves the name, the host's answer
// must carry ID, or the call is ErrUntrustedAnswer.
type Binding struct {
	Host    Host
	Owner   string // GitHub owner, or Azure DevOps organization
	Project string // Azure DevOps project; empty on GitHub
	Name    string // repository name
	ID      RepoID // pinned; also part of every Operation
}

// Ref is a full branch ref, "refs/heads/<name>", validated as a Git ref name. No other namespace.
type Ref string

// Capabilities says what the bound host can enforce, so callers branch on facts, not on Host. A false
// value means the matching Writer method returns ErrUnsupported before it sends anything.
type Capabilities struct {
	DraftPullRequests bool
	MergeStrategies   []MergeStrategy
	// VerifiedPublish is true when the client can check the IDs of a new commit and its tree before
	// any ref moves (GitHub: separate object and ref requests). Azure DevOps pushes move the ref in the
	// same request, so it is false there.
	VerifiedPublish bool
	// AtomicMerge is true when the host refuses, in the merge request itself, a merge whose head is not
	// the approved head (GitHub: the sha field of the merge request).
	AtomicMerge bool
}

// Mode is a Git tree entry mode.
type Mode string

const (
	ModeFile       Mode = "100644"
	ModeExecutable Mode = "100755"
	ModeDir        Mode = "040000"
	ModeSymlink    Mode = "120000"
	ModeSubmodule  Mode = "160000"
)

// TreeEntry is one entry of a directory at a commit.
type TreeEntry struct {
	Name string // one path segment, never a slash
	Mode Mode
	ID   ObjectID
}

// TreeLimit bounds one tree read. Zero fields take the package defaults (2 MiB of answer, 10,000
// entries). A tree past either bound, or one the host reports as truncated, is ErrTooLarge, never a
// short list. Callers that compare trees set their own byte bound, as Shadewright does today.
type TreeLimit struct {
	Bytes   int64
	Entries int
}

// File is the content of one regular file at one commit, checked against its blob ID.
type File struct {
	Path   string
	Commit ObjectID
	Mode   Mode
	ID     ObjectID
	Data   []byte
}

// Commit is a commit as the host reports it.
type Commit struct {
	ID      ObjectID
	Tree    ObjectID
	Parents []ObjectID
}

// PullRequestState is the lifecycle state of a pull request.
type PullRequestState string

const (
	Open   PullRequestState = "Open"   // GitHub open, Azure DevOps active
	Closed PullRequestState = "Closed" // GitHub closed unmerged, Azure DevOps abandoned
	Merged PullRequestState = "Merged" // GitHub merged, Azure DevOps completed
)

// PullRequest is a pull request of the bound repository, as the host recorded it.
type PullRequest struct {
	Repo   RepoID
	Number int64
	URL    string // web URL, for people; never fetched by the client
	Head   Ref
	Base   Ref
	// RecordedHead is the head commit the host recorded on the pull request (GitHub head.sha). For a
	// merged pull request it is the head that was merged and stays evidence after the branch moves or
	// is deleted. It is not the live branch head: read that with BranchHead.
	RecordedHead ObjectID
	// HeadRepo is the repository of the source branch. Not equal to Repo means a fork: such a pull
	// request is answered, never written to.
	HeadRepo RepoID
	State    PullRequestState
	Draft    bool
	Title    string // host text; compared, never logged unredacted
	Body     string
	// MergeCommit is set only when State is Merged and the host reports a full commit ID.
	MergeCommit ObjectID
	Mergeable   Mergeability // the host's own view; never decides anything by itself
}

// Mergeability is the host's report on whether a pull request can merge now.
type Mergeability string

const (
	MergeableUnknown  Mergeability = "Unknown"
	MergeableYes      Mergeability = "Yes"
	MergeableConflict Mergeability = "Conflict"
	MergeableBlocked  Mergeability = "Blocked"
)

// PullRequestQuery selects pull requests by head branch. Base empty means any base, which is how a
// product finds an owned head that is open against another base.
type PullRequestQuery struct {
	Head  Ref    // required
	Base  Ref    // optional
	State string // "open" or "all"
	Page  Cursor // zero for the first page
}

// PullRequestPage is one page of a pull request list. An entry with another head, another base than
// asked, or another repository than Repo or HeadRepo allows is ErrUntrustedAnswer for the whole call.
type PullRequestPage struct {
	Items []PullRequest
	Next  Cursor // zero when this is the last page
}

// Cursor is a page position parsed and validated from a host answer (a page number on GitHub, a
// continuation token matching a fixed pattern on Azure DevOps). The client rebuilds the next request on
// the pinned origin from it and never follows a URL the host sent.
type Cursor struct{ /* unexported */ }

// CheckState is the state of one check at a commit.
type CheckState string

const (
	CheckPending CheckState = "Pending"
	CheckPassed  CheckState = "Passed"  // GitHub success
	CheckNeutral CheckState = "Neutral" // GitHub neutral or skipped; never Passed in this package
	CheckFailed  CheckState = "Failed"  // any failing conclusion or error
)

// CheckSource says what kind of commit-bound host object a check is.
type CheckSource string

const (
	SourceCheckRun     CheckSource = "CheckRun"     // GitHub only
	SourceCommitStatus CheckSource = "CommitStatus" // both hosts
)

// Check is one check bound to one commit. Name is host text: callers compare it with names from their
// own configuration and never display or log it unredacted.
type Check struct {
	Source CheckSource
	Name   string
	// Origin is who produced it: the GitHub App ID of a check run, or the creator's account ID of a
	// commit status. Never empty: an entry without one is ErrUntrustedAnswer.
	Origin string
	ID     int64 // the host's ID of the run or status, for choosing the latest
	State  CheckState
}

// CheckQuery narrows a commit check read. Zero fields mean every check, latest run per name.
type CheckQuery struct {
	Name   string // only checks with this name
	Origin string // only checks from this producer
	All    bool   // every run, not only the latest per name (GitHub filter=all)
}

// CheckReport is every check the client read for one commit, with every page read: an incomplete read
// is ErrTooLarge, never a short report.
type CheckReport struct {
	Repo   RepoID
	Commit ObjectID // the commit asked for; an entry for another commit is ErrUntrustedAnswer
	Checks []Check
	ReadAt time.Time
}

// MaxChecks bounds a CheckReport; more is ErrTooLarge.
const MaxChecks = 300

// Reader reads one repository. A Reader built with a read-only credential holds no write method.
type Reader interface {
	Binding() Binding
	Capabilities() Capabilities

	// BranchHead returns the live head of a branch. ErrNotFound when the branch does not exist.
	BranchHead(ctx context.Context, ref Ref) (ObjectID, error)

	// Commit returns a commit's tree and parents. ErrNotFound when it does not exist.
	Commit(ctx context.Context, id ObjectID) (Commit, error)

	// ReadTree lists one directory at a commit, without recursion, sorted by name, with every mode,
	// type and ID. Duplicate names are ErrInvalidEntry. dir is "." or a clean relative path; a path that
	// is not a directory there is ErrPathNotInTree.
	ReadTree(ctx context.Context, commit ObjectID, dir string, lim TreeLimit) ([]TreeEntry, error)

	// ReadFile returns a regular file at a commit, at most limit bytes (clamped to MaxFileBytes), with
	// the size checked before the content is downloaded and the content checked against its blob ID.
	// A symbolic link, submodule or directory is ErrPathNotInTree; it is never followed.
	ReadFile(ctx context.Context, commit ObjectID, path string, limit int64) (File, error)

	// PullRequest returns the pull request with number, in any state. ErrNotFound when there is none in
	// this repository; an answer for another number or repository is ErrUntrustedAnswer.
	PullRequest(ctx context.Context, number int64) (PullRequest, error)

	// ListPullRequests returns one page of pull requests matching q, newest first.
	ListPullRequests(ctx context.Context, q PullRequestQuery) (PullRequestPage, error)

	// CommitChecks reads the check runs and commit statuses bound to commit. It says nothing about a pull
	// request: whether the checks are enough to merge, and which producers are trusted, is the caller's
	// decision. Pull request scoped evaluations (Azure DevOps policies) are a different type in the
	// azuredevops package and never appear here.
	CommitChecks(ctx context.Context, commit ObjectID, q CheckQuery) (CheckReport, error)
}

// MergeStrategy is how a pull request lands on its base.
type MergeStrategy string

const (
	MergeCommit MergeStrategy = "Merge"  // two-parent merge; Azure DevOps noFastForward
	Squash      MergeStrategy = "Squash" // one single-parent commit
	Rebase      MergeStrategy = "Rebase" // commits replayed on the base, base fast-forwarded
)

// MergeResult is what the host reported for a merge.
type MergeResult struct {
	Merged      bool
	MergeCommit ObjectID // empty when the host reports none yet; read PullRequest later
}

// CommitSource gives the client the objects of a commit the product has locally (Sablecourt reads them
// from its repository). The client recomputes every ID from these bytes before the first upload.
type CommitSource interface {
	// Object returns the raw content of a blob, tree or commit object (without the Git header).
	Object(ctx context.Context, id ObjectID) (kind string, data io.Reader, size int64, err error)
}

// Writer writes to one repository. Each method takes an approved Operation and sends only requests
// derived from it. Before each send it calls the WriteGate (see "Write authorization").
type Writer interface {
	// PublishCommit uploads the objects of op.Commit and creates or moves op.Ref to it. Before any upload
	// it recomputes the commit, tree and blob IDs from src and refuses with ErrConflict when they differ
	// from op, and refuses with ErrUnsupported anything outside op's limits (see PublishCommit). After the
	// uploads it compares the host's IDs with op and moves the ref only when every one matches. ErrConflict
	// when the ref is not at op.Old. ErrUnsupported when Capabilities.VerifiedPublish is false.
	PublishCommit(ctx context.Context, op PublishCommit, src CommitSource) (Commit, error)

	// PublishHostCommit builds one commit from changes on op.Parent with the identity the host chooses,
	// checks the host's tree ID against op.Tree and its parent against op.Parent, and only then moves
	// op.Ref with a compare-and-swap on op.Old. Shadewright's CommitFile and AppendCommit use it.
	PublishHostCommit(ctx context.Context, op PublishHostCommit, changes []FileChange) (Commit, error)

	// MoveRef creates or moves a branch with a compare-and-swap on op.Old. Creating a branch that already
	// points at op.New succeeds.
	MoveRef(ctx context.Context, op MoveRef) error

	// OpenPullRequest opens op's pull request, or returns the open one from op.Head to op.Base when its
	// title and body match. An open pull request of op.Head to another base is ErrConflict.
	OpenPullRequest(ctx context.Context, op OpenPullRequest) (PullRequest, error)

	// UpdatePullRequest changes title or body of an open pull request whose base and state match op.
	UpdatePullRequest(ctx context.Context, op UpdatePullRequest) (PullRequest, error)

	// ClosePullRequest closes an open pull request whose base matches op. Closing a closed one succeeds;
	// a merged one is ErrConflict.
	ClosePullRequest(ctx context.Context, op ClosePullRequest) (PullRequest, error)

	// Merge merges op.Number only if its head is op.Head, enforced by the host in the merge request.
	// ErrUnsupported before sending when Capabilities.AtomicMerge is false. ErrConflict when the pull
	// request's base or recorded head is not op's; ErrForbidden when a rule or policy of the host refuses,
	// including a merge queue requirement. A lost answer is ErrOutcomeUnknown and is never repeated in
	// the same call.
	Merge(ctx context.Context, op Merge) (MergeResult, error)
}

// Repo is a Reader and a Writer for one repository.
type Repo interface {
	Reader
	Writer
}

// FileChange sets or deletes one file in a host-built commit.
type FileChange struct {
	Path    string
	Mode    Mode   // ModeFile or ModeExecutable; ignored for Delete
	Content []byte // nil with Delete
	Delete  bool
}

// Bounds, the same on every host.
const (
	MaxFileBytes   = 1 << 20  // largest file ReadFile returns, and largest blob a write uploads
	MaxChanges     = 200      // changed paths in one commit
	MaxCommitBytes = 16 << 20 // blob content of one commit in all
)
```

### Write authorization

The product approves a write as an `Operation`: an immutable value that names the repository by its pinned ID, the exact refs or pull request, the expected old and new IDs, and the payload of that action or its digest. The set of operations is closed (the interface has an unexported method), so a gate can switch on the concrete type and deny everything else. Host-specific writes are operations of the same set.

```go
// Operation is one write a product approves. The set is closed: only the types below implement it.
type Operation interface{ operation() }

// Digest returns the SHA-256 of op's canonical encoding (type name, then each field in a fixed order).
// A product stores it with its approval and compares it in its WriteGate.
func Digest(op Operation) [32]byte

// PublishCommit publishes one commit the product already has, with its exact ID.
type PublishCommit struct {
	Repo   RepoID
	Ref    Ref
	Old    ObjectID // expected head; empty means the ref must not exist
	Commit ObjectID // the exact commit the ref will point at
	Tree   ObjectID // its tree
	Parent ObjectID // its single parent; for a new ref, also the recorded base
	Limits PublishLimits
}

// PublishLimits is what a PublishCommit may contain. Anything else is ErrUnsupported before any upload:
// a signed commit (a gpgsig header), extra headers, more or fewer than one parent, a mode outside Modes,
// a submodule, a path or size past the bounds.
type PublishLimits struct {
	Modes []Mode // stage A: ModeFile, ModeExecutable, ModeDir
}

// PublishHostCommit publishes a commit the host builds; its ID is not known before the upload.
type PublishHostCommit struct {
	Repo          RepoID
	Ref           Ref
	Old           ObjectID // expected head; empty means the ref must not exist
	Parent        ObjectID
	Tree          ObjectID // the tree the client computes from Parent and the changes, before any upload
	MessageDigest [32]byte // SHA-256 of the commit message
}

// MoveRef creates or moves one ref to a commit that already exists on the host.
type MoveRef struct {
	Repo RepoID
	Ref  Ref
	Old  ObjectID // empty means create
	New  ObjectID
}

// OpenPullRequest opens a pull request from Head at HeadCommit to Base.
type OpenPullRequest struct {
	Repo        RepoID
	Head        Ref
	HeadCommit  ObjectID // the live head must be this before the create
	Base        Ref
	Title, Body string
	Draft       bool
}

// UpdatePullRequest changes the title or body of an open pull request. Nil means unchanged.
type UpdatePullRequest struct {
	Repo        RepoID
	Number      int64
	Base        Ref
	Title, Body *string
}

// ClosePullRequest closes a pull request without merging. AnyBase lets a product close an owned pull
// request whatever its base (Shadewright's finalizer); otherwise Base must match.
type ClosePullRequest struct {
	Repo    RepoID
	Number  int64
	Head    Ref
	Base    Ref
	AnyBase bool
}

// Merge merges one pull request at one head onto one base.
type Merge struct {
	Repo     RepoID
	Number   int64
	Base     Ref
	Head     ObjectID
	Strategy MergeStrategy
	Title    string // merge commit title, optional
	Message  string // merge commit message, optional
}

// CreateCheckRun creates one GitHub check run on a commit.
type CreateCheckRun struct {
	Repo          RepoID
	Commit        ObjectID
	Name          string
	Status        string // "completed" or "in_progress"
	Conclusion    string // for completed: "success", "failure", "neutral", ...
	SummaryDigest [32]byte
}

// PostComment posts one GitHub issue comment on a pull request. It is never retried in the call.
type PostComment struct {
	Repo       RepoID
	Number     int64
	BodyDigest [32]byte
}

// Request names the host request about to be sent for an operation: a fixed label such as
// "github.CreateBlob", "github.CreateTree", "github.CreateCommit", "github.UpdateRefs",
// "github.CreatePull", "github.MergePull", "github.CreateCheckRun".
type Request string

// WriteAttempt is what the gate sees before one send.
type WriteAttempt struct {
	Op      Operation
	Digest  [32]byte // Digest(Op)
	Request Request
	Attempt int // 1 for the first send of this request in the call, then each retry
}

// WriteGate is the product's last check before a write request leaves the process. Returning an error
// stops the call: nothing more is sent. It must be quick and must not call the client.
type WriteGate func(ctx context.Context, w WriteAttempt) error
```

The rules:

- Every write request comes from the operation. Every path, query and body field of a write is built from the `Operation` and from values the client read and checked; nothing comes from free host text. A method with any argument besides the operation (the `CommitSource`, the `FileChange` list) verifies that argument against the operation before the first upload.
- Uploads are checked before they start. `PublishCommit` reads the commit from the `CommitSource`, refuses a signed or multi-parent commit, extra headers, modes outside `Limits` and anything past the bounds, recomputes every blob, tree and commit ID from the bytes, and compares them with the operation. Only then does it upload. After the uploads it compares the host's IDs with the operation and moves the ref only when all match. `PublishHostCommit` computes the tree ID locally from the parent's tree (read from the host) and the changes, compares it with `Tree`, and checks the message digest before uploading. A refusal after an upload leaves unreferenced objects at the host; their content was approved, and no ref points at them.
- The gate sees the approved value. A product compares `WriteAttempt.Digest` with the digest it approved (Sablecourt), or switches on the operation type and its fields (Shadewright's stale-plan rule lets through only a `CreateCheckRun` with the review check's name and a failed conclusion, and a `ClosePullRequest`). Its default branch denies.
- Refusal classes survive. `Config.GateClasses` lists the product's refusal sentinels (Shadewright: `ErrPaused`, `ErrWriteBlocked` and its record-hold error). A gate error that matches one comes back as `ErrStoppedBeforeWrite` wrapping that sentinel, with the gate's own text dropped. Any other gate error comes back as `ErrStoppedBeforeWrite` alone.
- Unknown operations are denied by the gate (its default branch) and by the client: a host package that receives an operation it does not implement returns `ErrUnsupported` before sending.

### Constructors and GitHub-only methods

```go
package github

// Config binds a Client to one repository.
type Config struct {
	Repo        githost.Binding    // pinned, from trusted product configuration
	Credential  githost.Credential // required
	Gate        githost.WriteGate  // required for New; NewReader takes none
	GateClasses []error            // refusal sentinels whose identity is kept
	Observer    githost.Observer
	MaxAttempts int           // 0 means 4
	BaseDelay   time.Duration // first backoff step; 0 means 1s
	MaxWait     time.Duration // longest delay waited in place; 0 means 15s
	Timeout     time.Duration // one method call, retries included; 0 means 2m
}

func New(cfg Config) (*Client, error)       // *Client implements githost.Repo
func NewReader(cfg Config) (*Reader, error) // *Reader implements githost.Reader only

// GitHub-only methods on *Client. Each write takes an Operation and passes the gate like the core.
func (c *Client) CreateCheckRun(ctx context.Context, op githost.CreateCheckRun, summary, text string) error
func (c *Client) PostComment(ctx context.Context, op githost.PostComment, body string) (int64, error)
func (c *Client) Compare(ctx context.Context, base, head githost.ObjectID) (Comparison, error)

// Get reads one resource of the bound repository for logic that stays in a product: comments, reviews,
// feedback pages, permissions, rules. It sends only GET, on the pinned origin, to a path under the bound
// repository built from escaped segments, with query keys from a fixed allow-list. decode reads the body
// as a stream, bounded by r.Limit. A 304 is returned as Response.NotModified when r.IfNoneMatch was set,
// and is an error otherwise.
func (c *Client) Get(ctx context.Context, r ReadRequest, decode func(Response) error) error

type ReadRequest struct {
	Op          string   // fixed label for errors and the Observer
	Path        []string // segments under /repos/{owner}/{name}/; empty, "." and ".." refused
	Query       url.Values
	Page        githost.Cursor
	IfNoneMatch string
	Limit       int64 // at most 16 MiB; 0 means 2 MiB
}

type Response struct {
	NotModified bool
	ETag        string
	Date        time.Time
	Next, Last  githost.Cursor
	Body        io.Reader // valid only inside decode
}
```

`Get` is how Shadewright keeps its feedback, comment marker and permission logic in its own adapter without its own HTTP client. It cannot write, cannot leave the repository's path and gives the caller no header.

```go
package app

// Config of a GitHub App credential for one installation and one repository.
type Config struct {
	ClientID       string
	InstallationID int64
	Repo           githost.Binding   // the token is requested for this repository ID only
	Signer         githost.Signer    // the private key stays in the key store
	Permissions    map[string]string // requested installation permissions; the product's choice
	Timeout        time.Duration     // token exchange; 0 means 30s
	RefreshMargin  time.Duration     // 0 means 5m
}

func New(cfg Config) (*Source, error) // validates offline; *Source implements githost.Credential
func (s *Source) AppID(ctx context.Context) (int64, error)
```

The token exchange asks for the pinned repository by ID (`repository_ids`, **spike**: the installation token page was not read; Shadewright asks by name today) and checks the grant that comes back: exactly that repository and exactly the requested permissions, as Shadewright's `auth` checks today. It runs on the same transport and the same origin and redirect rules as every other request. It is not a repository write and does not call the write gate, as today.

Azure DevOps (stage C) adds `azuredevops.New(Config{...})` and host-only methods: `PullRequestEvaluations` (pull request scoped, returning its own `PolicyEvaluation` type, never a `Check`), `Iterations`, `Threads`, `PostThread`, `PostPullRequestStatus` (on an iteration), `CommitStatuses`. The Graph membership read of ADR-0032 needs a second host (`vssps.dev.azure.com`) and is not part of the package until a product needs it.

### What is deliberately host-specific

GitHub check run creation, issue comments and reviews with their paging, permission reads, compare and rulesets. Azure DevOps pull request statuses on iterations, threads, votes, policy evaluation records and Graph memberships. These differ in shape (ADR-0032 section 3 has the details for Shadewright's use), and a neutral wrapper would hide differences the products' rules depend on: an Azure DevOps vote or policy evaluation carries no commit, and a GitHub comment marker depends on bot and App identity.

### Repository identity

The immutable repository ID is pinned in the product's trusted configuration and is a field of every `Operation`; a client refuses an operation for another ID before sending. How each write is bound to it on GitHub:

| Write | Addressed by | Binding |
| --- | --- | --- |
| Ref create and move | GraphQL `updateRefs` with the repository node ID (today's adapter, for moves) | the node ID is read once per client and checked against the pinned numeric ID; the mutation itself is bound. Creating a ref through `updateRefs` instead of `POST /git/refs` is **spike** |
| Blob, tree, commit uploads | REST, by owner and name | name check before the write |
| Pull request create, update, close; merge; check run; comment | REST, by owner and name | name check before the write |

"Name check before the write" means: the client reads `GET /repos/{owner}/{name}` and compares the numeric ID with the pinned one before the first write of each call. This is a race. If the repository is renamed, transferred or deleted and recreated under the same name between that read and a write, the write lands in the replacement. For uploads that leaks only approved content; for a pull request, merge, check run or comment it is a write to the wrong repository.

What narrows it is the credential. An App installation token requested with `repository_ids` set to the pinned ID cannot write anywhere else, so a name-addressed write to a replacement fails at the host. A fine-grained token selected for one repository has the same effect. A user OAuth token with the `repo` scope (Sablecourt's device flow default) can write to every repository of the user; with such a token the window between the check and the write is the only protection. Stage A therefore requires a repository-restricted credential for Sablecourt's push and merge.

### Authentication

`Token` has no exported way to read its value or put it into a header:

```go
// Token is a short-lived secret for one host. It prints, formats, logs and marshals as a placeholder.
type Token struct{ /* unexported: scheme, value behind a pointer, expiry */ }

// Scheme is how a Token is sent.
type Scheme int

const (
	Bearer Scheme = iota + 1 // GitHub tokens, Azure DevOps Entra tokens
	Basic                    // Azure DevOps personal access token (spike, stage C)
)

// NewToken wraps value. The product builds tokens in its Credential; armory never stores one beyond
// its expiry.
func NewToken(s Scheme, value string, expires time.Time) Token

// Expires returns when the token stops being valid; zero when the source did not say.
func (t Token) Expires() time.Time

// String, GoString, Format, LogValue, MarshalJSON and MarshalText return a placeholder.

// Scope is what a token is asked for: one repository, and whether the call writes.
type Scope struct {
	Repo  RepoID
	Write bool
}

// Credential returns a token for one request. The client asks before each request and does not keep
// the token; caching and refresh are the credential's business.
type Credential interface {
	Token(ctx context.Context, s Scope) (Token, error)
}

// CredentialFunc adapts a function to Credential.
type CredentialFunc func(ctx context.Context, s Scope) (Token, error)

// SignatureAlgorithm names the algorithm a Signer uses.
type SignatureAlgorithm string

const RS256 SignatureAlgorithm = "RS256"

// Signer signs a digest with a key that never leaves its key store (GitHub App JWT).
type Signer interface {
	Algorithm() SignatureAlgorithm
	Sign(ctx context.Context, digest []byte) ([]byte, error)
}

// Observer receives one payload-free event per HTTP attempt (request label, status class, attempt,
// duration, whether a delay was asked for). Nil means none. It never receives an error value.
type Observer func(Event)
```

Only `internal/transport` installs a token: on the final request, after checking that its scheme is `https`, its host is the pinned host exactly and it has no user info or non-default port. Redirects are refused, so the request that carries the token is the one checked. Shadewright aliases `providers.SignatureAlgorithm` and `providers.Signer` to these, so its Key Vault signer does not change.

| Credential | Who holds it | How it reaches armory |
| --- | --- | --- |
| GitHub App installation token | Key in a key store; product supplies the `Signer` (Shadewright: Key Vault) | `app.New(...)`, restricted to the pinned repository ID |
| GitHub fine-grained token for one repository | Product's secret store | `CredentialFunc` reading the store per call |
| GitHub user OAuth token (device flow) | Product's secret store; the device flow and its own HTTP client stay in the product | `CredentialFunc`; reads only, or the existing opener, until a restricted credential replaces it |
| Azure DevOps Entra token | Product's workload identity library | `CredentialFunc` wrapping it, `Bearer` |
| Azure DevOps personal access token | Product's secret store | `CredentialFunc`, `Basic` (**spike**: the PAT page was not read; ADR-0032 does not support PATs for Shadewright) |

Armory never reads a file, an environment variable or a secret store, and never writes a token anywhere. `Scope.Write` lets a product hand out a narrower token for reads.

Workloads never receive a `Credential`, a `Token`, a `*github.Client` or any `Writer`. In Sablecourt the client lives in the gate process and agents reach it only through gate actions; in Shadewright only the executor holds the Git App and the analyzer sees the typed `GitProvider` through the controller.

### What stays in each product

Shadewright:

- `GitProvider`, `FeedbackSource` and `AncestryChecker` as its product surface, and its fake (`internal/providers/fake/git.go`).
- Branch rules and proofs: `CheckBranch`, `CheckChain`, `CheckCommit`, `CheckBetween`, `CheckMerged`, `CountAhead`, `IsAncestor`, the `shadewright/` prefix, target ancestry for creation, the opposite condition for append, single-parent and allowed-path proofs, tree comparisons with its own byte bound, directory caching and symlink and submodule filtering in `ListDir`.
- Pull request rules: refusing an owned head open against another base, ignoring forks on the open lookup, `maintainer_can_modify: false` on create, the update no-op, closed and merged checks before payload validation, conflicting duplicate detection, fork-as-not-found by number, the any-base close of the finalizer, `EnableAutoMerge` answered as unsupported and `Capabilities` values answered locally.
- Merge receipts (ADR-0030) built on `PullRequest.RecordedHead`, never on the live head.
- `ReviewCheckRun`: reading its own App's check by name with `All`, choosing the greatest run ID, mapping neutral and skipped to success for its own decision. The core returns `CheckNeutral` and leaves that mapping to the product.
- Comments and feedback on `github.Client.Get`: marker search by whole trimmed line, newest pages first, rereading page one after deletions, `ErrTooManyComments`, posting at most once; feedback with `since`, `Date`, ETag and 304, per-item resume positions, streamed pages, review commit and time, foreign-item refusal; the login-addressed permission read checked against the original author.
- Error translation: mapping `githost` kinds to its sentinels, `ErrInvalidCommit` for a malformed ID, and keeping its refusal classes through `GateClasses`.
- `internal/providers/retryafter`, still used by its Prometheus, Foundry, Vertex and Azure Blob clients. Armory takes a copy.
- The write gate: record hold, kill switch and the stale-plan rule.

Sablecourt:

- Policy: which repository and credential, its branch prefix, target bases, the required checks with their trusted producers, approvals over `githost.Digest`, audit and evidence records.
- The local Git worktree and CLI code, including making the task branch one commit before approval.
- The OAuth device flow and its client; webhook verification.

### HTTP transport rules

These hold for every host and are tested in the contract suite.

1. Fixed origins. GitHub: `https://api.github.com`. Azure DevOps: `https://dev.azure.com`. Pinned in code; no production constructor takes a URL. Paths are built from escaped segments only. The next page is rebuilt from a validated `Cursor`, never from a `Link` header URL.
2. No redirects. `CheckRedirect` returns `http.ErrUseLastResponse`; a 3xx is an error, except a 304 on a `Get` that set `IfNoneMatch`. TLS 1.2 or later; dial, handshake and header timeouts; one attempt is bounded at 60 seconds and one method call at `Timeout`. The App token exchange follows the same rules.
3. Bounded reads. A JSON answer is read up to 2 MiB unless the call sets another bound (a `Get` may set up to 16 MiB; the auth exchange uses 64 KiB); a declared `Content-Length` over the bound is refused before reading, and an answer over it is `ErrTooLarge`. File content is bounded by `limit+1`. Lists are bounded by a page count and an item count; past either is `ErrTooLarge`, never a short list.
4. One send path. Every request that is not a GET goes through one unexported function in `internal/transport`, which takes the `Operation` and the `Request` label, builds the request, gets the token, installs it after the origin check, calls the gate as the last step, and then calls `http.Client.Do`. GET requests go through a second function whose type cannot carry another method. An **attempt** is one call to `Do`; the gate is asked before each one. `net/http` may resend a request on a reused connection when it knows nothing of the request was written; that is the same attempt, since no byte reached the host.
5. Retries. Reads are retried on network errors, timeouts, 408, 429, 5xx and rate limits, with exponential backoff from `BaseDelay` and jitter, up to `MaxAttempts`. A write is retried only where repeating it is safe: object uploads, a ref move with an expected old value (a refused move is explained by reading the ref: at the new commit is success, at another commit `ErrConflict`, unchanged is an unexplained refusal), a pull request create preceded by a list, and a check run create (as today; a second run with the same content changes nothing for a reader that takes the greatest ID). A merge, a comment and an Azure DevOps push whose answer was lost are not repeated in the call; they return `ErrOutcomeUnknown`.
6. Rate limits. GitHub: `Retry-After` (seconds or an HTTP date), then `X-RateLimit-Remaining: 0` with `X-RateLimit-Reset`, then one minute for a secondary limit or an unreadable 403, a GraphQL `RATE_LIMITED` error in a 200 answer, and a cap of one hour (the current `ratelimit` and `retryafter` rules). Azure DevOps: `Retry-After` (**spike**: the rate limit page was not read). A delay up to `MaxWait` is waited in place; a longer one returns a `RetryableError` carrying it. Nothing waits past the context deadline.
7. No logging. The package imports no logger. Bodies, headers, tokens and error values never reach the `Observer`.
8. Error text. Every error the package returns is built from fixed text, a request label, the status code and a kind. Errors from `net/http`, the `Credential`, the `Signer`, the `WriteGate` and JSON decoding are never wrapped as values: only their class is kept (canceled, deadline, retryable, a listed gate class). A 403 body may be searched with a bound for a rate limit marker, never kept.
9. Answers are checked. Repository ID, pull request number, head, base and commit are compared with what was asked; a mismatch is `ErrUntrustedAnswer` and is not retried.
10. A context that ended first wins. An answer that raced the end of the call's context is dropped (Shadewright issues #767 and #779).

### Error kinds

All are sentinels for `errors.Is`, in package `githost`:

| Kind | Meaning | Retry |
| --- | --- | --- |
| `ErrNotFound` | object does not exist | no |
| `ErrPathNotInTree` (wraps `ErrNotFound`) | path is not a regular file or directory there | no, stable for that commit |
| `ErrConflict` | stale state: head moved, branch exists elsewhere, base changed, merged when closing, recomputed ID differs from the operation | caller re-reads |
| `ErrForbidden` | identity lacks a permission, or a rule, policy or merge queue requirement refused | no |
| `ErrUnauthorized` | credential refused (401) | no |
| `ErrRejected` | host refused the request as invalid and state does not explain it | no |
| `ErrTooLarge` | answer, file, list, tree or commit past a bound, or an incomplete paged read | no |
| `ErrInvalidEntry` | tree entry no tree can hold, or a duplicate name | no |
| `ErrUntrustedAnswer` | host answered about another object or repository | no |
| `ErrUnsupported` | the host cannot enforce what the operation requires, or the operation is outside the limits; nothing was sent | no |
| `ErrInvalidConfig` | constructor input unusable | no |
| `ErrStoppedBeforeWrite` | the product's `WriteGate` refused; wraps the matching `GateClasses` sentinel if any; nothing more sent | product decides |
| `ErrOutcomeUnknown` | a write was sent and its answer lost; never a pre-send refusal | read state, then decide |
| `RetryableError` (matches `ErrRetryable`) | transient; `RetryAfter` when the host asked | yes, by caller |

`context.Canceled` and `context.DeadlineExceeded` are kept as themselves, so a caller tells a canceled call from a retryable deadline. Shadewright maps these kinds to its own sentinels in its adapter; `ErrOutcomeUnknown` is a separate case there, never a refusal before the send.

### Mapping to GitHub and Azure DevOps

| Core method | GitHub | Azure DevOps | Gap |
| --- | --- | --- | --- |
| `BranchHead` | `GET /git/ref/heads/{b}` (today's adapter) | `GET .../refs?filter=heads/{b}`, starts-with match, take the exact name (ADR-0032) | none |
| `Commit`, `ReadTree`, `ReadFile` | Git Data API trees and blobs, blob hash checked (today's adapter) | Commits, Trees and Items APIs | **spike** (ADR-0032 slice 1): size before content, how links and submodules show, truncation |
| `MoveRef` | move with GraphQL `updateRefs` compare-and-swap by node ID (today's adapter); create with `POST /git/refs`, reading the branch after a 422 (today's adapter) | `POST .../refs` with `GitRefUpdate[]` (`name`, `oldObjectId`, `newObjectId`); the page says to "specify both the old and new commit to avoid race conditions"; `staleOldObjectId` maps to `ErrConflict`; `forcePushRequired`, `createBranchPermissionRequired`, `writePermissionRequired`, `rejectedByPolicy` to `ErrForbidden` | create through `updateRefs` by node ID is **spike** (see "Repository identity") |
| `PublishCommit` | blobs, tree, `POST /git/commits` with `parents`, `author`, `committer` and their dates; every ID compared with the operation before the ref moves; `VerifiedPublish` true | `ErrUnsupported`: `POST .../pushes` creates the commit and moves the ref in one request, so the commit would be published before its IDs are checked; `VerifiedPublish` false | GitHub keeping the timezone offset and the exact message bytes is **spike**; a mismatch is refused before the ref moves, so it limits use, not safety. Azure DevOps: a design that pushes to a staging ref first publishes the commit there too and needs its own decision in stage C |
| `PublishHostCommit` | blobs, tree with `base_tree`, `POST /git/commits` with `parents` only (today's adapter), then the ref move | `POST .../pushes` with `refUpdates[{name, oldObjectId}]`; the tree ID is known only after the ref moved, so `ErrUnsupported` when the operation binds a tree | `GitChange` has no file mode field |
| `OpenPullRequest` | list `GET /pulls?head={owner}:{b}&state=open` across bases (today's adapter), then `POST /pulls` with `draft` and `maintainer_can_modify: false`; a 422 is explained by listing again, its body is not read | list `GET .../pullrequests?searchCriteria.sourceRefName=&searchCriteria.status=active`, then create; `isDraft` exists on `GitPullRequest` | whether Azure DevOps refuses a second active pull request of the same head and base is **spike** (ADR-0032); the list page states no order, so "newest" is the highest ID |
| `PullRequest`, `ListPullRequests` | `GET /pulls/{n}`, `GET /pulls?head=`; `head.sha` is `RecordedHead`; `mergeable` may be `null` while GitHub computes it, so `MergeableUnknown` | `GET .../pullrequests/{id}` (repository scoped); `status` `active`, `abandoned`, `completed`; `mergeStatus` `queued`, `conflicts`, `succeeded`, `rejectedByPolicy`, `failure` | Azure DevOps `lastMergeSourceCommit` is "the commit at the head of the source branch at the time of the last pull request merge"; whether it can serve as `RecordedHead` is **spike** |
| `UpdatePullRequest`, `ClosePullRequest` | `PATCH /pulls/{n}` (today's adapter), after reading state and base | `PATCH .../pullrequests/{id}`; updatable: status, title, description "up to 4000 characters" | Azure DevOps description over 4000 characters is `ErrRejected` before the send |
| `CommitChecks` | check runs `GET /commits/{sha}/check-runs` (`filter=latest`, or `all` with `CheckQuery.All`; `check_name` and `app_id` when set; 100 a page), statuses `GET /commits/{sha}/statuses` (reverse chronological, latest per context kept unless `All`) | commit statuses `GET .../commits/{id}/statuses`; policy evaluations are pull request scoped and are not read here | GitHub check runs carry `app.id`, statuses `creator.id`. Which checks are required is the product's configuration; the core reads no rules |
| `Merge` | read the pull request (state, base, `RecordedHead`, repository), then `PUT /pulls/{n}/merge` with `sha` ("SHA that pull request head must match to allow merge") and `merge_method`; 409 is `ErrConflict`, 405 `ErrForbidden`; `AtomicMerge` true | `ErrUnsupported` before sending: the Update page does not say a completion checks any head, so `AtomicMerge` is false until a **spike** shows the host refuses a completion whose `lastMergeSourceCommit` is not the source head | GitHub's merge request has no expected-base field: see "Security consequences". GitHub's merge endpoint "does not support ... merging with a merge queue"; such a refusal is `ErrForbidden` and there is no enqueue method |
| Draft | `draft` on create | `isDraft` | none for open; publishing a draft is host-specific |
| Merge commit | `merge_commit_sha` after merge is the merge, squash or rebased commit | `lastMergeCommit`, "empty" while "in progress or ... unsuccessful" | filled only when Merged and a full ID (ADR-0032 section 6) |

### Testing

- Recorded-fixture server per host (`githosttest`). Fixtures are JSON answers recorded from a test repository on each host, then scrubbed (IDs, names, URLs replaced with synthetic ones) and checked by armory's hygiene checks. The server matches method, path, query, headers and body; an unexpected request fails the test. It can inject 3xx, 304, oversize bodies, slow bodies, 403 and GraphQL rate limits, 5xx, lost answers after a write, answers for another object or repository, and a connection closed on a reused connection.
- Contract suite (`githosttest.RunContract`), exported so Shadewright and Sablecourt run it from their own modules. Scenarios: ref create and move with the right and a stale old value; create of a branch that exists at the same commit; `PublishCommit` refused before the first upload for a signed, two-parent, out-of-mode or mismatched commit, and refused before the ref moves when the host's ID differs; `PublishHostCommit` with a mismatched tree; `OpenPullRequest` twice returns one, and an owned head open to another base is `ErrConflict`; `Merge` with a stale head is `ErrConflict`, with a changed base `ErrConflict`, with a lost answer `ErrOutcomeUnknown`, and on a host without `AtomicMerge` `ErrUnsupported` with no request sent; `CommitChecks` with an entry for another commit or without an origin is `ErrUntrustedAnswer`, and an incomplete paged read `ErrTooLarge`; a redirect is never followed and a token never reaches another origin; a `Link` header pointing elsewhere is never fetched; every bound gives `ErrTooLarge`; a canary token, URL, body, gate error text and signer error text never appear in any error text, `%v`, `%#v`, `slog` output or `Observer` event; no request body carries `bypassPolicy`.
- Gate tests, per mutation. For each write request (every REST mutation, the GraphQL `updateRefs` mutation, each host-specific write, each retry and the 422 recovery of branch creation and pull request create), the test refuses at that request and asserts that the next request the server sees is not a write. The suite also counts gate calls against write requests the server received, and asserts that an operation type a host does not implement is `ErrUnsupported` with nothing sent.
- The same contract suite runs against the in-memory fake in `githosttest`. The fake has no HTTP, so redirect, header and wire-order tests run only against the fixture server.
- Fuzzing for every decoder of host answers, the commit and tree parsers used for recomputing IDs, and ref name, path and cursor validation.
- Live smoke tests behind a build tag against the test repositories, run by hand before a release, never in pull request CI.
- Import allow-list test: production packages of `githost` import only the standard library and each other, under their own list, separate from the redaction package's.

### Migration

**Stage A: the package, with GitHub, used by Sablecourt.**

Armory gets `go.mod` (if the redaction work has not created it yet), `githost`, `internal/transport`, `github`, `github/app` and `githosttest`. The transport, retry, rate limit and error code are ported from Shadewright's `client.go`, `ratelimit` and `retryafter` by copy, not by import; Shadewright is unchanged. The release controls of ADR-0048 (tag ruleset, release attestation, checksum verification on bumps, the `githost` import allow-list) are in place before Sablecourt pins the first version, not only before Shadewright does. Sablecourt replaces its `GitHubPROpener` and builds `git.push` and `git.merge_pr` on the package:

- push: the push accepts exactly one unsigned commit with one parent equal to the recorded `Base`, with modes `100644`, `100755` and `040000`, within the bounds, and refuses anything else before any upload. Sablecourt reads the persisted `Base` and `Tip` and the commit's objects from its trusted repository (the checkout may already be gone), and when a task ended with zero or several commits it builds the one commit before asking for approval, never after. The approval is over `githost.Digest` of a `PublishCommit` naming the pinned repository ID, the exact task ref, the expected old head (empty for a new branch), the commit, its tree and its parent. Its `WriteGate` refuses every attempt whose digest is not the approved one.
- checks: `CommitChecks` at the pull request's `RecordedHead`, after checking that it equals the approved head. Each configured check must be present with its expected source, name and trusted producer (`Origin`) and be `Passed`. A missing check, a missing origin, `Neutral`, an incomplete read or a stale report establishes nothing. Sablecourt records repository ID, pull request, base, head and read time with the evidence.
- merge: the approval is over a `Merge` naming the pull request, its base, the head and the strategy. Sablecourt's action gate allows some actions directly today; the merge handler must require this approval itself. Before calling `Merge` it rereads the evidence and that the pull request is its own. `Merge` sends `sha` with the head; the identity it uses has no ruleset bypass, so required rules stay with the host. A merge queue requirement is a refusal. A lost answer means Sablecourt reconciles by pull request number and recorded head before any further write.
- credential: push and merge run with a credential restricted to the pinned repository (see "Repository identity").

What could go wrong: the API surface is fixed before the second host proves it (mitigated by the Azure DevOps column, by `ErrUnsupported` in place of weaker semantics, and by `v0.x` tags, which allow breaking changes with notice); the copied transport drifts from Shadewright's before stage B (mitigated by the stage B differential test); a gate bug in Sablecourt writes with the credential's rights (mitigated by the per-mutation gate tests, the digest check and the repository-restricted credential); GitHub may not reproduce a commit byte for byte (timezone, message), in which case `PublishCommit` refuses before the ref moves and Sablecourt cannot push until the spike finds the cause.

**Stage B: Shadewright moves its shared parts onto the package.**

Shadewright's `internal/providers/github` stays its adapter: it implements `GitProvider`, `FeedbackSource` and `AncestryChecker` as today and keeps every rule listed under "What stays in each product". It stops owning HTTP: transport, App authentication, rate limits, and the requests for refs, trees, blobs, commits, pull requests and check runs come from `githost/github`; feedback, comments, permissions and rules use `github.Client.Get`. What moves, by file, on `main` today:

| Shadewright file | Lines | Where it goes |
| --- | --- | --- |
| `github/client.go` | 549 | `internal/transport` and `github` (most of it already copied in stage A) |
| `github/auth/` (`jwt.go` 98, `source.go` 515, `token.go` 62, `repo.go` 72, `doc.go` 32) | 779 | `github/app`, `githost.Token`, `githost.Signer` |
| `github/ratelimit/ratelimit.go` | 92 | `github` (unexported) |
| `github/provider.go`, the requests that read refs, trees and blobs, build commits, move refs, and read, open, list, update and close pull requests | about 600 of 1,690 | `github` core methods |
| `github/handoff.go`, the check run create and list requests | about 70 of 341 | `github.CreateCheckRun`, `CommitChecks` |
| `github/validate.go`, object ID, path and ref helpers | about 50 of 111 | `githost` |

About 2,100 production lines move and about 2,100 stay: the rest of `provider.go` (branch rules, tree comparisons, pull request rules, marker logic), all of `feedback.go` (519, on `Get`), most of `handoff.go` (receipts and `ReviewCheckRun` selection), `ancestry.go` (73, on `Compare`), `doc.go` and the rest of `validate.go`. `retryafter` (43) stays for its other callers. Tests: `auth/source_test.go` (1,155, with its product dependencies such as the fake signer replaced by a test signer in armory), `ratelimit_test.go` (96), the parts of `provider_test.go` (2,655) and the fixtures of `testdata` (43 files) that cover the moved requests move, split by operation; feedback and comment fixtures stay. `internal/providers/fake/git.go` (1,095) stays: it fakes `GitProvider`, not the HTTP client.

Behaviour of `GitProvider` is preserved for the cases below, because the logic that decides them stays in Shadewright; the differential test proves it for every fixture-driven case:

- `PublishHostCommit` keeps today's host-chosen commit identity and the recovery after a lost answer (a ref move found at the new commit is success; a fresh `CommitFile` builds a new commit).
- Merge receipts use `RecordedHead` and are unaffected by later branch moves or deletion.
- `ListDir` and tree comparisons pass Shadewright's byte bound as `TreeLimit`.
- The 304 of a conditional feedback read is `Response.NotModified`.
- Rate limit delays, `BaseDelay`, the one-hour cap and the GraphQL 200 rate limit behave as today.
- Gate refusals keep `ErrPaused`, `ErrWriteBlocked` and the record-hold identity through `GateClasses`; a lost write is `ErrOutcomeUnknown`, never a refusal.

Known differences, accepted and listed in the stage B pull request: the `User-Agent`, the fixed text of some errors (the differential compares error class and Shadewright's sentinel, and text through a mapping table), and the gate called through `WriteGate` with an `Operation` instead of from the context.

The differential test runs the old adapter and the new one against the same recorded server (started through `githosttest`) and compares, per call, the request sequence (method, path, query, body, headers except `User-Agent`), the result and the error class. It runs on the stage B pull request and on each armory bump while the old adapter is in the tree; the old adapter is removed one Shadewright release later. After that, product-owned compatibility cases stay as Shadewright tests.

What could go wrong: a missed write path would send without the kill switch (the one send path, the per-mutation gate tests and the differential request sequence stand against it); the Key Vault signer moves behind an alias (no new code, the named algorithm type is shared); ADR-0048's governance must be in place before Shadewright imports armory, so stage B waits for the redaction stage B checklist; Shadewright's executor now trusts a second module for its only Git write path.

**Stage C: Azure DevOps.**

`azuredevops` implements `githost.Repo` and the host-specific methods ADR-0032 needs, behind the same contract suite, after ADR-0032's read-only slice answers its spikes and the spikes marked here (merge head enforcement, `RecordedHead`, second active pull request, PAT scheme, rate limit headers). Until a spike shows atomic enforcement, `Merge`, `PublishCommit` and a tree-bound `PublishHostCommit` return `ErrUnsupported` there. Shadewright's `internal/providers/azuredevops` becomes an adapter as in stage B. For both products, "Azure DevOps supported" also needs each product's own preflight of policy configuration and effective permissions, which the package does not supply.

What could go wrong: a spike fails and a core method cannot keep its contract on Azure DevOps (then `Capabilities` reports it and the method returns `ErrUnsupported`; the contract suite has a capability-gated branch, never a weaker assertion); policy evaluations are a preview API (`7.1-preview.1`) and can change; the Entra token is not scoped to a repository (ADR-0032 consequences), so the bound repository and the gate are the only limits inside the process and a name-addressed write keeps the race described above; Azure DevOps Server is out of scope.

## Consequences

- One transport with the hardening Shadewright has, for both products. Sablecourt's opener loses its configurable URL, redirect following and unbounded reads; its OAuth flow keeps its own client and needs its own hardening.
- A fix to the transport, the credential code or the shared operations is written once. It reaches a product only when that product bumps its pin; an urgent fix waits for both bumps.
- A bug in the package hits both products' write paths at once. The contract suite, the per-mutation gate tests, the differential test, the release controls of ADR-0048 and each product's own gate tests stand against it.
- Shadewright's `GitProvider` does not change and its controller code does not move. About half of its adapter stays in Shadewright.
- The shared `Repo` has 16 methods (9 on `Reader`, including two accessors, and 7 on `Writer`) against 26 on `GitProvider`. The rest is product rules or host-specific, by design.
- Armory gains a public API it must keep compatible from `v1`; until then `v0.x` minor releases may break it with a changelog entry.
- Armory's contributor rules need the `githost` scope in the pull request title check and in `AGENTS.md`, and an issue per public API change.

### If stage A ships alone

Shadewright keeps working on its own adapter. Sablecourt keeps a hardened opener and the stage A operations. What is lost is shared maintenance: a security fix needs two implementations, the stage B differential never runs, and an armory release does nothing for Shadewright. To keep that state safe:

- Sablecourt runs `githosttest.RunContract` and the transport and refusal fixtures against its use of the package before it ships push or merge.
- Shadewright keeps its own transport tests; nothing in its tree depends on armory.
- A fix found in one client is checked against the other by hand and recorded on both issue trackers.

## Alternatives considered

- Keep separate clients. No coordination cost. Sablecourt would build push, merge and check reads on its own client, which today follows redirects and decodes without a bound, and Azure DevOps would be written twice. Comes back if the second product stops using GitHub or Azure DevOps.
- Share only the transport. The pinned origin, bounds, retry and scrubbing are host neutral and the part most worth sharing. But the next mistakes are in the operations: compare-and-swap on refs, verifying a commit before publication, the merge head check, and reading checks bound to a commit. This record takes a middle way: it shares those operations and leaves Shadewright's rules, comments and feedback in Shadewright. Sharing less comes back if the core interface does not fit Azure DevOps after stage C's spikes.
- A third-party client library (for example `google/go-github` and Microsoft's Azure DevOps Go library). Large API coverage, maintained by others. The sampled versions accept a base URL, can return host text in errors and decode successful JSON without a product-chosen bound (`go-github` v88 has a redirect-suppressing client and bounded error reads, but an uncapped success decoder). Wrapping them to restore the rules for every call used, and re-reviewing each upgrade for the same properties, costs about as much as the calls themselves, and they bring dependencies past the module's dependency ceiling (ADR-0048 point 9). Comes back if the set of calls grows far past what this ADR lists.
- Put the package in Shadewright and let Sablecourt import it. Go refuses an import of another module's `internal/`; exporting a package would make the operator module a dependency of Sablecourt and turn Shadewright's internal adapter into a public API with its own compatibility promise (ADR-0048 rejects the same for redaction).
- Push the task branch with the `git` CLI and a token. Keeps the exact local commit, signatures and multi-commit branches, and an explicit lease (`--force-with-lease=<ref>:<expected>`) gives an expected-old condition the product can see. The product would still have to enforce fast-forward-only pushes itself, hand the token to a child process and its credential helper, and neutralise local Git configuration, which Sablecourt already works hard to do. Kept for the product to choose if signed or multi-commit pushes become a requirement.

## Security consequences

- No long-lived credential in armory. Tokens are asked for per request and dropped; the App key stays behind `Signer`. No exported function returns a token's value or writes it into a header; the transport installs it only on a request to the pinned origin.
- No authorization in armory. The product approves an `Operation`; the client sends only requests derived from it and asks the gate before each one. Armory adds structural refusals: no force, no delete, no bypass field on any request, `refs/heads/` only, unknown operations denied, uploads verified before they start.
- The caller can no longer pick the repository. The binding and the operation carry the pinned ID from trusted configuration; an agent's input cannot name another repository. Name-addressed writes keep the check-then-write race described under "Repository identity"; a repository-restricted credential closes it at the host.
- On GitHub the merge request enforces the head but has no expected-base field. The client reads the base right before the merge and after it; a retarget in between merges onto the wrong base, and the read after the merge reports it as `ErrUntrustedAnswer` but cannot undo it. A ruleset that limits who may change a pull request's base, and the product's own identity being the only writer of its pull requests, narrow this.
- Less attack surface in Sablecourt: no configurable API URL, no redirect, bounded reads, scrubbed errors.
- One supply chain for both write paths. A compromised armory release could push or merge in both products. ADR-0048's controls (pinned version, checksum database, release attestation, tag ruleset, reviewed bump with a source diff) apply to `githost`, and must be operational before the first product pins it.
- Azure DevOps: merge and commit publication return `ErrUnsupported` until the host is shown to enforce the head and the commit before state changes. Policy evaluations carry no commit and are a separate type; a product that treats one as evidence at a commit must bind it to an iteration it verified itself.
- Classic branch protection is invisible to the rules read on GitHub without admin rights; products name the checks they require and never infer "nothing required".

## Decisions on the earlier open questions

1. GitHub Enterprise Server is not supported now. Neither product's production wiring needs it, and accepting a host at runtime would widen the credential boundary.
2. Signed commits are refused before any upload in stage A. Sablecourt's own snapshot commits are unsigned, and a recreated commit without its signature has another ID.
3. Comments stay host-specific. Shadewright's marker depends on GitHub bot and App identity and on deletion-safe paging; Azure DevOps threads have a different ownership and paging contract.
4. Webhook verification stays in the receiving products, with secret lookup, body limits and dispatch.
5. Shadewright keeps its own `GitProvider` fake. It models chain, merge and marker decisions a host-protocol fake cannot.
6. One armory module and one tag series, with a separate import allow-list for `githost` and consumer compatibility tests in each product.
7. This stays ADR-0001. A later decision about record keeping does not renumber it.

## Open questions

1. Sablecourt's write credential for push and merge: a GitHub App installation token restricted to the repository, or a fine-grained token for one repository. The device-flow `repo` token is not acceptable for these writes.
2. Whether GitHub's `updateRefs` can create a ref by node ID, which would bind branch creation to the repository ID (**spike** in stage A).
3. Whether the merge base race on GitHub is accepted with the narrowing above, or Sablecourt also requires a ruleset that restricts retargeting before it enables merge.

## Stage A tasks

Each lands as its own pull request and is useful on its own. The issues exist in shadewright-labs/armory (epic 7); the changes each needs are listed, and two new issues are needed.

1. Issue 8, `feat(githost): core types, write operations, error kinds and Token` (title change): `ObjectID`, `RepoID`, `Binding`, `Ref`, the closed `Operation` set with `Digest`, `WriteAttempt`, `WriteGate`, the sentinels, `Token` without any header or value accessor, `Credential`, `SignatureAlgorithm` and `Signer`, with table tests, the canary print test and a golden test of the canonical digest encoding.
2. Issue 9, `feat(githost): Reader, Writer and the in-memory fake`: body change to the new method set (`PublishCommit`, `PublishHostCommit`, `MoveRef`, `ClosePullRequest`, `CommitChecks`, `RecordedHead`, cursors, `TreeLimit`); the fake checks operations against its state the same way.
3. Issue 10, `feat(githost): pinned-origin transport and the one send path` (title change): origin check before the token is installed, redirects refused, the one unexported write path with the gate last, the definition of an attempt, `GateClasses`, cursors rebuilt on the pinned origin, the conditional 304, `BaseDelay`, the GraphQL 200 rate limit, the one-hour cap, scrubbing of the whole error chain, the end-of-context rule, `Observer` without error values.
4. Issue 11, `test(githost): recorded-fixture server and contract suite`: body change: both live in the exported `githosttest` so another module can run them; add the per-mutation gate tests, the cross-origin token test and the canary on gate and signer error text.
5. Issue 12, `feat(githost/github): rate limit rules and branch reads`: body change for `TreeLimit` (byte and entry bounds set by the caller), duplicate and truncation refusal, and the pinned repository ID check on the first read.
6. Issue 13, `feat(githost/github): pull request reads`: body change for `RecordedHead` separate from the live head, `HeadRepo`, paged lists with cursors, any-base lists by head, strict rejection of foreign answers.
7. Issue 14, `feat(githost/github): MoveRef with compare-and-swap` (title change): `MoveRef` operation, move by node ID, create with the 422 read-after, and the node-ID create spike.
8. Issue 15, `feat(githost/github): PublishCommit verified before upload` (title change): local recomputation of blob, tree and commit IDs, the stage A limits (one unsigned commit with one parent, modes `100644`, `100755`, `040000`), comparison of the host's IDs before the ref moves, and `PublishHostCommit`.
9. Issue 16, `feat(githost/github): open, update and close pull requests`: body change for operation-bound requests, the base check before update and close, the owned-head-on-another-base refusal and `maintainer_can_modify: false`.
10. Issue 17, `feat(githost/github): commit checks at an immutable head` (title change): `CommitChecks` with origin on every entry, `All`, complete paging; drop `Blocking`, the rules read and the pull request head reads before and after.
11. Issue 18, `feat(githost/github): Merge at the approved head and base`: body change for the `Merge` operation, the base and state read before and after, `sha` in the request, `ErrUnsupported` without `AtomicMerge`, merge queue as `ErrForbidden`, `ErrOutcomeUnknown` without retry.
12. New issue: `feat(githost/github/app): App installation token restricted to the pinned repository`: offline config validation, token exchange on the shared transport, the grant check (repository and permissions), single-flight refresh, refresh margin.
13. New issue: `chore: release controls and contributor rules for githost`: the `githost` scope in the pull request title check and `AGENTS.md`, the `githost` import allow-list, the tag ruleset and release attestation, before any product pins a version.
14. Issue 19, `chore: first tagged release that includes githost`: body change: blocked by task 13, and named as Sablecourt's first pin.

Sablecourt's side is tracked in its own repository: build one commit before approval, approve over `githost.Digest`, push with a repository-restricted credential, record commit checks as evidence, and require the merge approval in the merge handler.

## Sources

Read on 2026-11-26.

Microsoft Learn, Azure DevOps Services REST API 7.1:

- [Pushes - Create](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pushes/create?view=azure-devops-rest-7.1): `refUpdates`, `commits[].changes`, `changeType`, `newContent` and `ItemContentType`, `GitChange` fields (no mode), response `treeId` and `commitId`.
- [Refs - Update Refs](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/refs/update-refs?view=azure-devops-rest-7.1): `GitRefUpdate`, `GitRefUpdateResult`, the 16 `GitRefUpdateStatus` values.
- [Pull Requests - Update](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-requests/update?view=azure-devops-rest-7.1): updatable properties, 4000 character description, `GitPullRequestCompletionOptions` (`mergeStrategy`, `bypassPolicy`, `deleteSourceBranch`, `autoCompleteIgnoreConfigIds`), `isDraft`, `mergeStatus`, `status`, `lastMergeCommit`, `lastMergeSourceCommit`. The page has no completion example and does not say that a completion checks any head.
- [Pull Requests - Get Pull Request](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-requests/get-pull-request?view=azure-devops-rest-7.1) and [Get Pull Requests](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-requests/get-pull-requests?view=azure-devops-rest-7.1): repository-scoped URL, `searchCriteria` parameters, no stated order.
- [Evaluations - List](https://learn.microsoft.com/en-us/rest/api/azure/devops/policy/evaluations/list?view=azure-devops-rest-7.1): artifact ID template, `PolicyEvaluationRecord`, `PolicyEvaluationStatus`, `isBlocking` on the configuration, version 7.1-preview.1.
- [Statuses - Create](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/statuses/create?view=azure-devops-rest-7.1) and [Statuses - List](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/statuses/list?view=azure-devops-rest-7.1): commit statuses, `GitStatusState`, `latestOnly`, `createdBy`.
- [Pull Request Statuses - Create](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-request-statuses/create?view=azure-devops-rest-7.1): `iterationId`.
- [Customize pull request workflows with status](https://learn.microsoft.com/en-us/azure/devops/repos/git/pull-request-status?view=azure-devops): iterations, latest status per context, reset on new changes.
- [Complete, abandon, or revert pull requests](https://learn.microsoft.com/en-us/azure/devops/repos/git/complete-pull-requests?view=azure-devops) and the `GitPullRequestMergeStrategy` reference: the four merge types, override needs the exempt permission.

Not read, so nothing above is stated from them: Items, Trees, Commits, Pull Request Threads, Policy Configurations, the rate limit page, the personal access token page, Azure DevOps Server.

GitHub REST API (pages show version 2022-11-28):

- [Pulls](https://docs.github.com/en/rest/pulls/pulls?apiVersion=2022-11-28): merge (`sha`, `merge_method`, 405, 409), create (`draft`), get (`mergeable` may be `null`, `merge_commit_sha`).
- [Check runs](https://docs.github.com/en/rest/checks/runs?apiVersion=2022-11-28): list for a ref, `filter`, `check_name`, `app_id`, statuses and conclusions.
- [Commit statuses](https://docs.github.com/en/rest/commits/statuses?apiVersion=2022-11-28): combined status, list in reverse chronological order.
- [Repository rules](https://docs.github.com/en/rest/repos/rules?apiVersion=2022-11-28): rules for a branch, rulesets only, `required_status_checks` with `context` and `integration_id`.
- [Git commits](https://docs.github.com/en/rest/git/commits?apiVersion=2022-11-28): `author` and `committer` with `date`, `parents`, `signature`.

Not read: the GitHub App installation token page and the GraphQL reference. Shadewright's `auth` code requests a token for one repository by name and checks the grant; asking by ID is a spike.

Shadewright, on `main`: `internal/providers/git.go`, `errors.go`, `signer.go`, `checkpoint.go`, `retryafter/`, `github/` (all files), `internal/controller/killswitch.go`, ADR-0006, ADR-0030, ADR-0032, ADR-0048. Sablecourt, on `main`: `internal/gate/mcp/`, `internal/runtime/coding/worktree.go`, `internal/providers/github_oauth.go`, `internal/webhook/github.go`, `cmd/sablecourtd/main.go`.
