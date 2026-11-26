# ADR-0001: Package `githost`, transport and credentials for one Git host origin

- Status: Proposed
- Date: 2026-11-26

This record decides what is built on branch `feat/githost-core` of armory and was not contested in three design reviews: the pinned-origin transport, the `Token` type, the credential and signer interfaces, the error kinds and the `retryafter` package. Each rule below names the test that checks it when the rule is about security. Typed GitHub calls, the final shape of the write check and the App installation-token source are not decided here; "Not decided here" lists where each one is.

## Context

Shadewright and Sablecourt both call GitHub over HTTP with their own code. Shadewright has a hardened adapter, `internal/providers/github`: its transport pins `https://api.github.com`, refuses redirects, bounds bodies, retries with backoff and keeps secrets out of errors, and its kill switch runs before each write attempt. Sablecourt's `git.open_pr` action posts to `/repos/{owner}/{repo}/pulls` through `http.DefaultClient`, so it follows redirects and decodes a successful answer without a size bound. Its API base URL is configurable.

Both products need the same things from a client: one origin, no redirects, bounded reads, retries that do not resend a write by accident, a token that cannot leak through printing or tracing, and errors with no secrets in them. Writing and fixing that twice is the cost this package removes.

Earlier drafts of this record also shared a write authorization model: a closed set of operations, a digest over their encoding and a gate comparing it. The products authorize differently (Shadewright with a stale-plan gate in its controller, Sablecourt with approval of a pinned plan), and three design reviews of those larger drafts each found new high-severity cases of the same kind. The shared model did not converge, so authorization stays in each product.

## Decision

The package has five built parts. Each rule is a short sentence a test can check. Test files are under `githost/`.

### 1. Pinned-origin transport (`internal/transport`)

The transport is internal: no exported constructor exists yet, and a product will reach it through the typed calls that are still to come.

- A client is built for one host. `New` refuses a host with a scheme, path, user, query, backslash or space, a missing credential, and a fixed `Authorization` header (`TestNewRefusesConfig`). A port is accepted; nothing restricts it to loopback.
- `Do` accepts GET, HEAD, POST, PUT and PATCH. It refuses other methods, a path that is relative, starts with `//`, or holds a query, fragment or backslash, a missing operation label, and a path its `Template` does not name. These refusals are `ErrRejected` and happen before a credential is asked for or anything is sent (`transport_test.go`, `TestRequestOffOriginRefused`; `observer_test.go`, `TestTemplateMatchesPath`).
- The URL is checked against the pinned https origin again before the token is set, and a round tripper below the HTTP client refuses any request off that origin (`TestOnOrigin`, `TestPinnedRoundTripper` in `transport_test.go`).
- Redirects are not followed. A 3xx other than 304 is an error, and the redirect target gets no request (`transport_test.go`, `TestRedirectNotFollowed`). A 304 comes back as an answer (`TestNotModified`); the transport does not check that the request was conditional.
- The wire request carries a context that keeps the caller's cancellation and deadline and returns no value for any key. A client trace in the caller's context gets no callback for any request, so trace hooks never see the `Authorization` header (`trace_test.go`, `TestCallerTraceNeverSeesTheWire`, `TestWireContextFollowsTheCaller`). The credential and the write check get the caller's context with its values.
- A token is asked for before each attempt with `Scope{Repo, Write}`, where `Write` is true for POST, PUT and PATCH (`TestScopeAsked`, `TestCallerTraceNeverSeesTheWire`).
- Requests with a body carry no `GetBody`, so Go's HTTP transport does not resend a body it has partly written (`TestRequestsCannotBeRewound`). The step from there to "at most one write reaches the host per write check" rests on how Go's transport behaves and has no test of its own.
- A 2xx body is read up to the request's `Limit` or the client's `BodyLimit` (2 MiB by default). A declared length over the bound is refused before reading, and a longer body is `ErrTooLarge` and is not retried (`TestBodyBounds`, `TestDefaultBodyLimit`). Bodies of other statuses are not read, except up to 16 KiB of a 403 searched for a rate-limit marker and then dropped.
- One call is bounded by `Timeout` (2 minutes by default) and one attempt, body included, by `AttemptTimeout` (60 seconds; `TestSlowBody`). The default HTTP transport sets TLS 1.2 as the minimum, 10 second dial and handshake timeouts, a 30 second wait for headers and a 256 KiB header limit, and keeps Go's proxy settings from the environment. Tests replace that transport, so none of these settings is tested.
- Reads (GET, HEAD) and writes marked `Idempotent` are retried on network errors, timeouts, 408, 429, 5xx and rate limits, up to `MaxAttempts` (4), with exponential backoff from `BaseDelay` (500 ms) and jitter (`TestTransientThenSuccess`, `TestNetworkErrorRetried`, `TestJitterApplied`). A write not marked idempotent is sent once (`TestWriteSentOnce`).
- A rate limit is a 429, or a 403 with `Retry-After`, with `X-RateLimit-Remaining: 0`, or whose first 16 KiB name a rate limit or abuse detection or cannot be read. The delay is the one the headers ask for, else one minute, capped at one hour. A delay up to `MaxWait` (15 seconds) is waited in place; a longer one is returned as `RetryableError` with the delay. No wait starts that would end past the context deadline, and a cancellation ends a wait (`TestRetryAfterAndRateLimits`, `TestRateLimited`, `TestNoWaitPastDeadline`, `TestCancelDuringBackoff`). A plain 403 comes back as an answer (`TestPlain403IsAnswer`).
- An answer or body that arrives after the call's context ended is dropped and the end of the context is returned (`TestAnswerAfterContextEnd`).
- Error text is fixed words, the operation label, the status and a valid refusal class. Errors of `net/http` and of the credential are replaced, and the credential's class (canceled, deadline, retryable, not found, forbidden, unauthorized) is kept. A helper in `transport_test.go` walks the whole error chain in the error tests for token, body, path and check canaries, URLs and the server address (`TestUnreachableHostScrubbed`, `TestCredentialErrorsKeepClassOnly`).
- The `Observer` gets one `Event` per attempt handed to the HTTP client: method, host, `Template`, status, attempt and duration. It is called synchronously after the body is read and closed, outside the attempt timeout, and `Do` starts no attempt past the caller's deadline after it (`observer_test.go`). An attempt stopped before the send (credential failure, write check refusal) gives no event. `Event` has no field of interface, map, slice, pointer, func or channel type (`TestEventFields`).
- A `Template` is a path with fixed segments of lowercase ASCII letters, digits, `.`, `_` and `-`, `{name}` for one segment and a final `{name...}` for one or more. It holds no value of the request beyond its fixed segments.
- `NextPage` reads the `rel="next"` link of an answer and returns the original request with its `page` parameter one higher. It reads only the page number from the link and does not follow or keep the link's URL. A link off the origin, a second next link, a missing, repeated or malformed page, a page outside 1 to 1000, or a page that is not the current one plus one is `ErrUntrustedAnswer`; a malformed page on the request itself is `ErrRejected` (`TestNextPage`, `FuzzNextPage`). There is no last-page result and no cursor support.
- `Config.RoundTripper` replaces the HTTP transport and is a trusted component: it sees the `Authorization` header. It sits in an internal package, so code outside the module cannot set it.

### 2. `Token`

- A `Token` prints, formats and marshals as a placeholder with every `fmt` verb tested, through `json`, `xml`, `gob` and `slog`, as a field, pointer, slice or map element (`token_test.go`, `TestTokenNeverPrints`, `TestTokenMarshalersReturnPlaceholder`).
- No exported function or method returns its value. Package `internal/secret` holds the one function that does, and the transport calls it after the origin check (`TestTokenReveal`).
- A token has a scheme, `Bearer` or `Basic` (a password with an empty user, for an Azure DevOps personal access token, not tried against the host), and an optional expiry.

### 3. `Credential` and `Signer`

- `Credential.Token(ctx, Scope)` returns a token for one request; `CredentialFunc` adapts a function. Caching and refresh are the credential's business.
- `Signer` has `Algorithm()` and `Sign(ctx, digest)`, with `SignatureAlgorithm` a named string type and `RS256` defined. No built code calls a `Signer` yet; the App token source will.

### 4. Error kinds

- Sentinels matched with `errors.Is`: `ErrNotFound`, `ErrPathNotInTree` (wraps `ErrNotFound`), `ErrConflict`, `ErrForbidden`, `ErrUnauthorized`, `ErrRejected`, `ErrTooLarge`, `ErrInvalidEntry`, `ErrUntrustedAnswer`, `ErrUnsupported`, `ErrInvalidConfig`, `ErrStoppedBeforeWrite`, `ErrOutcomeUnknown`, `ErrUnusableReceipt` and `ErrRetryable`. Types for `errors.As`: `*Error` (operation, status, kind), `*RetryableError` (with `RetryAfter`) and `*Refusal` (`errors_test.go`).
- `StatusError` maps 404 and 410 to `ErrNotFound`, 409 to `ErrConflict`, 403 to `ErrForbidden`, 401 to `ErrUnauthorized`, and 400 and 422 to `ErrRejected`. Other statuses, 405 among them, get no kind (`TestStatusError`).
- Once an attempt of a write may have reached the host (a network error, a timeout, 408, 5xx, or a context that ended before a non-2xx answer was taken), every later failure of the call except an unusable receipt matches `ErrOutcomeUnknown` as well as its own cause: a write check refusal, a credential error, a cancellation, a later status such as 409 or 422, or the last retry. Such an error never matches `ErrRetryable`. A rate-limited attempt did not write and leaves the outcome known (`outcome_test.go`, `TestOutcomeUnknownAcrossAttempts`).
- A 2xx answer to a write whose body is past the bound or cannot be read is `ErrUnusableReceipt` with the status. It is not sent again, it is not retryable, and it does not match `ErrOutcomeUnknown` (`TestUnusableReceipt`, first attempt). The same holds after an earlier lost attempt of an idempotent write, a case no test covers. The caller reads state to learn what was written.
- `context.Canceled` and `context.DeadlineExceeded` keep their identity; a deadline is also `RetryableError`.

### 5. `retryafter`

- `Parse` reads `Retry-After` in seconds or as an HTTP date; `ParseReset` reads a Unix reset time; `Header` uses `Retry-After` when present, else the reset when `X-RateLimit-Remaining` is `0`. Results are capped at the caller's maximum, and a past, negative or unreadable value gives zero (`retryafter_test.go`).
- A present but unusable `Retry-After` gives zero and the reset is not read; the transport then waits its one-minute default.
- The package is exported, keeps no state and makes no request, so other HTTP clients can use it.

## The write check

The transport calls one check, `WriteCheck`, immediately before every attempt of every request whose method is not GET or HEAD, after the token is set and the body is built (`transport_test.go`, `TestWriteCheckBeforeEachAttempt`, `TestReadsNotChecked`). Its name and its argument (`Send{Op, Method, Template, Attempt}`) are provisional and become final with the first typed write call.

Fixed now:

- It is called on every attempt, retries included, with the caller's context.
- A refusal stops the call and nothing more is sent; the error matches `ErrStoppedBeforeWrite`. A nil check refuses every write (`TestNilWriteCheckAllowsNoWrite`).
- A refusal carries only a class: a `*Refusal` found in the check's error whose `Class` is 1 to 32 lowercase ASCII letters, digits and dashes is copied into a new `Refusal`. The check's text and error are dropped, and an invalid class is dropped too (`outcome_test.go`, `TestRefusalClass`; `errors_test.go`, `TestRefusalValid`).

## Repository identity

`RepoRef` has `Host`, `Owner`, `Project` (Azure DevOps; empty on GitHub), `Name` and `ID`. `ID` is configuration: the product supplies the host's immutable repository ID with the names, and the client does not learn it from the host. The built transport passes `RepoRef` to the credential in `Scope` and does nothing else with it. Comparing `ID` with the host's answers comes with the typed calls.

`ObjectID.Valid` accepts 40 or 64 lowercase hexadecimal characters and refuses the all-zero ID of either length, which hosts read as "create" or "delete" a ref (`repo_test.go`).

`Host` names GitHub and Azure DevOps. The transport takes the host from its configuration; pinning `api.github.com` in code comes with the GitHub client.

Writes addressed by owner and name race against a rename, transfer or recreation of the repository between any check and the write. A credential restricted to the one repository closes that window; a broad token does not.

## Not decided here

- Typed GitHub calls: each is designed with its code in its own small pull request, with its design notes in the issue (armory issues 12 to 18).
- The GitHub App installation-token source: issue 21.
- The recorded-fixture server and contract suite: issue 11.
- Release controls (tag ruleset, attestation, the `githost` import allow-list and the contributor rules): issue 22, before any product pins a version.
- Azure DevOps calls: a later record after a spike. Input for it from the REST pages: ref updates take `oldObjectId` and `newObjectId` and report `staleOldObjectId`; `POST .../pushes` creates the commit and moves the ref in one request; the pull request update page does not say a completion checks any head.
- Product authorization. Sablecourt's push, check evidence and merge (its issues 133, 135 and 144) get Sablecourt's own record, accepted before that code. It answers: what an approval names, so an approval for one write cannot authorize another; how approvals are encoded if compared by digest; that the bytes approved are the bytes sent; where approval is required and who may reuse it after a retry or restart; which pull request races it accepts or narrows (head moved before create, retarget before update, close or merge, a second pull request to another base); whether review covers the source commit or the merge result; how the latest check result per producer is chosen and stale evidence refused; how an uncertain write is reconciled before the next one; which credential writes (a repository-restricted token, no ruleset bypass where host checks are relied on); and how a one-commit branch is built and its returned object IDs checked before the ref moves. Shadewright keeps its stale-plan gate, branch proofs, merge receipts and its `GitProvider` fake.

### Guidance for the per-call work (input, not a decision)

"Host" means the host refuses in the same request when the condition fails; "check-then-write" means any check the product makes beforehand can be overtaken.

| Planned call | Precondition | Notes from design review |
| --- | --- | --- |
| Create blob, create tree | none needed: content-addressed | The upload stores content even if no ref moves. In a tree, `sha: null` deletes an entry. |
| Create commit | none | Without author and committer dates a retry can create a second, unreferenced commit with another ID. Compare the returned ID before a ref moves. |
| Create ref | ref absent: host | A 422 also covers invalid requests and is not proof the ref exists. Ancestry and the target tip are check-then-write. |
| Move ref (GraphQL `updateRefs`) | expected old value and fast-forward: host, with nonzero IDs and `force: false` | The node ID binds the repository. A refused retry can come from protection or permission with the ref still at the old value. The ref can move away and back between reads. |
| Open pull request | check-then-write | Head and base are branch names whose tips can move. A duplicate refusal concerns the same head and base; a 422 alone does not diagnose it. |
| Update, close pull request | check-then-write | The PATCH carries no expected head, base, state or version, so concurrent edits can be overwritten. |
| Merge | head equals `sha`: host | No expected base tip, merge result or evidence. A 405 means the merge cannot go ahead; it does not diagnose a merge queue. A credential with bypass rights changes which protections apply. |
| Create check run | none | The SHA is literal. It does not show that the SHA is still the head or that the result stays current. |
| Post comment | check-then-write | Marker uniqueness is racy. The endpoint also accepts issues and closed pull requests. |

## Consequences

- Both products can share one transport and one token type once the typed calls exist. Until then the built code is a library with no exported client.
- Armory holds no authorization. Each product builds its own on the write check.
- A fix reaches a product when it bumps its armory version, so an urgent fix waits for both bumps.
- A bug in the transport affects both products' write paths. The transport tests, the contract suite of issue 11 and each product's own tests stand against it.
- Armory gains a public API, kept compatible from `v1`. The pull request title check and `AGENTS.md` need a `githost` scope (issue 22).

## Security consequences

The library guarantees, under the tests named above: a token does not appear in printed, marshalled or logged output; caller trace hooks see no request; requests leave only for the pinned origin and redirects are not followed; error text holds no token, body, URL or callback text; the write check runs before each write attempt and a refusal stops the call; a write that may have landed is reported as `ErrOutcomeUnknown` and an accepted write with an unusable answer as `ErrUnusableReceipt`.

The products keep: choosing and restricting the credential, which closes the name-addressed race; keeping `Credential`, `Token` and clients out of workloads; what the write check allows; reconciling uncertain outcomes; and anything a configured `RoundTripper` does, since it sees the token. A delay a credential reports in `RetryableError` is passed on without the one-hour cap. Armory reads no file, environment variable or secret store, except that the default HTTP transport follows Go's proxy settings from the environment.

## Alternatives considered

- Keep separate clients: no coordination, but each hardening fix is written twice and Sablecourt's opener stays unbounded until it is.
- A shared write authorization model: tried in earlier drafts; three design reviews did not converge.
- A third-party client such as `google/go-github`: it accepts a base URL and decodes successful answers without a caller-chosen bound, so each call would need its rules restored and re-checked on every upgrade.
- The package in Shadewright: Go refuses imports of another module's `internal/`, and exporting it would make the operator module a dependency of Sablecourt.

## Recorded decisions

1. GitHub Enterprise Server is not supported now. Neither product needs it, and a configurable host would widen the credential boundary.
2. Signed commits are out of scope for now: a commit recreated on the host without its signature has another ID.
3. Comments stay host-specific. Shadewright's marker depends on GitHub App identity and paging.
4. Webhook verification stays in the receiving products.
5. Shadewright keeps its `GitProvider` fake.
6. One armory module and one tag series, with a `githost` import allow-list separate from the redaction packages'.
7. This is ADR-0001. A later decision about record keeping does not renumber it.

## Stages

**Stage A.** The five parts above are built on `feat/githost-core`. Then the per-call work: each typed call in its own pull request with its design notes in the issue, the App source (issue 21) and the fixture server and contract suite (issue 11). Sablecourt adopts the transport first for its opener, through the open-pull-request call, which drops the configurable URL, redirect following and unbounded reads. Release controls (issue 22) are in place before Sablecourt's first pin (issue 19).

**Stage B.** Shadewright moves its transport and App authentication onto the package once the calls it needs exist, and keeps its adapter logic, gate and proofs. The pull request for that move lists and tests each behaviour difference.

## Issue dispositions

Armory issues, epic 7:

- 8 (core types, error kinds, `Token` and credentials) and 10 (pinned-origin transport and the write check): covered by this record.
- 11 to 19, 21 and 22: each carries its own design notes; this record decides none of them.

## Sources

Read on 2026-11-26.

- Armory, branch `feat/githost-core`: `githost/` (all files, tests included).
- GitHub REST API (version 2022-11-28): [Pulls](https://docs.github.com/en/rest/pulls/pulls?apiVersion=2022-11-28), [Check runs](https://docs.github.com/en/rest/checks/runs?apiVersion=2022-11-28), [Commit statuses](https://docs.github.com/en/rest/commits/statuses?apiVersion=2022-11-28), [Git commits](https://docs.github.com/en/rest/git/commits?apiVersion=2022-11-28).
- Azure DevOps REST API 7.1: [Pushes - Create](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pushes/create?view=azure-devops-rest-7.1), [Refs - Update Refs](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/refs/update-refs?view=azure-devops-rest-7.1), [Pull Requests - Update](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-requests/update?view=azure-devops-rest-7.1).
- Shadewright, on `main`: `internal/providers/github/`, [ADR-0048](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0048-redaction-in-a-shared-module.md). Sablecourt, on `main`: `internal/gate/mcp/`, `cmd/sablecourtd/main.go`.
