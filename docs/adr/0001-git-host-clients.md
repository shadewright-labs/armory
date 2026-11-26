# ADR-0001: Package `githost`, a safe client for one Git host origin

- Status: Proposed
- Date: 2026-11-26

Parts of stage A are built on a branch (the transport, `Token`, the credential interfaces, `retryafter`); nothing is merged. Statements about GitHub cite Shadewright's adapter on `main` or the GitHub REST pages under "Sources". Statements about Azure DevOps cite the Microsoft Learn pages listed there, read on 2026-11-26. A claim marked **spike** must be confirmed against a test repository before code depends on it.

## Context

Shadewright and Sablecourt both call GitHub over HTTP.

**Shadewright** has a hardened adapter, `internal/providers/github`, of about 4,240 production lines: `provider.go` (1,690), `client.go` (549), `feedback.go` (519), `handoff.go` (341), `validate.go` (111), `ancestry.go` (73), `doc.go` (83), `auth` (779) and `ratelimit` (92), plus `internal/providers/retryafter` (43). It implements `GitProvider` (26 methods), `FeedbackSource` and `AncestryChecker`. Its transport pins `https://api.github.com`, refuses redirects, requires TLS 1.2, bounds every body (2 MiB for JSON by default, 64 KiB in `auth`, streamed feedback pages of up to 30 items of 400 KiB), retries with backoff and jitter (4 attempts and a 500 ms first step by default), waits a requested delay of up to 15 seconds in place and caps any delay at one hour. Its errors carry fixed text, a status and a class, never a URL, header, body or token. Its kill switch (`providers.CheckWrite`) runs immediately before each write attempt, after the token and body are ready. The App key stays in Key Vault behind a `Signer` ([ADR-0006](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0006-github-app-key-in-key-vault.md)). Its write authorization is a stale-plan gate in the Kubernetes controller.

**Sablecourt** has a `git.open_pr` action that posts to `/repos/{owner}/{repo}/pulls` with a token from its secret store. The shipped wiring uses `http.DefaultClient`, so it follows redirects and decodes a successful answer without a size bound; the API base URL is configurable. It also has an OAuth device flow (default scope `repo`) with its own client, and a webhook handler. It authorizes writes through out-of-band approval of a pinned plan. It wants to push task branches, read check evidence and merge.

A first version of this record put a shared write authorization model into the package: a closed set of operations, a digest over their canonical encoding, and a gate comparing that digest. Two rounds of design review each returned "rework", and the second found new high-severity cases of the same kind as the first: what an approval covers, how the digest is framed, inputs that change between verification and send, check-then-write races on pull requests, a broad token, stale check evidence. The two products authorize differently, and the shared model did not converge. This version shares only what both products need from a client and leaves authorization to each product.

## Decision

1. Package `githost` gives a product a safe client for one Git host origin and one repository. It is not an authorization layer. It holds the pinned-origin transport, the `Token` type, the `Credential` and `Signer` interfaces, a GitHub App installation-token source, error kinds, the exported `retryafter` package and a small set of typed GitHub calls.
2. Each typed call maps to one documented host request, takes every field the host acts on as an explicit argument, and returns what the host returned after validation. It makes no claim about approval. Each call's documentation says which preconditions the host enforces in the same request and which are check-then-write. A racy precondition is never presented as a guarantee.
3. The one private send path calls a product-supplied pre-send hook immediately before every attempt of every non-GET repository request, with the literal request facts. The hook can refuse. That is all the library promises about authorization.
4. The immutable repository ID is product configuration. The client checks it on reads and before writes, and documents that for name-addressed writes the check is a race that only a credential restricted to that repository closes.
5. What an approval binds, evidence freshness, push rules, comment markers, merge receipts and branch proofs belong to the products. Sablecourt writes its own decision record for push, check evidence and merge before that code is built.
6. Only the transport and credential parts are host-neutral now. Typed Azure DevOps calls need a spike and a later record.
7. The package uses only the Go standard library.

## Package layout

```text
githost/                     RepoRef, ObjectID, Token, Credential, Signer, PreSend, Observer, error kinds
githost/retryafter/          Retry-After and rate-limit reset parsing, exported for other HTTP clients
githost/github/              typed GitHub calls and the bounded read (Get)
githost/github/app/          GitHub App installation-token source, restricted to one repository
githost/githosttest/         recorded-fixture server and contract suite, exported for other modules
githost/internal/transport/  pinned-origin HTTP client, the one send path, bounds, retry, scrubbing
githost/internal/secret/     the only way a token's value leaves a Token; used by the transport alone
```

Production constructors take no URL. Only `githosttest` can point a client at a loopback fixture server.

## The kept surface

```go
package githost

// RepoRef is the one repository a client is bound to. Every field comes from the product's trusted
// configuration. ID is the host's immutable repository ID (GitHub numeric ID); it is never learned.
type RepoRef struct {
	Host        Host // GitHub only for now
	Owner, Name string
	ID          string
}

type ObjectID string // full lowercase hex object ID; Valid() refuses names and short IDs

// Token prints, formats, logs and marshals as a placeholder. It has no exported accessor and no
// method that writes it into a header.
type Token struct{ /* unexported */ }
func NewToken(s Scheme, value string, expires time.Time) Token
func (t Token) Expires() time.Time

type Scope struct {
	Repo  RepoRef
	Write bool
}
type Credential interface {
	Token(ctx context.Context, s Scope) (Token, error)
}
type CredentialFunc func(ctx context.Context, s Scope) (Token, error)

type SignatureAlgorithm string // named type; Shadewright aliases it
type Signer interface {
	Algorithm() SignatureAlgorithm
	Sign(ctx context.Context, digest []byte) ([]byte, error)
}

// Send is what a PreSend hook sees before one attempt of a non-GET repository request.
type Send struct {
	Repo     RepoRef
	Call     string // the typed call's fixed name, such as "github.Merge"
	Method   string
	Template string // path template, such as "/repos/{owner}/{repo}/pulls/{number}/merge"
	Args     any    // the typed call's argument struct, a copy of what is sent
	Attempt  int    // 1 for the first attempt, then each retry
}

// PreSend is called immediately before every attempt of every non-GET repository request. A non-nil
// error stops the call; nothing more is sent. A nil PreSend refuses every such request.
type PreSend func(ctx context.Context, s Send) error

// Observer gets one payload-free event per attempt: call, method, template, status, attempt, duration.
type Observer func(Event)
```

```go
package github

type Config struct {
	Repo        githost.RepoRef
	Credential  githost.Credential
	PreSend     githost.PreSend
	Observer    githost.Observer
	MaxAttempts int           // 0 means 4
	BaseDelay   time.Duration // 0 means 500 ms, as in Shadewright today
	MaxWait     time.Duration // 0 means 15 s
	Timeout     time.Duration // one call, retries included; 0 means 2 min
}

func New(cfg Config) (*Client, error)

// Reads.
func (c *Client) Ref(ctx context.Context, branch string) (githost.ObjectID, error)
func (c *Client) Commit(ctx context.Context, id githost.ObjectID) (Commit, error)
func (c *Client) Tree(ctx context.Context, id githost.ObjectID, lim TreeLimit) ([]TreeEntry, error)
func (c *Client) Blob(ctx context.Context, id githost.ObjectID, limit int64) ([]byte, error)
func (c *Client) Compare(ctx context.Context, base, head githost.ObjectID) (Comparison, error)
func (c *Client) PullRequest(ctx context.Context, number int64) (PullRequest, error)
func (c *Client) ListPullRequests(ctx context.Context, q PullRequestQuery) (PullRequestPage, error)
func (c *Client) CheckRuns(ctx context.Context, commit githost.ObjectID, q CheckRunQuery) (CheckRunPage, error)
func (c *Client) Statuses(ctx context.Context, commit githost.ObjectID, page Page) (StatusPage, error)
func (c *Client) Get(ctx context.Context, r ReadRequest, decode func(Response) error) error

// Writes. Each passes PreSend before every attempt.
func (c *Client) CreateBlob(ctx context.Context, content []byte) (githost.ObjectID, error)
func (c *Client) CreateTree(ctx context.Context, a CreateTreeArgs) (githost.ObjectID, error)
func (c *Client) CreateCommit(ctx context.Context, a CreateCommitArgs) (Commit, error)
func (c *Client) CreateRef(ctx context.Context, branch string, id githost.ObjectID) error
func (c *Client) MoveRef(ctx context.Context, branch string, old, new githost.ObjectID) error
func (c *Client) OpenPullRequest(ctx context.Context, a OpenPullRequestArgs) (PullRequest, error)
func (c *Client) UpdatePullRequest(ctx context.Context, number int64, a UpdatePullRequestArgs) (PullRequest, error)
func (c *Client) ClosePullRequest(ctx context.Context, number int64) (PullRequest, error)
func (c *Client) Merge(ctx context.Context, number int64, a MergeArgs) (MergeResult, error)
func (c *Client) CreateCheckRun(ctx context.Context, a CreateCheckRunArgs) (int64, error)
func (c *Client) PostComment(ctx context.Context, number int64, body string) (int64, error)
```

Result types keep what the host sent that products decide on: `TreeEntry` keeps type and mode; `Comparison` keeps `status`, `ahead_by` and `behind_by`; `PullRequest` keeps `RecordedHead` (the host's `head.sha`, which stays evidence after the branch moves) apart from the live head that `Ref` reads, plus head and base refs, head repository ID, state, merged flag, `merge_commit_sha`, a nullable `mergeable` and a nullable body; a check run keeps its `app.id`, a status its `creator.id`. `MergeArgs.SHA` is required. `UpdatePullRequestArgs` uses pointers, so nil means the field is not sent. `CreateCheckRunArgs` has the head SHA, name, status, conclusion and the whole output (title, summary, text). `CreateCommitArgs` has message, tree, parents and optional author and committer with dates; without them the host picks the identity and the commit ID cannot be predicted. On GitHub a `Page` is a validated page number the caller may set, so a product can walk pages backwards or resume from a stored one; the next request is rebuilt on the pinned origin, never from a `Link` URL.

`Get` is the read-only escape hatch for what stays in a product (comments, reviews, permission reads, rules): GET only, a path of escaped segments under the bound repository, query keys from a fixed allow-list, a body bound of at most 16 MiB, and `Response.NotModified` for a 304 when the request set `IfNoneMatch`. It carries no header from the caller and returns no header but `ETag`, `Date` and the page positions.

### Preconditions per write call

"Host" means the host refuses in the same request when the condition fails. "Racy" means any check the product makes beforehand can be overtaken before the write lands. Every write below except `MoveRef` is addressed by owner and name, so the repository binding is racy too (see "Repository identity").

| Call | Host request | Precondition and who enforces it | Sent again on retry |
| --- | --- | --- | --- |
| `CreateBlob`, `CreateTree` | `POST /git/blobs`, `POST /git/trees` | none needed: content-addressed. The blob ID is checked against a locally computed hash | yes |
| `CreateCommit` | `POST /git/commits` | none. Tree and parents are literal; without author and committer dates a retry can create a second, unreferenced commit | yes; the ID returned is the one from the answer that arrived |
| `CreateRef` | `POST /git/refs` | ref must not exist: host (422, returned as `ErrConflict`) | no; a lost answer is `ErrOutcomeUnknown` |
| `MoveRef` | GraphQL `updateRefs` by repository node ID, `beforeOid`, `afterOid`, `force: false` | expected old value: host. Bound to the repository ID through the node ID | yes; a refused retry is explained by one ref read (at `new` is success, elsewhere `ErrConflict`) |
| `OpenPullRequest` | `POST /pulls` with `maintainer_can_modify` as passed | head commit: racy, the host records whatever the branch points at. A second open pull request with the same head and base: refused by the host (**spike** to confirm the 422). One with the same head to another base: not refused | no |
| `UpdatePullRequest`, `ClosePullRequest` | `PATCH /pulls/{n}` | none. State and base checks made beforehand are racy; a concurrent retarget is not seen | no |
| `Merge` | `PUT /pulls/{n}/merge` with `sha` and `merge_method` | head equals `sha`: host (409 is `ErrConflict`). Base, state and check results: racy. An advance of the base ref changes the merge result and is not covered at all. A merge queue requirement refuses (405, `ErrForbidden`) | no |
| `CreateCheckRun` | `POST /check-runs` | none; the head SHA is literal. A newer run on the same commit can follow at any time | no |
| `PostComment` | `POST /issues/{n}/comments` | none. Uniqueness of a marker across concurrent calls is racy | no |

The library never sends a force flag, a delete, or a bypass field. A write whose answer is lost and that is not sent again returns `ErrOutcomeUnknown`. When the context ended first, the error matches both `ErrOutcomeUnknown` and the context error, so a caller sees the cancellation and still knows the write may have landed.

## The pre-send hook

The transport has one unexported send path. For every non-GET repository request it builds the request and its body once from a copy of the call's arguments taken at entry (byte slices included), gets the token, checks the URL against the pinned origin, installs the token, calls `PreSend` with that copy, and then calls `http.Client.Do`. Every attempt sends the same body bytes. A caller who changes its own slices after the call starts changes nothing that is sent or shown to the hook.

An attempt is one call to `Do`. `net/http` may resend on a reused connection when nothing of the request was written; that is the same attempt, because no byte reached the host. A refusal comes back as `ErrStoppedBeforeWrite` with the hook's text dropped. A product that needs its own refusal sentinels back lists them in `Config` and gets `ErrStoppedBeforeWrite` wrapping the matching one.

GET requests go through a second function whose type cannot carry another method. The App token exchange (`POST /app/installations/{id}/access_tokens`) is a third, private path to one fixed endpoint: it uses the same origin, redirect, timeout and bound rules, authenticates with the App JWT, changes no repository state and does not call the hook.

Tests prove the hook per mutation: for each write call, for each retry and for the read after a refused `MoveRef` retry, a test refuses at that attempt and asserts that the next request the fixture server sees is not a write. The suite also counts hook calls against write requests the server received.

## Repository identity

The client reads `GET /repos/{owner}/{name}` before the first write of each call and compares the numeric ID with `RepoRef.ID`; reads that carry a repository ID are compared too, and a mismatch is `ErrUntrustedAnswer`. The same read gives the node ID that `MoveRef` uses, so ref moves are bound to the repository by the host. Every other write is addressed by name: if the repository is renamed, transferred or recreated between the check and the write, the write lands in the replacement.

What closes that window is the credential. An App installation token requested for the pinned repository cannot write elsewhere. A fine-grained token for one repository has the same effect. The library requires nothing about the credential, but the App source it ships always asks for one repository. A broad token, such as the `repo` scope of Sablecourt's device flow, defeats the binding, and the package documentation says so.

## Transport rules

1. One origin, `https://api.github.com`, pinned in code. Paths are built from escaped segments. No user info or non-default port.
2. Redirects are refused; a 3xx is an error except a 304 answering a conditional `Get`. TLS 1.2 or later, dial, handshake and header timeouts, one attempt bounded at 60 seconds and one call at `Timeout`.
3. Bounded reads: 2 MiB of JSON unless the call sets another bound, 64 KiB for the token exchange, a declared `Content-Length` over the bound refused before reading. Lists are bounded by pages and items; past either is `ErrTooLarge`, never a short list.
4. Reads are retried on network errors, timeouts, 408, 429, 5xx and rate limits, with backoff from `BaseDelay` and jitter, up to `MaxAttempts`. Writes are retried only where the table above says so.
5. Rate limits: `Retry-After` (seconds or an HTTP date), then `X-RateLimit-Remaining: 0` with `X-RateLimit-Reset`, then one minute for a secondary limit or an unreadable 403, a GraphQL `RATE_LIMITED` error in a 200 answer, and a cap of one hour. A delay up to `MaxWait` is waited in place; a longer one is a `RetryableError` carrying it. Nothing waits past the context deadline.
6. No logging and no secrets in errors. The package imports no logger. Errors are built from fixed text, the call name, the status and a kind. Errors from `net/http`, the credential, the signer, the hook and JSON decoding are never wrapped as values; only their class is kept. A 403 body may be searched with a bound for a rate-limit marker and is then dropped. The `Observer` never receives a body, header, token or error value.
7. Answers are checked: repository ID, pull request number, head, base and commit are compared with what was asked; a foreign answer is `ErrUntrustedAnswer` and is not retried.
8. An answer that raced the end of the call's context is dropped (Shadewright issues 767 and 779).

## Error kinds

`ErrNotFound`, `ErrPathNotInTree` (wraps `ErrNotFound`), `ErrConflict`, `ErrForbidden`, `ErrUnauthorized`, `ErrRejected`, `ErrTooLarge`, `ErrInvalidEntry`, `ErrUntrustedAnswer`, `ErrUnsupported`, `ErrInvalidConfig`, `ErrStoppedBeforeWrite`, `ErrOutcomeUnknown` (a write was sent and its answer lost; never a pre-send refusal) and `RetryableError` (matches `ErrRetryable`, with `RetryAfter`). `context.Canceled` and `context.DeadlineExceeded` keep their identity. Products map these to their own sentinels.

## What moves out

### To Sablecourt

Sablecourt's push, check evidence and merge (its issues 133, 135 and 144) need their own decision record in Sablecourt, accepted before code. That record must answer what the design reviews of this one raised:

- Wrong-write approval: what exactly an approval names (repository ID, refs, pull request, base, expected IDs, the whole payload including check-run output and messages), so that an approval for one write cannot authorize another.
- Digest framing: if approvals are compared by digest, a versioned, length-delimited encoding with presence tags for optional fields and slices, and tests of pairs that must differ.
- Frozen inputs: the bytes approved are the bytes sent, read once from the trusted repository and never read again from a caller-owned source.
- Pull request races: the head can move before create, a retarget can happen before update, close or merge, and a second pull request to another base can appear after a lookup. Which of these Sablecourt accepts, narrows (rulesets, being the only writer) or refuses.
- Base-ref advance: whether review covers the source commit alone or a particular integration result, given that `sha` pins the head only.
- Stale check evidence: a newer failure on the same SHA, how the latest result per producer is chosen, which required checks the host itself enforces at merge, and how an uncertain check-run POST is reconciled.
- Uncertain outcome after cancellation: how a write that may have landed is detected and reconciled before any further write, for merge by number, recorded head and actual base.
- Credential choice: an App installation token or a fine-grained token restricted to the repository for every write, the opener included; the device-flow `repo` token for reads only.
- The single-commit push rule: Sablecourt's task end can leave zero, one or two snapshot commits on top of the harness's work, so a one-commit branch has to be built before approval, and signed or multi-commit branches refused or handled another way.

Sablecourt also keeps its OAuth device flow and client, webhook verification and its local Git code.

### To Shadewright (kept there)

- `GitProvider`, `FeedbackSource`, `AncestryChecker` and its fake.
- The stale-plan gate, kill switch and record hold, called from `PreSend`.
- Branch rules and proofs: the `shadewright/` prefix, target ancestry before creation, the opposite condition for append, single-parent and allowed-path proofs, tree comparisons with its own byte bound, `ListDir` caching and symlink and submodule filtering.
- Pull request rules: listing the head across bases before create and refusing an owned head open to another base, ignoring forks on that lookup, the update no-op, closed and merged checks, duplicate detection, fork-as-not-found by number, the finalizer's any-base close, `EnableAutoMerge` answered as unsupported.
- Merge receipts (ADR-0030) on `RecordedHead`.
- `ReviewCheckRun` selection (its App and check name, greatest run ID, neutral and skipped as success).
- Comment markers and feedback paging on `Get`, and the login-addressed permission read.
- Its own `retryafter` copy for the Prometheus, Foundry, Vertex and Azure Blob clients until they move to `githost/retryafter`.

## Azure DevOps

Only the transport and credential parts are host-neutral now. Typed Azure DevOps calls are a later record, after a spike against a test organisation. Facts from the pages read, as input for it:

- Ref updates take `oldObjectId` and `newObjectId`; the page says to "specify both the old and new commit to avoid race conditions", and `staleOldObjectId` is a refusal status.
- `POST .../pushes` creates the commit and moves the ref in one request, so commit and tree IDs are known only after the ref moved. `GitChange` has no file mode.
- The pull request Update page does not say a completion checks any head. Merge head enforcement is unproven.
- Policy evaluations are pull request scoped and carry no commit; statuses can be posted on an iteration. Evidence at a commit has to be bound to an iteration the product verified.
- A pull request description is limited to 4000 characters. Whether `lastMergeSourceCommit` can serve as a recorded head is open.
- Entra tokens are not scoped to one repository, so the name-addressed race applies to every write.

## Stages

**Stage A.** The transport, `Token`, the credential interfaces and `retryafter` are built on a branch; before merge they take this record's names (`PreSend` with `Repo`, `Call` and `Args` replaces the provisional write check, and `RepoRef.ID` becomes configuration). Then the App source, then the typed GitHub calls with the recorded-fixture server and the contract suite, both exported so another module can run them. Sablecourt adopts the opener first: its `git.open_pr` loses the configurable URL, redirect following and unbounded reads. Push and merge follow only after Sablecourt's own record. The release controls of Shadewright's [ADR-0048](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0048-redaction-in-a-shared-module.md) (tag ruleset, attestation, checksum verification on bumps, the `githost` import allow-list) are in place before Sablecourt's first pin. None of them is operational for armory today.

**Stage B.** Shadewright moves its transport, App authentication and literal requests onto the package and keeps its adapter logic. An estimate from file sizes on `main`, not a count of a built change: about 1,420 lines go away outright (`client.go` 549, `auth` 779, `ratelimit` 92); about 700 lines of request code in `provider.go`, `handoff.go` and `validate.go` become calls into the package and shrink rather than vanish; about 2,100 stay (the rest of `provider.go`, `feedback.go`, most of `handoff.go`, `ancestry.go`, `doc.go`). Tests that move with the code: `auth/source_test.go` (1,155, its fake signer replaced by a test signer), `ratelimit_test.go` (96) and the transport parts of `provider_test.go` (2,655) and `testdata` (43 fixtures). Only those ported transport tests are claimed to pass unchanged. Known differences to list and test in the stage B pull request:

- `BaseDelay` defaults to 500 ms in both; any other default is a change.
- The installation token is requested for the repository by ID; Shadewright asks by name today (**spike**). The App source checks that the grant names exactly the pinned repository; checking the granted permissions is new, as Shadewright's `auth` today requests named permissions but does not read them back.
- Every write call now reads the repository ID first.
- `CreateCheckRun` is sent once and a lost answer is `ErrOutcomeUnknown`; today it goes through the retrying sender.
- `CreateRef` returns `ErrConflict` on 422; today's adapter reads the branch after a 422, and that read moves into the adapter.
- Today's commit request sends `message`, `tree` and `parents` and no author, committer or dates; the adapter keeps passing exactly that.
- The `User-Agent` and some error text differ; the adapter's tests compare error class and sentinel.

A differential test of old against new compares results and refusal classes, and asserts each difference above explicitly; it does not compare raw request sequences, which differ by design.

### If stage A ships alone

Shadewright keeps its own adapter and transport tests, and nothing in its tree depends on armory. Sablecourt keeps a hardened opener and runs the contract suite against its use of the package before shipping more. What is lost is shared maintenance: a transport fix needs two implementations and is checked against the other client by hand, recorded on both trackers.

## Consequences

- One hardened transport for both products, and a token type that cannot be read or put into a header outside the transport.
- No authorization in armory. Each product builds its own on `PreSend`, and each write call says what the host enforces and what it does not.
- A fix reaches a product only when that product bumps its pin; an urgent fix waits for both bumps.
- A bug in the transport hits both products' write paths. The contract suite, the per-mutation hook tests, the release controls of ADR-0048 and each product's own tests stand against it.
- Armory gains a public API it keeps compatible from `v1`; `v0.x` minor releases may break it with a changelog entry. The contributor rules need a `githost` scope in the pull request title check and in `AGENTS.md`.

## Security consequences

- No long-lived credential in armory. The App key stays behind `Signer`. Armory never reads a file, environment variable or secret store. Tokens are asked for per request; only the transport installs one, on a request to the pinned origin.
- Workloads never receive a `Credential`, `Token` or `*github.Client`. In Sablecourt the client lives in the gate process; in Shadewright only the executor holds the Git App.
- Name-addressed writes are racy against a repository replacement unless the credential is restricted to one repository.
- `sha` on a merge pins the head only. Base, state and check results are racy and are the product's to narrow.
- One supply chain for both write paths: a compromised release could write in both products, so ADR-0048's controls apply before the first pin.

## Alternatives considered

- Keep separate clients. No coordination, but Sablecourt's client follows redirects and decodes without a bound today, and every hardening fix would be written twice.
- A shared write authorization model in the library. Tried in the first version of this record; two design reviews found new high-severity gaps each time, because the products authorize differently. Comes back only if a third product shares one product's model.
- A third-party client such as `google/go-github` v88. The sampled version accepts a base URL, can return host text in errors and decodes successful JSON without a caller-chosen bound, although it has a redirect-suppressing client and bounded error reads. Restoring the rules for each call used and re-checking each upgrade costs about as much as the calls, and it brings dependencies past ADR-0048's ceiling.
- Put the package in Shadewright. Go refuses imports of another module's `internal/`, and exporting it would make the operator module a dependency of Sablecourt.
- Push with the `git` CLI and a token. An explicit lease (`--force-with-lease=<ref>:<expected>`) gives an expected-old condition, but the product must then enforce fast-forward-only pushes, hand the token to a child process and neutralise local Git configuration. Left for Sablecourt's own record.

## Recorded decisions

1. GitHub Enterprise Server is not supported now. Neither product needs it, and a runtime host would widen the credential boundary.
2. Signed commits are out: `CreateCommit` takes no signature, and a recreated commit without its signature has another ID.
3. Comments stay host-specific. Shadewright's marker depends on GitHub bot and App identity and deletion-safe paging.
4. Webhook verification stays in the receiving products, with secret lookup, body limits and dispatch.
5. Shadewright keeps its `GitProvider` fake; it models decisions a protocol fake cannot.
6. One armory module and one tag series, with a `githost` import allow-list separate from the redaction packages' and consumer tests in each product.
7. This is ADR-0001. A later decision about record keeping does not renumber it.

## Open questions

1. Can GraphQL `updateRefs` create a ref by node ID, which would bind branch creation to the repository ID? (**spike**, stage A)
2. Does the installation-token request accept `repository_ids`, and does the answer list granted permissions to check? (**spike**, stage A; the token page was not read)

## Tasks

The issues are in shadewright-labs/armory under epic 7.

| Issue | Disposition | Title and body |
| --- | --- | --- |
| 7 | retitle | `Epic: githost, a safe client for one Git host origin`. Tracks ADR-0001: the pinned-origin transport, `Token`, credentials, the App source, `retryafter` and typed GitHub calls, with a pre-send hook as the only authorization point. Approval models stay in the products; Sablecourt's push, evidence and merge need its own record. |
| 8 | retitle | `feat(githost): core types, error kinds, Token and credentials`. `RepoRef` with a configured ID, `ObjectID`, the error kinds, `Token` with no exported accessor and placeholder printing, `Credential`, `CredentialFunc`, `SignatureAlgorithm`, `Signer`, `PreSend` and `Send`, with table tests and a canary print test. |
| 9 | close | Out of scope: there is no host-neutral `Reader` or `Writer` and no in-memory fake. Products that need a fake define their own interface; the fixture server is the shared test double. |
| 10 | retitle | `feat(githost): pinned-origin transport and the pre-send hook`. One origin, redirects refused, conditional 304, bounded reads, retry with backoff, `Retry-After` and rate limits, cursors rebuilt on the origin, the whole error chain scrubbed, an `Observer` with no secrets, one send path calling `PreSend` before each attempt with a frozen copy of the arguments, and the definition of an attempt. |
| 11 | keep | `test(githost): recorded-fixture server and contract suite`. Both in the exported `githosttest`. The server injects 3xx, 304, oversize and slow bodies, rate limits, 5xx, lost answers and foreign answers. The suite covers every typed call, the per-mutation hook tests, the cross-origin token test and the canary on hook and signer error text. |
| 12 | retitle | `feat(githost/github): ref, commit, tree, blob and compare reads`. Literal reads with the repository ID check, tree type and mode kept, duplicate and truncation refusal, blob hash check, `Comparison` with status and counts. |
| 13 | keep | `feat(githost/github): pull request reads`. `PullRequest` with `RecordedHead` apart from the live head, head repository, nullable body and mergeable; paged lists by head and optional base with numeric pages; foreign answers refused. |
| 14 | retitle | `feat(githost/github): CreateRef and MoveRef`. `POST /git/refs` sent once with 422 as `ErrConflict`; `updateRefs` by node ID with `beforeOid` and no force, retried with the explained refusal; the node-ID create spike. |
| 15 | retitle | `feat(githost/github): create blobs, trees and commits`. Literal calls with message, tree, parents and optional author and committer; the blob ID checked locally; no signature field. |
| 16 | retitle | `feat(githost/github): open, update and close pull requests, and post a comment`. Literal calls sent once, nil fields not sent, each documented as check-then-write with no host precondition; the duplicate-pull-request spike. |
| 17 | retitle | `feat(githost/github): check runs and statuses of a commit, and create a check run`. Reads with name, App and filter arguments, origin on every entry, complete paging; create with the whole output, sent once. |
| 18 | retitle | `feat(githost/github): merge with an expected head sha`. `PUT .../merge` with a required `sha`, 409 as `ErrConflict`, 405 and merge queue as `ErrForbidden`, a lost answer `ErrOutcomeUnknown` without retry; base and evidence documented as racy. |
| 19 | keep | `chore: first tagged release that includes githost`. Blocked by 22; Sablecourt's first pin, for the opener. |
| 21 | keep | `feat(githost/github/app): App installation token restricted to the pinned repository`. A private fixed-endpoint exchange with the transport's origin, redirect and bound rules and no hook; offline config validation, the grant check, single-flight refresh, refresh margin. |
| 22 | keep | `chore: release controls and contributor rules for githost`. The `githost` scope in the title check and `AGENTS.md`, the import allow-list, the tag ruleset and attestation, before any product pins a version. |

## Sources

Read on 2026-11-26.

GitHub REST API (version 2022-11-28): [Pulls](https://docs.github.com/en/rest/pulls/pulls?apiVersion=2022-11-28) (merge with `sha` and `merge_method`, 405, 409; create with `draft`; `mergeable` may be `null`), [Check runs](https://docs.github.com/en/rest/checks/runs?apiVersion=2022-11-28), [Commit statuses](https://docs.github.com/en/rest/commits/statuses?apiVersion=2022-11-28), [Git commits](https://docs.github.com/en/rest/git/commits?apiVersion=2022-11-28). Not read: the installation token page and the GraphQL reference.

Azure DevOps REST API 7.1: [Pushes - Create](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pushes/create?view=azure-devops-rest-7.1), [Refs - Update Refs](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/refs/update-refs?view=azure-devops-rest-7.1), [Pull Requests - Update](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-requests/update?view=azure-devops-rest-7.1), [Evaluations - List](https://learn.microsoft.com/en-us/rest/api/azure/devops/policy/evaluations/list?view=azure-devops-rest-7.1), [Pull Request Statuses - Create](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-request-statuses/create?view=azure-devops-rest-7.1).

Shadewright, on `main`: `internal/providers/git.go`, `checkpoint.go`, `signer.go`, `retryafter/`, `github/` (all files), ADR-0006, ADR-0030, ADR-0032, ADR-0048. Sablecourt, on `main`: `internal/gate/mcp/`, `internal/runtime/coding/worktree.go`, `internal/providers/github_oauth.go`, `cmd/sablecourtd/main.go`.
