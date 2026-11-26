# ADR-0001: Git host clients move into armory as package `githost`

- Status: Proposed
- Date: 2026-11-26

Nothing in this ADR is built. Statements about Azure DevOps cite the Microsoft Learn pages listed under "Sources", read on 2026-11-26. Statements about GitHub cite either the Shadewright adapter as it is on `main` today or the GitHub REST pages listed under "Sources". Anything the pages do not say is marked **spike**: a stage must confirm it against a test repository before the code that depends on it is built.

## Context

Two products of the organisation talk to Git hosts over HTTP.

**Shadewright** has a hardened GitHub adapter, `internal/providers/github`, of about 4,240 production lines: `provider.go` (1,690), `client.go` (549), `feedback.go` (519), `handoff.go` (341), `validate.go` (111), `ancestry.go` (73), `doc.go` (83), the `auth` subpackage (779: App JWT, installation token cache, repository URL rules, a `Token` type that never prints) and `ratelimit` (92), plus `internal/providers/retryafter` (43). It implements `GitProvider` (24 methods in `internal/providers/git.go`), `FeedbackSource` and `AncestryChecker`. Its rules:

- one repository and one target branch per instance, fixed at construction; writes only to `shadewright/` branches;
- one host, `https://api.github.com`, pinned in code; no redirects; TLS 1.2 or later; every body read with a bound (2 MiB for JSON, 64 KiB in `auth`);
- commits built with the Git Data API (blob, tree, commit, all content addressed) and the branch moved with a compare-and-swap on the expected head, never forced;
- error statuses classed from status code and headers, never from the body (one exception: a 403 body is searched but never repeated for a secondary rate limit); errors carry fixed text, the status code and a class, never a URL, header, body, repository, branch or token;
- retries with backoff and jitter, at most 4 attempts; a delay GitHub asks for is waited in place up to 15 seconds, and a longer one is returned as a `RetryableError` so the caller requeues;
- a write checkpoint (the kill switch, `providers.CheckWrite`) called right before every write request and every retry of one;
- the App private key never enters the process: the JWT digest is signed through a `Signer` interface backed by Key Vault ([ADR-0006](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0006-github-app-key-in-key-vault.md)).

Shadewright's [ADR-0032](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0032-azure-devops-as-a-second-git-provider.md) (Proposed) maps every `GitProvider` method to Azure DevOps and picks a federated Entra identity with no stored key.

**Sablecourt** (the second product) has three small pieces: a `git.open_pr` action that posts to `/repos/{owner}/{repo}/pulls` with a token from its secret store (owner and repository come from the action input, the API base URL is configurable, the client is `http.DefaultClient`, so redirects are followed and the body is read without a bound); a GitHub OAuth device flow; and a webhook handler that checks `X-Hub-Signature-256`. A local `git` CLI wrapper makes the task commit in a worktree. It now needs two things it does not have: pushing a finished task branch (one local commit on a branch under its own prefix) to the host with the approval pinned to the commit, and reading the checks of a pull request it opened and merging it on approval at the reviewed commit, never bypassing branch rules.

Without a shared package Sablecourt would write a third, weaker GitHub client and later a second Azure DevOps client. Armory already takes shared security code under the rules of Shadewright's [ADR-0048](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0048-redaction-in-a-shared-module.md) (tags on a protected `main`, checksum database, attestation check on bumps, a dependency ceiling, no product dependency).

## Decision

1. Armory gets a package `githost`: a host-neutral core interface for one repository, a GitHub implementation now and an Azure DevOps implementation later, each with a host-specific surface for what has no counterpart on the other host.
2. Armory holds no credential and makes no authorization decision. A product passes a credential source and a write gate; armory calls the gate before each write request and sends nothing when it refuses. Which repository, which branch, which approval and which audit record belong to the product.
3. The HTTP rules of the Shadewright adapter become the rules of the package, for every host and every caller.
4. Migration runs in three stages. Stage A builds the package with GitHub and Sablecourt adopts it for the push and merge work. Stage B moves Shadewright onto it behind its existing `GitProvider`, with no change of behaviour, proven by a differential test. Stage C adds Azure DevOps for both.
5. The package uses only the Go standard library. Azure identity libraries stay in the products, behind the credential interface.

### Package layout

```text
githost/                  core types, Reader, Writer, Repo, Credential, WriteGate, errors, Token
githost/github/           GitHub client: implements githost.Repo plus GitHub-only methods
githost/github/app/       GitHub App credential: JWT signed through githost.Signer, installation token cache
githost/azuredevops/      Azure DevOps client (stage C): implements githost.Repo plus Azure DevOps-only methods
githost/githosttest/      in-memory fake of githost.Repo for product unit tests; contract suite entry point
githost/internal/transport/  pinned-host HTTP client, bounded reads, retry, Retry-After, error scrubbing
githost/internal/hostfake/   recorded-fixture HTTP server used by armory's own tests only
```

`internal/hostfake` is the only way armory's tests point a client at a non-production URL. Production constructors take no base URL.

### The exported core

```go
// Package githost is a client for one repository on a Git host. It holds no credential and decides
// nothing about what a caller may do: the product supplies a Credential and a WriteGate.
package githost

import (
	"context"
	"net/http"
	"time"
)

// Host names a Git host.
type Host string

const (
	GitHub      Host = "github"      // https://api.github.com only
	AzureDevOps Host = "azuredevops" // https://dev.azure.com only
)

// ObjectID is a full Git object ID: 40 or 64 lowercase hexadecimal characters.
type ObjectID string

// Valid reports whether id is a full object ID. Branch names and short IDs are never accepted.
func (id ObjectID) Valid() bool

// RepoRef names one repository. A client is bound to exactly one at construction.
type RepoRef struct {
	Host    Host
	Owner   string // GitHub owner, or Azure DevOps organization
	Project string // Azure DevOps project; empty on GitHub
	Name    string // repository name
	// ID is the host's immutable repository ID (GitHub numeric ID, Azure DevOps GUID), read once and
	// compared with every answer that carries one. An answer for another repository is ErrUntrustedAnswer.
	ID string
}

// Capabilities says what the bound host supports, so callers branch on facts, not on Host.
type Capabilities struct {
	DraftPullRequests bool
	MergeStrategies   []MergeStrategy
	// MergeChecksHead is true when the host refuses, in the merge request itself, a merge whose head is no
	// longer MergeRequest.ExpectedHead. False means the client checks the head before and after only.
	MergeChecksHead bool
	// CommitIdentity is true when CreateCommit honours author, committer and their dates, so an unsigned
	// local commit can be reproduced with the same ID.
	CommitIdentity bool
}

// TreeEntry is one entry of a directory at a commit.
type TreeEntry struct {
	Name string // one path segment, never a slash
	Mode Mode   // ModeFile, ModeExecutable, ModeDir, ModeSymlink, ModeSubmodule
	ID   ObjectID
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

// File is the content of one regular file at one commit, checked against its blob ID.
type File struct {
	Path   string
	Commit ObjectID
	Mode   Mode
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

// PullRequest is a pull request of the bound repository.
type PullRequest struct {
	Number int64
	URL    string // web URL, for people; never fetched by the client
	Head   string // source branch name, without refs/heads/
	Base   string // target branch name, without refs/heads/
	// HeadID is the head of the source branch read from the branch itself, not a cached field of the
	// pull request, which can lag on both hosts.
	HeadID ObjectID
	State  PullRequestState
	Draft  bool
	Title  string
	Body   string
	// MergeCommit is set only when State is Merged and the host reports a full commit ID; empty means
	// not known yet, and the caller decides how long to wait.
	MergeCommit ObjectID
	// Mergeable is the host's own view: MergeableYes, MergeableConflict, MergeableBlocked or
	// MergeableUnknown (still computing, or not reported). It never decides anything by itself.
	Mergeable Mergeability
	// FromFork is true when the source branch is in another repository. Such a pull request is answered,
	// never written to.
	FromFork bool
}

// Mergeability is the host's report on whether a pull request can merge now.
type Mergeability string

const (
	MergeableUnknown  Mergeability = "Unknown"
	MergeableYes      Mergeability = "Yes"
	MergeableConflict Mergeability = "Conflict"
	MergeableBlocked  Mergeability = "Blocked" // rules or policies not met
)

// PullRequestQuery selects pull requests of the bound repository. Head is required.
type PullRequestQuery struct {
	Head     string
	Base     string // empty means any base
	OpenOnly bool
}

// MaxListedPullRequests bounds ListPullRequests; more is ErrTooLarge, never a list cut short.
const MaxListedPullRequests = 100

// CheckState is the state of one check, status or policy at a commit.
type CheckState string

const (
	CheckPending       CheckState = "Pending"
	CheckPassed        CheckState = "Passed"   // GitHub success; Azure DevOps approved or succeeded
	CheckNeutral       CheckState = "Neutral"  // GitHub neutral or skipped
	CheckFailed        CheckState = "Failed"   // any failing conclusion, error, rejected or broken
	CheckNotApplicable CheckState = "NotApplicable"
)

// CheckSource says what kind of host object a check is.
type CheckSource string

const (
	SourceCheckRun     CheckSource = "CheckRun"     // GitHub only
	SourceCommitStatus CheckSource = "CommitStatus" // both hosts
	SourcePolicy       CheckSource = "Policy"       // Azure DevOps branch policy evaluation
	SourcePRStatus     CheckSource = "PRStatus"     // Azure DevOps pull request status
)

// Check is one check as the host reports it. Name is host text: callers compare it with names from their
// own configuration and never display or log it unredacted.
type Check struct {
	Source CheckSource
	Name   string
	// Origin identifies who produced it where the host says so: a GitHub App ID, an Azure DevOps
	// identity ID or policy type ID. Empty when the host does not say.
	Origin string
	State  CheckState
	// Blocking is true when the host says this check blocks completion (an Azure DevOps blocking policy,
	// a GitHub required status check rule). False means optional or not reported.
	Blocking bool
}

// CheckReport is every check the client read for one pull request at one head.
type CheckReport struct {
	Number int64
	Head   ObjectID // the head the report is for; equal to the head asked for, or the call fails
	Checks []Check
	ReadAt time.Time
}

// MaxChecks bounds a CheckReport; more is ErrTooLarge.
const MaxChecks = 300

// Reader reads one repository. A Reader built with a read-only credential holds no write method.
type Reader interface {
	Repository() RepoRef
	Capabilities() Capabilities

	// BranchHead returns the commit at the head of a branch. ErrNotFound when the branch does not exist.
	BranchHead(ctx context.Context, branch string) (ObjectID, error)

	// Commit returns a commit's tree and parents. ErrNotFound when it does not exist.
	Commit(ctx context.Context, id ObjectID) (Commit, error)

	// ReadTree lists one directory at a commit, without recursion, sorted by name. dir is "." or a clean
	// relative path. More than MaxTreeEntries entries is ErrTooLarge; an entry no tree can hold is
	// ErrInvalidEntry; a path that is not a directory in the tree is ErrPathNotInTree.
	ReadTree(ctx context.Context, commit ObjectID, dir string) ([]TreeEntry, error)

	// ReadFile returns a regular file at a commit, at most limit bytes (clamped to MaxFileBytes), with the
	// size checked before the content is downloaded and the content checked against its blob ID. A
	// symbolic link, submodule or directory is ErrPathNotInTree; it is never followed.
	ReadFile(ctx context.Context, commit ObjectID, path string, limit int64) (File, error)

	// PullRequest returns the pull request with number, in any state. ErrNotFound when there is none in
	// this repository; an answer for another number or repository is ErrUntrustedAnswer.
	PullRequest(ctx context.Context, number int64) (PullRequest, error)

	// ListPullRequests returns the pull requests matching q, newest first. An entry the query did not ask
	// for (another head, base or repository) is ErrUntrustedAnswer for the whole call.
	ListPullRequests(ctx context.Context, q PullRequestQuery) ([]PullRequest, error)

	// Checks reads every check, status and policy evaluation that applies to pull request number at
	// head. ErrConflict when the pull request's head is not head before or after the read, so a report is
	// never for a commit other than the one asked for. It returns what the host says; whether that is
	// enough to merge is the caller's decision.
	Checks(ctx context.Context, number int64, head ObjectID) (CheckReport, error)
}

// RefUpdate creates or moves one branch. Old empty means create; New must be a full object ID. There is
// no force and no delete.
type RefUpdate struct {
	Branch string   // name without refs/heads/; validated as a Git ref name
	Old    ObjectID // expected current head; empty for a branch that must not exist yet
	New    ObjectID
}

// FileChange sets or deletes one file in a new commit.
type FileChange struct {
	Path    string
	Mode    Mode   // ModeFile or ModeExecutable; ignored for Delete
	Content []byte // nil with Delete
	Delete  bool
}

// Signature is an author or committer of a new commit.
type Signature struct {
	Name  string
	Email string
	When  time.Time
}

// NewCommit is one commit built from file changes on top of Parent, and the move of Branch to it.
type NewCommit struct {
	Branch  string
	Parent  ObjectID // must be the branch head; for a new branch, the commit to start it from
	Create  bool     // true when Branch must not exist yet
	Message string
	Changes []FileChange // at most MaxChanges, at most MaxCommitBytes of content in all
	// Author and Committer are used where Capabilities.CommitIdentity is true; otherwise the host
	// sets them and a non-zero value is ErrUnsupported.
	Author, Committer Signature
	// ExpectTree, when set, is the tree the commit must have. The client compares it with the host's tree
	// before the branch moves and returns ErrConflict without moving it on a mismatch.
	ExpectTree ObjectID
	// ExpectCommit, when set, is the commit ID the new commit must have (an unsigned local commit
	// reproduced with the same identity). Checked before the branch moves, like ExpectTree.
	ExpectCommit ObjectID
}

// NewPullRequest opens a pull request in the bound repository, from Head to Base.
type NewPullRequest struct {
	Head, Base  string
	Title, Body string
	Draft       bool // ErrUnsupported where Capabilities.DraftPullRequests is false
}

// PullRequestUpdate changes an open pull request. Nil fields are left as they are.
type PullRequestUpdate struct {
	Title *string
	Body  *string
	Close bool // close without merging (Azure DevOps: abandon)
}

// MergeStrategy is how a pull request lands on its base.
type MergeStrategy string

const (
	MergeCommit MergeStrategy = "Merge"  // two-parent merge; Azure DevOps noFastForward
	Squash      MergeStrategy = "Squash" // one single-parent commit
	Rebase      MergeStrategy = "Rebase" // commits replayed on the base, base fast-forwarded
)

// MergeRequest merges one pull request at one head. There is no field that bypasses a rule or a
// policy, on any host, and the client never sends one.
type MergeRequest struct {
	Number       int64
	ExpectedHead ObjectID
	Strategy     MergeStrategy
	Title        string // merge commit title, optional
	Message      string // merge commit message, optional
}

// MergeResult is what the host reported for a merge.
type MergeResult struct {
	Merged      bool
	MergeCommit ObjectID // empty when the host reports none yet; read PullRequest later
}

// Writer writes to one repository. Every method calls the client's WriteGate right before each write
// request and each retry of one, and sends nothing more in the call when the gate refuses.
type Writer interface {
	// UpdateRef creates or moves a branch with a compare-and-swap on u.Old. ErrConflict when the branch
	// head is not u.Old (or exists, for a create). Creating a branch that already points at u.New succeeds.
	UpdateRef(ctx context.Context, u RefUpdate) error

	// CreateCommit makes one commit from c.Changes on c.Parent and moves c.Branch to it with a
	// compare-and-swap on c.Parent, and returns it. ErrConflict when the head moved or an Expect field
	// does not match. A retry after a lost answer finds the branch at the new commit and succeeds.
	CreateCommit(ctx context.Context, c NewCommit) (Commit, error)

	// OpenPullRequest opens a pull request, or returns the open one from the same head to the same base
	// unchanged. It lists before it creates; see the mapping table for the race on each host.
	OpenPullRequest(ctx context.Context, p NewPullRequest) (PullRequest, error)

	// UpdatePullRequest changes title or body, or closes, an open pull request. Closing a closed one
	// succeeds; a merged one is ErrConflict.
	UpdatePullRequest(ctx context.Context, number int64, u PullRequestUpdate) (PullRequest, error)

	// Merge merges the pull request at m.ExpectedHead. ErrConflict when the head is not m.ExpectedHead;
	// ErrForbidden when a rule or policy of the host refuses the merge. A merge whose answer was lost is
	// ErrOutcomeUnknown and is never repeated in the same call: the caller reads PullRequest first.
	Merge(ctx context.Context, m MergeRequest) (MergeResult, error)
}

// Repo is a Reader and a Writer for one repository.
type Repo interface {
	Reader
	Writer
}

// Bounds, the same on every host.
const (
	MaxFileBytes   = 1 << 20  // largest file ReadFile returns, and largest file in a FileChange
	MaxTreeEntries = 1000
	MaxChanges     = 200      // files in one NewCommit
	MaxCommitBytes = 16 << 20 // content of one NewCommit in all
)
```

Credential and write gate:

```go
// Scheme is how a Token is sent.
type Scheme int

const (
	Bearer Scheme = iota + 1 // GitHub tokens, Azure DevOps Entra tokens
	Basic                    // Azure DevOps personal access token (spike, stage C)
)

// Token is a short-lived secret for one host. It prints, formats, logs and marshals as a placeholder.
// Its value is never returned as a string: SetHeader writes it into a request header.
type Token struct{ /* unexported: scheme, value behind a pointer, expiry */ }

// NewToken wraps value. The product builds tokens; armory never stores one beyond its expiry.
func NewToken(s Scheme, value string, expires time.Time) Token

// Expires returns when the token stops being valid; zero when the source did not say.
func (t Token) Expires() time.Time

// SetHeader sets the Authorization header of h. It is the only way the value leaves the Token.
func (t Token) SetHeader(h http.Header)

// String, GoString, Format, LogValue, MarshalJSON and MarshalText return a placeholder.

// Scope is what a token is asked for: one repository, and whether the call writes.
type Scope struct {
	Repo  RepoRef
	Write bool
}

// Credential returns a token for one request. The client asks for a token before each request and
// does not keep it; caching and refresh are the credential's business. Errors keep only their class
// (canceled, deadline, retryable, not found, forbidden, unauthorized) when the client wraps them.
type Credential interface {
	Token(ctx context.Context, s Scope) (Token, error)
}

// CredentialFunc adapts a function to Credential.
type CredentialFunc func(ctx context.Context, s Scope) (Token, error)

// Signer signs a digest with a key that never leaves its key store (GitHub App JWT, RS256).
type Signer interface {
	Algorithm() string // "RS256"
	Sign(ctx context.Context, digest []byte) ([]byte, error)
}

// WriteOp is the fixed set of write requests a client sends.
type WriteOp string

const (
	OpCreateObjects     WriteOp = "CreateObjects" // GitHub blob, tree, commit; content addressed
	OpCreateRef         WriteOp = "CreateRef"
	OpMoveRef           WriteOp = "MoveRef"
	OpPush              WriteOp = "Push" // Azure DevOps commit and ref move in one request
	OpOpenPullRequest   WriteOp = "OpenPullRequest"
	OpUpdatePullRequest WriteOp = "UpdatePullRequest"
	OpClosePullRequest  WriteOp = "ClosePullRequest"
	OpMerge             WriteOp = "Merge"
	OpHostSpecific      WriteOp = "HostSpecific" // a write of a host-specific method; Detail names it
)

// WriteIntent describes the write request about to be sent. Every field comes from the caller's
// arguments or from values the client read and checked, never from free host text.
type WriteIntent struct {
	Op      WriteOp
	Detail  string // fixed label of a host-specific write, for example "github.CreateCheckRun"
	Repo    RepoRef
	Branch  string
	Number  int64
	Old     ObjectID // expected head, when the write has one
	New     ObjectID // resulting commit, when known before the send
	Attempt int      // 1 for the first send, then each retry
}

// WriteGate is the product's last check before a write request leaves the process. Returning an error
// stops the call: nothing more is sent, and the error comes back wrapped in ErrStoppedBeforeWrite with
// its text dropped. The gate must be quick and must not call the client.
type WriteGate func(ctx context.Context, w WriteIntent) error

// Observer receives one payload-free event per HTTP attempt (operation label, status class, attempt,
// duration, whether a delay was asked for). Nil means none. It cannot change what the client does.
type Observer func(Event)
```

Constructors live in the host packages and take everything at once:

```go
package github

// Config binds a Client to one repository.
type Config struct {
	Owner, Name string            // the one repository; validated as GitHub names
	Credential  githost.Credential // required
	Gate        githost.WriteGate // required for New; NewReader takes none
	Observer    githost.Observer
	MaxAttempts int               // 0 means 4
	MaxWait     time.Duration     // longest delay waited in place; 0 means 15s
	Timeout     time.Duration     // one method call, retries included; 0 means 2m
}

func New(cfg Config) (*Client, error)        // *Client implements githost.Repo
func NewReader(cfg Config) (*Reader, error)  // *Reader implements githost.Reader only

// GitHub-only methods on *Client (stage B needs them; each write passes the gate with OpHostSpecific):
func (c *Client) CreateCheckRun(ctx context.Context, head githost.ObjectID, run CheckRun) error
func (c *Client) CheckRuns(ctx context.Context, head githost.ObjectID, name string, appID int64) ([]CheckRunInfo, error)
func (c *Client) Compare(ctx context.Context, base, head githost.ObjectID) (Comparison, error) // ahead, behind, status
func (c *Client) IssueComments(ctx context.Context, number int64, page Page) (CommentPage, error)
func (c *Client) PostIssueComment(ctx context.Context, number int64, body string) (int64, error) // never retried in the call
func (c *Client) Reviews(ctx context.Context, number int64, page Page) (ReviewPage, error)
func (c *Client) Permission(ctx context.Context, userID int64) (Permission, error)
func (c *Client) BranchRules(ctx context.Context, branch string) ([]Rule, error)
```

```go
package app

// Config of a GitHub App credential for one installation and one repository.
type Config struct {
	ClientID       string
	InstallationID int64
	Owner, Name    string
	Signer         githost.Signer // the private key stays in the key store
	Permissions    map[string]string // requested installation permissions; the product's choice
}

func New(cfg Config) (*Source, error) // *Source implements githost.Credential and caches the installation token
func (s *Source) AppID(ctx context.Context) (int64, error)
```

Azure DevOps (stage C) adds `azuredevops.New(Config{Organization, Project, Repository, ...})` and host-only methods: `PostPullRequestStatus` (on an iteration), `Iterations`, `Threads`, `PostThread`, `PolicyEvaluations`, `CommitStatuses`. The Graph membership read of ADR-0032 needs a second host (`vssps.dev.azure.com`) and is not part of the package until a product needs it.

### What is deliberately host-specific

GitHub check runs (created by an App, bound to a commit), issue comments and reviews with their paging cursors, permission reads, compare, and rulesets. Azure DevOps pull request statuses on iterations, threads, votes, policy evaluation records, and Graph memberships. These differ in shape (ADR-0032 section 3 has the details for Shadewright's use), and a neutral wrapper over them would hide the differences that the products' security rules depend on, for example that an Azure DevOps vote carries no commit.

### Authentication

The client asks the `Credential` for a token before each request, sets it with `Token.SetHeader`, and drops it. Armory provides one credential implementation, `github/app`, because its logic is pure protocol (JWT claims, installation token exchange, refresh five minutes before expiry, one refresh shared by concurrent callers) and the key stays behind `Signer`. Everything else is a `CredentialFunc` in the product:

| Credential | Who holds it | How it reaches armory |
| --- | --- | --- |
| GitHub App installation token | Key in a key store; product supplies the `Signer` (Shadewright: Key Vault) | `app.New(...)` |
| GitHub user OAuth token (device flow) | Product's secret store; device flow stays in the product | `CredentialFunc` reading the store per call |
| Azure DevOps Entra token | Product's workload identity library | `CredentialFunc` wrapping it, `Bearer` |
| Azure DevOps personal access token | Product's secret store | `CredentialFunc`, `Basic` (**spike**: the PAT page was not read; ADR-0032 does not support PATs for Shadewright) |

Armory never reads a file, an environment variable or a secret store, and never writes a token anywhere. `Scope.Write` lets a product hand out a narrower token for reads.

### What stays in each product

- Policy: which repository a client is built for, which branches a caller may write (`shadewright/` for Shadewright, Sablecourt's own prefix for it), which base branches are targets, which checks must pass and from which origin. Armory validates only structure: a ref name Git accepts, `refs/heads/` only, full object IDs.
- Approvals: Sablecourt's approval pinned to a commit, and Shadewright's kill switch, both enforced through `WriteGate`, which sees the exact `Old` and `New` IDs before the send.
- Audit: products record what they asked and what came back. Armory has an `Observer` with no payload and logs nothing.
- Shadewright's own rules: `CheckBranch`, `CheckChain`, `CheckMerged`, `CheckCommit`, `CheckBetween`, marker comments, feedback cursors and the merge receipt logic of ADR-0030 stay in Shadewright and are rebuilt on `Reader` and the GitHub-only methods.
- Local Git: Sablecourt's worktree and CLI code stays where it is.
- Webhooks: signature checks stay in the products for now (open question 4).

### HTTP transport rules

These hold for every host and are tested in the contract suite.

1. Fixed hosts. GitHub: `https://api.github.com`. Azure DevOps: `https://dev.azure.com`. Pinned in code; no constructor takes a URL. Paths are built from escaped segments only.
2. No redirects. `CheckRedirect` returns `http.ErrUseLastResponse`; a 3xx is an error. TLS 1.2 or later; dial, handshake and header timeouts; one attempt is bounded at 60 seconds and one method call at `Timeout`.
3. Bounded reads. A JSON answer is read up to 2 MiB unless the call sets a lower bound, a declared `Content-Length` over the bound is refused before reading, and an answer over it is `ErrTooLarge`. File content is bounded by `limit+1`. Lists are bounded by a page count and an item count; past either is `ErrTooLarge`, never a short list.
4. Retries. Reads are retried on network errors, timeouts, 408, 429, 5xx and rate limits, with exponential backoff and jitter, up to `MaxAttempts`. A write is retried only when it is idempotent by construction: content-addressed objects, a ref move with an expected old value, a pull request create preceded by a list. A merge, a comment post and an Azure DevOps push whose answer was lost are not repeated in the call; they return `ErrOutcomeUnknown` and the caller reads state first.
5. Rate limits. GitHub: `Retry-After`, then `X-RateLimit-Remaining: 0` with `X-RateLimit-Reset`, then one minute for a secondary limit (the current `ratelimit` package). Azure DevOps: `Retry-After` (**spike**: the rate limit page was not read). A delay up to `MaxWait` is waited in place; a longer one returns a `RetryableError` carrying it. Nothing waits past the context deadline.
6. No logging. The package imports no logger. Bodies, headers and tokens never reach the `Observer`.
7. Error text. Errors carry an operation label, the status code and a class. Never a URL, query, header, body, repository, branch, pull request title or token. Transport errors from `net/http` are replaced, since they hold the URL. A 403 body may be searched with a bound for a rate limit marker, never kept.
8. Answers are checked. Repository ID, pull request number, head and base are compared with what was asked; a mismatch is `ErrUntrustedAnswer` and is not retried.
9. A context that ended first wins. An answer that raced the end of the call's context is dropped (Shadewright issues #767 and #779).

### Error kinds

All are sentinels for `errors.Is`, in package `githost`:

| Kind | Meaning | Retry |
| --- | --- | --- |
| `ErrNotFound` | object does not exist | no |
| `ErrPathNotInTree` (wraps `ErrNotFound`) | tree read, path is not a regular file or directory there | no, stable for that commit |
| `ErrConflict` | stale state: head moved, branch exists elsewhere, merged when closing, Expect mismatch | caller re-reads |
| `ErrForbidden` | identity lacks a permission, or a rule or policy refused | no |
| `ErrUnauthorized` | credential refused (401) | no |
| `ErrRejected` | host refused the request as invalid and state does not explain it | no |
| `ErrTooLarge` | answer, file, list or commit past a bound | no |
| `ErrInvalidEntry` | tree entry no tree can hold | no |
| `ErrUntrustedAnswer` | host answered about another object | no |
| `ErrUnsupported` | capability missing on this host | no |
| `ErrInvalidConfig` | constructor input unusable | no |
| `ErrStoppedBeforeWrite` | the product's `WriteGate` refused; nothing more sent | product decides |
| `ErrOutcomeUnknown` | a write was sent and its answer lost | read state, then decide |
| `RetryableError` (matches `ErrRetryable`) | transient; `RetryAfter` when the host asked | yes, by caller |

Shadewright maps these to its own sentinels in its wrapper; the names match except for the three new ones (`ErrUnsupported`, `ErrStoppedBeforeWrite`, `ErrOutcomeUnknown`), which map to its existing refusal handling.

### Mapping: GitHub and Azure DevOps

| Core method | GitHub | Azure DevOps | Gap |
| --- | --- | --- | --- |
| `BranchHead` | `GET /git/ref/heads/{b}` (today's adapter) | `GET .../refs?filter=heads/{b}`, starts-with match, take the exact name (ADR-0032) | none |
| `Commit`, `ReadTree`, `ReadFile` | Git Data API trees and blobs, blob hash checked (today's adapter) | Commits, Trees and Items APIs | **spike** (ADR-0032 slice 1): size before content, how links and submodules show |
| `UpdateRef` | create `POST /git/refs`; move with GraphQL `updateRefs` compare-and-swap (today's adapter) | `POST .../refs` with `GitRefUpdate[]` (`name`, `oldObjectId`, `newObjectId`); the page says to "specify both the old and new commit to avoid race conditions"; result `updateStatus` per ref: `staleOldObjectId` maps to `ErrConflict`, `forcePushRequired`, `createBranchPermissionRequired`, `writePermissionRequired`, `rejectedByPolicy` to `ErrForbidden` | create of an existing branch at the same commit: both read first |
| `CreateCommit` | blobs, tree with `base_tree`, `POST /git/commits` with `parents`, `author` and `committer` each with `date`, then the ref move; tree and commit IDs known before the ref moves, so `ExpectTree` and `ExpectCommit` are checked with nothing visible changed | one `POST .../pushes` with `refUpdates[{name, oldObjectId}]` and one commit whose `changes` are `add`, `edit` or `delete` with `newContent` (`rawText` or `base64Encoded`); the response gives `treeId` and `commitId` | Azure DevOps: the push moves the ref in the same request, so an Expect mismatch is found after the branch moved, and the client then moves it back with a ref update from the new ID to the old one and returns `ErrConflict`; `GitChange` has no file mode field, so `ModeExecutable` is **spike**; whether `author`, `committer` and dates in the request are honoured is not on the page, so `CommitIdentity` is false until a **spike** shows otherwise |
| `OpenPullRequest` | list `GET /pulls?head=&base=&state=open`, then `POST /pulls` with `draft` (today's adapter, plus `draft`) | list `GET .../pullrequests?searchCriteria.sourceRefName=&searchCriteria.targetRefName=&searchCriteria.status=active`, then create; `isDraft` exists on `GitPullRequest` | GitHub refuses a second open pull request of the same head and base (today's adapter reads the 422); whether Azure DevOps refuses a second active one is **spike** (ADR-0032); the list page states no order, so "newest" is the highest ID |
| `PullRequest`, `ListPullRequests` | `GET /pulls/{n}`, `GET /pulls`; `mergeable` may be `null` while GitHub computes it, so `MergeableUnknown` | `GET .../pullrequests/{id}` (repository scoped); `status` `active`, `abandoned`, `completed`; `mergeStatus` `queued`, `conflicts`, `succeeded`, `rejectedByPolicy`, `failure` | `HeadID` is read from the source branch on Azure DevOps, since `lastMergeSourceCommit` is "the commit at the head of the source branch at the time of the last pull request merge" and can lag |
| `UpdatePullRequest` | `PATCH /pulls/{n}` (today's adapter) | `PATCH .../pullrequests/{id}`; updatable: status, title, description "up to 4000 characters", completion options, merge options, auto-complete set by, target ref | Azure DevOps body limit 4000 characters is `ErrRejected` before the send |
| `Checks` | check runs `GET /commits/{sha}/check-runs?filter=latest` (100 a page) and statuses `GET /commits/{sha}/statuses` (reverse chronological, latest per context kept); `Blocking` from `GET /rules/branches/{base}` `required_status_checks` with `context` and optional `integration_id` | policy evaluations `GET .../policy/evaluations?artifactId=vstfs:///CodeReview/CodeReviewId/{projectId}/{prId}` (7.1-preview.1) with `status` `queued`, `running`, `approved`, `rejected`, `notApplicable`, `broken` and `configuration.isBlocking`; commit statuses `GET .../commits/{id}/statuses?latestOnly=true` | GitHub checks belong to a commit; Azure DevOps policy evaluations belong to a pull request and carry no commit. The client reads the head, then the evaluations, then the head again, and returns `ErrConflict` if it moved. The rules endpoint covers rulesets only, not classic branch protection, which needs admin rights; a repository protected only by classic protection reports `Blocking` false, so products name the required checks in their own policy and never infer "nothing required" |
| `Merge` | `PUT /pulls/{n}/merge` with `sha` ("SHA that pull request head must match to allow merge"), `merge_method` `merge`, `squash` or `rebase`; 409 when the head did not match, 405 when the merge cannot be performed | `PATCH .../pullrequests/{id}` with `status: completed` and `completionOptions.mergeStrategy` `noFastForward`, `squash`, `rebase` or `rebaseMerge`; `bypassPolicy` exists and is never sent | GitHub: `MergeChecksHead` true. Azure DevOps: the Update page does not say a completion checks any head, so `MergeChecksHead` is false: the client sends `lastMergeSourceCommit` set to the expected head and reads the head before and after; whether the host refuses on a mismatch is **spike**. If it does not, a push between the read and the completion merges an unreviewed head unless the product's policy (votes and status reset on push) blocks it. `rebaseMerge` has no GitHub counterpart; GitHub's merge endpoint "does not support ... merging with a merge queue" |
| Draft | `draft` on create | `isDraft` | none for open; publishing a draft is host-specific |
| Merge commit | `merge_commit_sha` after merge is the merge, squash or rebased commit | `lastMergeCommit`, "empty" while "in progress or ... unsuccessful" | filled only when Merged and a full ID (ADR-0032 section 6) |

### Testing

- Recorded-fixture fake server per host (`internal/hostfake`). Fixtures are JSON answers recorded from a test repository on each host, then scrubbed (IDs, names, URLs replaced with synthetic ones) and checked by armory's hygiene checks. The server matches method, path, query and body; an unexpected request fails the test. It can inject 3xx, oversize bodies, slow bodies, 403 rate limits, 5xx, lost answers after a write and answers for another object.
- Contract suite (`githosttest.RunContract`). One set of scenarios both implementations must pass through their fake servers: create and move a branch with the right and a stale old value; create a branch that exists at the same commit; `CreateCommit` with matching and mismatched `ExpectTree`; `OpenPullRequest` twice returns one; `Merge` with a stale head is `ErrConflict` and with a lost answer `ErrOutcomeUnknown`; `Checks` when the head moves mid-read is `ErrConflict`; the gate is called before every write and each retry and its refusal stops the call; a redirect is never followed; every bound gives `ErrTooLarge`; a canary token, URL and body string never appear in any error text, `%v`, `%#v` or `slog` output; no request body carries `bypassPolicy`.
- The same suite runs against the in-memory fake in `githosttest`, so a product that tests against the fake gets the same semantics as production.
- Fuzzing for every decoder of host answers, and for ref name and path validation.
- Live smoke tests behind a build tag against the test repositories, run by hand before a release, never in pull request CI.
- Import allow-list test: production packages import only the standard library and each other.

### Migration

**Stage A: the package, with GitHub, used by Sablecourt.**

Armory gets `go.mod` (if the redaction work has not created it yet), `githost`, `internal/transport`, `github` and `githosttest`. The transport, retry, rate limit and error code are ported from Shadewright's `client.go`, `ratelimit` and `retryafter` by copy, not by import; Shadewright is unchanged. Sablecourt replaces its `GitHubPROpener` and builds `git.push` and `git.merge_pr` on it:

- push: reads the local commit's parent, tree and changed files from its worktree, calls `CreateCommit` with `Create`, the local author and committer, `ExpectTree` and `ExpectCommit` set to the local IDs. Its approval is pinned to the local commit ID, and its `WriteGate` refuses any intent whose `New` is not that ID. On GitHub, an unsigned local commit with the same tree, parent, message, author and committer reproduces the same ID; a signed one does not, and is `ErrConflict` before the branch moves.
- merge: reads `Checks` at the reviewed head into its evidence store, decides from its own list of required checks and its out-of-band approval, and calls `Merge` with that head.

What could go wrong: the API surface is fixed before the second host proves it (mitigated by the Azure DevOps column above and by the `v0.x` tags, which allow breaking changes with notice); the copied transport drifts from Shadewright's before stage B (mitigated by the differential test of stage B, which compares requests); Sablecourt's user token has wider rights than an App installation token, so a gate bug pushes with a person's rights (the gate test and the branch prefix rule in the product hold that); a push of a merge commit or many local commits does not fit `CreateCommit` (Sablecourt refuses anything but one commit on the base, as its worktree code makes today).

**Stage B: Shadewright adopts the package with no behaviour change.**

Shadewright's `internal/providers/github` becomes a wrapper: it implements `GitProvider`, `FeedbackSource` and `AncestryChecker` as today, keeps its rules, and calls `github.Client` for HTTP. What moves, by file, on `main` today:

| Shadewright file | Lines | Where it goes |
| --- | --- | --- |
| `github/client.go` | 549 | `internal/transport` and `github` (most of it already copied in stage A) |
| `github/auth/` (`jwt.go` 98, `source.go` 515, `token.go` 62, `repo.go` 72, `doc.go` 32) | 779 | `github/app`, `githost.Token`, `githost.Signer` |
| `github/ratelimit/ratelimit.go` | 92 | `github` (unexported) |
| `retryafter/retryafter.go` | 43 | `internal/transport` |
| `github/provider.go`, the parts that read refs, trees and blobs, build commits, move refs, and read, open, list and close pull requests | about 750 of 1,690 | `github` core methods |
| `github/handoff.go`, `CreateCheckRun` and `ReviewCheckRun` reads | about 130 of 341 | `github` host-specific methods |
| `github/feedback.go`, the HTTP of comments, reviews and permission reads | about 200 of 519 | `github` host-specific methods |
| `github/ancestry.go` | 73 | `github.Compare` |
| `github/validate.go`, object ID, path and ref helpers | about 60 of 111 | `githost` |

About 2,700 production lines move and about 1,500 stay in Shadewright (its branch rules, tree comparisons, marker logic, feedback cursors). Tests: `auth/source_test.go` (1,155), `ratelimit_test.go` (96), `retryafter_test.go` (53), about 1,200 of `provider_test.go` (2,655) and the 40 fixtures in `testdata` move; the rest stays as wrapper tests. `internal/providers/fake/git.go` (1,095) stays: it fakes `GitProvider`, not the HTTP client. `providers.Signer` becomes an alias of `githost.Signer`, so the Key Vault signer does not change.

The differential test: for every fixture-driven test of the current adapter, run the old adapter and the wrapper against the same recorded server and compare, per call, the sequence of requests (method, path, query, body, headers except `User-Agent`), the result, and the error class and text. Any difference fails. It runs on the stage B pull request and on each armory bump while the old adapter is still in the tree; the old adapter is removed one Shadewright release later.

What could go wrong: the kill switch is now called through `WriteGate` instead of from the context, so a missed write path would send without the check (the contract test counts gate calls against write requests, and the differential test compares the request sequence); the error text changes and breaks a Shadewright test or a status message that relies on it (the differential test compares text); the Key Vault signer is behind a new interface (alias, no new code); the `User-Agent` changes from `shadewright`; ADR-0048's governance (attestation check, tag ruleset, pinned version, bump pull requests with a tag-to-tag diff) must be in place before Shadewright imports armory, so stage B waits for the redaction stage B checklist; the Kubernetes-free module adds no dependency, but Shadewright's executor now trusts a second module for its only Git write path.

**Stage C: Azure DevOps for both products.**

`azuredevops` implements `githost.Repo` and the host-specific methods ADR-0032 needs, behind the same contract suite, after ADR-0032's read-only slice answers its spikes and the spikes marked here (push identity and file mode, merge head check, second active pull request, PAT scheme, rate limit headers). Shadewright's `internal/providers/azuredevops` becomes a wrapper as in stage B.

What could go wrong: a spike fails and a core method cannot keep its contract on Azure DevOps (then `Capabilities` reports it and the method returns `ErrUnsupported`; the contract suite has a capability-gated branch, never a weaker assertion); policy evaluations are a preview API (`7.1-preview.1`) and can change; the Entra token is not scoped to a repository (ADR-0032 consequences), so the `WriteGate` and the bound repository are the only limits inside the process; Azure DevOps Server is out of scope.

## Consequences

- One HTTP client with the hardening Shadewright has, for both products. Sablecourt's current client loses its configurable URL, redirect following and unbounded reads.
- A fix to the transport or the rate limit rules is written once, and reaches both products through a release and two bump pull requests; an urgent fix waits for both.
- A bug in the package hits both products' write paths at once. The contract suite, the differential test, the attestation check of ADR-0048 and each product's own gate tests stand against that.
- Shadewright's `GitProvider` does not change and its controller code does not move.
- The core interface is smaller than `GitProvider` (12 methods against 24). The rest is either Shadewright's own rules or host-specific, by design.
- Armory gains a public API it must keep compatible from `v1`; until then `v0.x` minor releases may break it with a changelog entry.

## Alternatives considered

- Keep separate clients. No coordination cost. Sablecourt would write push, merge and check reads from scratch on a client that today follows redirects and reads without bounds, and Azure DevOps would be written twice. Comes back if the second product stops using GitHub or Azure DevOps.
- Share only the transport. The pinned host, bounds, retry and scrubbing are the part most worth sharing and are host neutral. But the next mistakes are in the operations: compare-and-swap on refs, the merge head check, reading checks at the right head, and Azure DevOps policy evaluations that carry no commit. Sharing only the transport leaves those to be re-derived in each product. Comes back if the core interface does not fit the second host after stage C's spikes.
- A third-party client library (for example `google/go-github` and Microsoft's Azure DevOps Go library). Large API coverage, maintained by others. They follow redirects or leave it to the caller, put response text in errors, read bodies without a product-chosen bound, accept a base URL, and pull in dependencies the module's dependency ceiling (ADR-0048 point 9) does not allow. Wrapping them to restore the rules is about as much code as the calls themselves, and every upgrade must be re-reviewed for the same properties. Comes back if the set of calls grows far past what this ADR lists.
- Put the package in Shadewright and let Sablecourt import it. Go refuses an import of another module's `internal/`, and exporting it brings the operator's dependencies along (the same reason ADR-0048 rejects it).
- Push the task branch with the `git` CLI and a token. Keeps the exact local commit, signatures and multi-commit branches. It hands the token to a child process and its credential helper, depends on the local Git config Sablecourt already works hard to neutralise, and gives no compare-and-swap the product can see before the send. Kept for the product to choose if signed commits become a requirement.

## Security consequences

- No credential in armory. Tokens are asked for per request and dropped; the App key stays behind `Signer`. A token cannot be printed, logged or marshalled through any `githost` type.
- No authorization in armory. The bound repository, the write gate and the product's branch rules decide. Armory adds structural refusals only: no force, no delete, no bypass field on any request type, `refs/heads/` only.
- The caller can no longer pick the repository. Sablecourt's `git.open_pr` takes owner and repository from the action payload today; with a client bound at construction from gate configuration, an agent's input cannot name another repository.
- Less attack surface in Sablecourt: no configurable API URL, no redirect, bounded reads, scrubbed errors.
- One supply chain for both write paths. A compromised armory release could push or merge in both products. ADR-0048's controls (pinned version, checksum database, release attestation, tag ruleset, reviewed bump with a source diff) apply to `githost` as they do to `redact`.
- Merge on Azure DevOps may not check the head in the request (spike). Until the spike passes, `MergeChecksHead` is false and a product that requires the check refuses to merge on that host.
- Policy evaluations carry no commit. A product that reads them as "checks at the reviewed commit" relies on the head being read before and after, and on status and vote reset policies on the branch; ADR-0032 section 5 lists what Shadewright requires in place.
- Classic branch protection is invisible to the rules read on GitHub without admin rights, so `Blocking` false never means "not required".

## Open questions

1. GitHub Enterprise Server. The pinned host excludes it. Does either product need it? If so, a second constructor with a validated `https` host and the same rules, decided before `v1`.
2. Signed commits. Does Sablecourt need to push signed local commits? `CreateCommit` cannot reproduce a signed commit ID, so that path would be the `git` CLI alternative.
3. Comments in the core. Shadewright posts marker comments and Azure DevOps has threads. Move a neutral `Comment` into the core in stage C, or keep it host-specific?
4. Webhook signatures. Sablecourt's HMAC check is small and correct. Move it as `github.VerifyWebhook` (and an Azure DevOps service hook counterpart), or keep receiving events in the products?
5. Fake ownership. Should Shadewright's `fake/git.go` be rebuilt on `githosttest`, or stay as the `GitProvider` fake it is?
6. Version and governance. Does `githost` ship in the same module and tags as `redact`, so one attestation check covers both, or as a separate module with its own tags?
7. ADR numbering. Armory has no ADR directory yet. Is this its ADR-0001, or does a record-keeping ADR come first?

## Stage A tasks

Each lands as its own pull request and is useful on its own.

1. `chore: create go.mod, the githost package and its CI gates`: module file (if absent), package doc, import allow-list test, `go test -race` in CI.
2. `feat(githost): core types, error kinds and Token`: `ObjectID`, `RepoRef`, the sentinels, `Token` with placeholder printing and `SetHeader`, `Credential`, `Signer`, `WriteGate`, with table tests and the canary print test.
3. `feat(githost): Reader, Writer and the in-memory fake`: the interfaces and `githosttest.Fake`, no HTTP, so products can code against them.
4. `feat(githost): pinned-host transport`: fixed host, no redirects, bounded reads, retry with backoff, `Retry-After`, error scrubbing, the end-of-context rule, `Observer`; ported from Shadewright's client with its tests.
5. `test(githost): recorded-fixture server and contract suite skeleton`: `internal/hostfake` and `githosttest.RunContract` running against the in-memory fake.
6. `feat(githost/github): rate limit rules and branch reads`: `ratelimit` port, `BranchHead`, `Commit`, `ReadTree`, `ReadFile`, with fixtures.
7. `feat(githost/github): pull request reads`: `PullRequest` and `ListPullRequests` with answer checks and `Mergeable`.
8. `feat(githost/github): UpdateRef with compare-and-swap`: create and move, the gate called before each send, no force.
9. `feat(githost/github): CreateCommit with Expect checks`: blobs, tree, commit with author and committer, tree and commit ID checks before the ref moves.
10. `feat(githost/github): OpenPullRequest and UpdatePullRequest`: list-then-create, drafts, close.
11. `feat(githost/github): Checks at a head`: check runs, statuses, branch rules, head read before and after.
12. `feat(githost/github): Merge at the expected head`: `sha` and `merge_method`, `ErrOutcomeUnknown` on a lost answer, no retry.
13. `chore: first v0.1.0 release of githost`: under ADR-0048's tag ruleset and release workflow.
14. In Sablecourt: replace the pull request opener with `githost/github` bound from gate configuration.
15. In Sablecourt: `git.push` for the task branch, approval pinned to the local commit ID through the write gate.
16. In Sablecourt: record `Checks` of an opened pull request as timestamped evidence.
17. In Sablecourt: `git.merge_pr` requiring its listed checks passed at the reviewed head and an out-of-band approval.

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

Shadewright, on `main`: `internal/providers/git.go`, `errors.go`, `signer.go`, `retryafter/`, `github/` (all files), ADR-0006, ADR-0030, ADR-0032, ADR-0048.
