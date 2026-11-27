# ADR-0001: Package `githost`, transport and credentials for one Git host origin

- Status: Proposed
- Date: 2026-11-26

This record decides what is built in armory pull request 29 and was not contested in three design reviews: the pinned-origin transport, the `Token` type, the credential and signer interfaces, the error kinds and the `retryafter` package. Each rule below names the test that checks it when the rule is about security. Typed GitHub calls, the final shape of the write check and the App installation-token source are not decided here; "Not decided here" lists where each one is.

## Context

Shadewright and Sablecourt both call GitHub over HTTP with their own code. Shadewright has a hardened adapter, `internal/providers/github`: its transport pins `https://api.github.com`, refuses redirects, bounds bodies, retries with backoff and keeps secrets out of errors, and its kill switch runs before each write attempt. Sablecourt's `git.open_pr` action posts to `/repos/{owner}/{repo}/pulls` through `http.DefaultClient`, so it follows redirects and decodes a successful answer without a size bound. Its API base URL is a field of the opener's configuration, which only tests set today.

Both products need the same things from a client: one origin, no redirects, bounded reads, retries that do not resend a write by accident, a token that cannot leak through printing or tracing, and errors with no secrets in them. Writing and fixing that twice is the cost this package removes.

Earlier drafts of this record also shared a write authorization model: a closed set of operations, a digest over their encoding and a gate comparing it. The products authorize differently (Shadewright with a stale-plan gate in its controller, Sablecourt with approval of an action payload that names branches, not commit IDs), and three design reviews of those larger drafts each found new high-severity cases of the same kind. The shared model did not converge, so authorization stays in each product.

## Decision

The package has five built parts. Each rule is a short sentence a test can check. Test files are under `githost/`.

### 1. Pinned-origin transport (`internal/transport`)

The transport is internal: no exported constructor exists yet, and a product will reach it through the typed calls that are still to come.

- A client is built for one host. `New` refuses a host with a scheme, path, user, query, backslash or space, a missing credential, and a fixed `Authorization`, `Proxy-Authorization`, `Accept-Encoding`, `Idempotency-Key` or `X-Idempotency-Key` header (`TestNewRefusesConfig`, `TestNewRefusesBadHeaders`). A port is accepted only on `localhost` or a loopback IP, for tests (`TestHostPortOnlyOnLoopback`).
- `Do` accepts GET, HEAD, POST, PUT and PATCH. It refuses other methods, a path that is relative, starts with `//`, or holds a query, fragment or backslash, a missing operation label, and a path its `Template` does not name. These refusals are `ErrRejected` and happen before a credential is asked for or anything is sent (`transport_test.go`, `TestRequestOffOriginRefused`; `observer_test.go`, `TestTemplateMatchesPath`).
- The URL is checked against the pinned https origin again before the token is set, and a round tripper below the HTTP client refuses any request off that origin (`TestOnOrigin`, `TestPinnedRoundTripper` in `transport_test.go`).
- Redirects are not followed, and the redirect target gets no request (`transport_test.go`, `TestRedirectNotFollowed`). A 3xx answer to a write does not say whether the host wrote, so it is `ErrOutcomeUnknown` and is not retried (`TestRedirectOnWrite`). A 304 comes back as an answer only to a GET or HEAD that sent a usable condition: an `If-None-Match` that is not a valid entity-tag list or an `If-Modified-Since` that is not an HTTP date is refused before anything is sent (`TestNotModified`, `TestNotModifiedOnlyConditional`, `TestConditionalValidators`).
- The wire request carries a context that keeps the caller's cancellation and deadline and returns no value for any key. A client trace in the caller's context gets no callback for any request, so trace hooks never see the `Authorization` header (`trace_test.go`, `TestCallerTraceNeverSeesTheWire`, `TestWireContextFollowsTheCaller`). The credential and the write check get the caller's context with its values.
- A token is asked for before each attempt with `Scope{Repo, Write}`, where `Write` is true for POST, PUT and PATCH (`TestScopeAsked`, `TestCallerTraceNeverSeesTheWire`).
- Requests with a body carry no `GetBody`, so Go's HTTP transport does not resend a body it has partly written (`TestRequestsCannotBeRewound`). Go's transport resends a request by itself when it is a GET, HEAD, OPTIONS or TRACE, or when it carries an idempotency key header and nothing of it was written. `Do` sends none of those methods as a write and `New` refuses the two key headers, so a write is sent once per write check (`TestNoResendBelowTheCheck`, `TestReusedConnClosedBeforeBodilessWrite`). Go's HTTP/2 client has a further resend (Go 1.26.0 sends a bodiless request again after a protocol error from the peer), which is one reason the default transport does not speak HTTP/2.
- A 2xx body is read up to the request's `Limit` or the client's `BodyLimit` (2 MiB by default). A declared length over the bound is refused before reading, and a longer body is `ErrTooLarge` and is not retried (`TestBodyBounds`, `TestDefaultBodyLimit`). Bodies of other statuses are not read, except up to 16 KiB of a 403 searched for a rate-limit marker and then dropped.
- One call is bounded by `Timeout` (2 minutes by default; the credential lookup, the write check and the observer run under the caller's context, not the attempt timer, so a slow one is bounded by the caller's deadline only) and one attempt, body included, by `AttemptTimeout` (60 seconds; `TestSlowBody`). A write check that returns after the attempt's time is over stops the attempt before anything is sent, and that is not an unknown outcome (`TestSlowWriteCheckNotOutcomeUnknown`). The default HTTP transport is built by the package and takes nothing from `http.DefaultTransport`, so a dial hook, TLS setting or proxy installed there by another part of the process does not reach a request that carries a token (`TestNewIgnoresProcessTransport`, `TestNoProcessWideHTTPState`). It speaks HTTP/1.1 only (`TestDefaultClientSpeaksHTTP1`): Go's HTTP/2 debug log prints every header, `Authorization` included, and its resend rule is named above. It sets TLS 1.2 as the minimum, 10 second dial and handshake timeouts, a 30 second wait for headers and a 256 KiB header limit, and asks for no compression (`TestDefaultTransport`). It uses no proxy and does not read proxy settings from the environment (`TestDefaultTransportIgnoresEnvProxy`). A caller that wants a proxy sets `Config.Proxy`, and may pass `http.ProxyFromEnvironment` there. Through a proxy the request travels in a CONNECT tunnel: the proxy learns the host, the port and the timing, receives its own credential when its URL holds a user, and does not receive the token (`TestProxyNeverSeesTheToken`). A proxy URL that holds a user and is not https is refused before anything is sent, so a proxy credential does not travel in clear text (`TestProxyCleartextUserinfoRefused`).
- Reads (GET, HEAD) and writes marked `Idempotent` are retried on network errors, timeouts, 408, 429, 5xx and rate limits, up to `MaxAttempts` (4), with exponential backoff from `BaseDelay` (500 ms) and jitter (`TestTransientThenSuccess`, `TestNetworkErrorRetried`, `TestJitterApplied`). A write not marked idempotent is sent once (`TestWriteSentOnce`).
- A rate limit is a 429, or a 403 with `Retry-After`, with `X-RateLimit-Remaining: 0`, or whose first 16 KiB name a rate limit or abuse detection or cannot be read. The delay is the one the headers ask for, else one minute, capped at one hour. A delay up to `MaxWait` (15 seconds) is waited in place; a longer one is returned as `RetryableError` with the delay. No wait starts that would end past the context deadline, and a cancellation ends a wait (`TestRetryAfterAndRateLimits`, `TestRateLimited`, `TestNoWaitPastDeadline`, `TestCancelDuringBackoff`). A plain 403 comes back as an answer (`TestPlain403IsAnswer`).
- An answer or body that arrives after the call's context ended is dropped and the end of the context is returned (`TestAnswerAfterContextEnd`).
- Error text is fixed words, the operation label, the status and a valid refusal class. The class is chosen by the product's write check and copied as is when it has the permitted characters, so a product must not build it from a repository name, a token or any input (`TestRefusalClass` checks the character rule only). Errors of `net/http` and of the credential are replaced, and the credential's class (canceled, deadline, retryable, not found, forbidden, unauthorized) is kept. A helper in `transport_test.go` walks the whole error chain in the error tests for token, body, path and check canaries, URLs and the server address (`TestUnreachableHostScrubbed`, `TestCredentialErrorsKeepClassOnly`).
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
- Once an attempt of a write may have reached the host (a network error, a timeout, 408, 5xx, or a context that ended before a non-2xx answer was taken), every later failure of the call matches `ErrOutcomeUnknown` as well as its own cause: a write check refusal, a credential error, a cancellation, a later status such as 409 or 422, or the last retry. Such an error never matches `ErrRetryable`. A rate-limited attempt did not write and leaves the outcome known (`outcome_test.go`, `TestOutcomeUnknownAcrossAttempts`).
- A 2xx answer to a write whose body is past the bound or cannot be read is `ErrUnusableReceipt` with the status. It is not sent again, it is not retryable, and on a first attempt it does not match `ErrOutcomeUnknown` (`TestUnusableReceipt`). After an earlier lost attempt it matches both: one write was accepted, and the earlier one may have been applied too (`TestUnusableReceiptAfterLostAttempt`). The caller reads state to learn what was written. This holds for REST calls, where 2xx means the host applied the request. A GraphQL answer can be 200 with an error in its body (Shadewright's `updateRefs` does this), so the typed GraphQL call must read the body for errors before it counts a 2xx as accepted; that is decided with the call.
- `context.Canceled` and `context.DeadlineExceeded` keep their identity; a deadline is also `RetryableError`.

### 5. `retryafter`

- `Parse` reads `Retry-After` in seconds or as an HTTP date; `ParseReset` reads a Unix reset time; `Header` uses `Retry-After` when present, else the reset when `X-RateLimit-Remaining` is `0`. Results are capped at the caller's maximum, and a past, negative or unreadable value gives zero (`retryafter_test.go`).
- A present but unusable `Retry-After` falls back to the rate limit reset; when both are unusable the transport waits its one-minute default.
- The package is exported, keeps no state and makes no request, so other HTTP clients can use it.

## The write check

The transport calls one check, `WriteCheck`, immediately before every attempt of every request whose method is not GET or HEAD, after the token is set and the body is built (`transport_test.go`, `TestWriteCheckBeforeEachAttempt`, `TestReadsNotChecked`). Its name and its argument (`Send{Op, Method, Template, Attempt}`) are provisional and become final with the first typed write call.

Fixed now:

- It is called on every attempt, retries included, with the caller's context.
- A refusal stops the call and nothing more is sent; the error matches `ErrStoppedBeforeWrite`. A nil check refuses every write (`TestNilWriteCheckAllowsNoWrite`).
- A refusal carries only a class: a `*Refusal` found in the check's error whose `Class` is 1 to 32 lowercase ASCII letters, digits and dashes is copied into a new `Refusal`. The check's text and error are dropped, and an invalid class is dropped too (`outcome_test.go`, `TestRefusalClass`; `errors_test.go`, `TestRefusalValid`).

## Reading of invariant 3

Note, 2026-11-27. Invariant 3 says Armory makes network calls only when that is the package's whole purpose and "the caller supplies the client", and that parsers read within "limits the caller sets". `githost` builds its own HTTP transport and has its own defaults (body limit, timeouts, attempts). This is a deliberate reading of the invariant, not a weakening of it. The purpose of the rule is that Armory cannot reach a host the caller did not choose, cannot read a credential on its own and cannot leak one. The package keeps all three: the caller supplies the host and every `Token` through `Credential`, and Armory reads no environment variable, file or proxy setting. A client the caller built would instead let a dial hook, a redirect policy, a proxy or an HTTP/2 debug log see the `Authorization` header, which is the leak the rule exists to prevent. The main defaults are limits the caller can set (`Limit`, `BodyLimit`, `Timeout`, `AttemptTimeout`, `MaxAttempts`). Some bounds are fixed in the package: the 16 KiB rate-limit probe of a 403, the 256 KiB header limit, the page cap of 1000 and the 128-byte limit on a `Retry-After` value. Fixed bounds are stricter than any caller needs, not looser, so they satisfy invariant 4 without a setting. The caller's own client is replaced by one the package builds, and `Config.RoundTripper` is internal for the same reason. `AGENTS.md` invariant 3 does not mention this reading; whether to add a sentence there is for the user to decide when accepting this record.

## Repository identity

`RepoRef` has `Host`, `Owner`, `Project` (Azure DevOps; empty on GitHub), `Name` and `ID`. `ID` is configuration: the product supplies the host's immutable repository ID with the names, and the client does not learn it from the host. The built transport passes `RepoRef` to the credential in `Scope` and does nothing else with it. Comparing `ID` with the host's answers comes with the typed calls.

`ObjectID.Valid` accepts 40 or 64 lowercase hexadecimal characters and refuses the all-zero ID of either length, which hosts read as "create" or "delete" a ref (`repo_test.go`).

`Host` names GitHub and Azure DevOps. The transport takes the host from its configuration; pinning `api.github.com` in code comes with the GitHub client.

Writes addressed by owner and name race against a rename, transfer or recreation of the repository between any check and the write. A credential restricted to the one repository closes that window only if every refresh of it is bound to the intended repository ID, and a broad token does not close it. Neither product does this today: Shadewright asks for a token by repository name and checks `full_name`, and Sablecourt's command line requests the OAuth `repo` scope and its opener takes owner and name from each action. Each adoption pull request must show how the ID is bound.

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
- Armory holds no authorization. The write check is a stop switch: it sees `Op`, `Method`, `Template` and `Attempt`, not the path or the body. Each product authorizes before it makes the typed call, and the check can only stop what the product already allowed.
- A fix reaches a product when it bumps its armory version, so an urgent fix waits for both bumps.
- A bug in the transport affects both products' write paths. The transport tests, the contract suite of issue 11 and each product's own tests stand against it.
- Armory gains a public API, kept compatible from `v1`.

## Security consequences

The library guarantees, under the tests named above and with the one limit below: a token does not appear in printed, marshalled or logged output; caller trace hooks see no request; requests leave only for the pinned origin and redirects are not followed; error text holds no token, body, URL or callback text; the write check runs before each write attempt and a refusal stops the call; a write that may have landed is reported as `ErrOutcomeUnknown` and an accepted write with an unusable answer as `ErrUnusableReceipt`; a 2xx answer to a write carries `EarlierAttemptUnknown` when an earlier attempt of the same call may also have written (`TestEarlierAttemptUnknown`).

One limit: Go's HTTP/1.1 client writes a prefix of unsolicited bytes a host sends on an idle connection to the process's default logger (`TestIdleChannelUnsolicitedPeerBytesLogged`). Those are bytes the host sent. The package cannot switch that log off, and a host that echoes a credential back there would put it in the process log.

The products keep: choosing and restricting the credential, which closes the name-addressed race; keeping `Credential`, `Token` and clients out of workloads; what the write check allows; reconciling uncertain outcomes; and anything a configured `RoundTripper` does, since it sees the token. A delay a credential reports in `RetryableError` is capped at one hour like any other (`TestCredentialRetryAfterBounded`). Armory reads no file, environment variable or secret store. A product that sets `Config.Proxy` chooses what the proxy learns. The proxy selector function is called with the request after the token is set, so it can see the `Authorization` header (`TestProxyNeverSeesTheToken` tests the proxy server, not the selector). `Config.Proxy` stays internal until the selector gets a request without credentials.

## Alternatives considered

- Keep separate clients: no coordination, but each hardening fix is written twice and Sablecourt's opener stays unbounded until it is.
- A shared write authorization model: tried in earlier drafts; three design reviews did not converge.
- A third-party client such as `google/go-github`: it accepts a base URL and decodes successful answers without a caller-chosen bound, so each call would need its rules restored and re-checked on every upgrade.
- The package in Shadewright: Go refuses imports of another module's `internal/`, and exporting it would make the operator module a dependency of Sablecourt.

## Recorded decisions

1. GitHub Enterprise Server is not supported now. Neither product needs it, and a configurable host would widen the credential boundary.
2. Signed commits are out of scope for now: a commit recreated on the host without its signature has another ID. Sablecourt's branch push (its issue 133) therefore pushes unsigned commits or uses the git command line, not this package's API calls.
3. Comments stay host-specific. Shadewright's marker depends on GitHub App identity and paging.
4. Webhook verification stays in the receiving products.
5. Shadewright keeps its `GitProvider` fake.
6. One armory module and one tag series, with a `githost` import allow-list separate from the redaction packages'.
7. This is ADR-0001. A later decision about record keeping does not renumber it.

## Stages

**Stage A.** The five parts above are built in pull request 29. Then the per-call work: each typed call in its own pull request with its design notes in the issue, the App source (issue 21) and the fixture server and contract suite (issue 11). Sablecourt adopts the transport first for its opener, through the open-pull-request call, which drops the configurable URL, redirect following and unbounded reads. The opener today returns a plain error that the action handler maps to a redacted 502, so that pull request must also pass `ErrStoppedBeforeWrite` and `ErrOutcomeUnknown` through, so a retry can tell a refusal before sending from a pull request that may exist. Release controls (issue 22) are in place before Sablecourt's first pin (issue 19).

**Stage B.** Shadewright moves its transport and App authentication onto the package once the calls it needs exist, and keeps its adapter logic, gate and proofs. It is not a change with no behaviour difference. Shadewright's default transport today is a clone of `http.DefaultTransport` (`internal/providers/github/client.go`, `newHTTPClient`), so it takes the proxy from the environment (`HTTPS_PROXY`, `NO_PROXY`) and negotiates HTTP/2. The package transport does neither: it uses no proxy unless the caller sets `Config.Proxy`, which is internal today, and it speaks HTTP/1.1 only. When Shadewright switches it must check whether any deployment relies on an environment proxy (and then needs `Config.Proxy` exported with `http.ProxyFromEnvironment`, a change under invariant 5), that no part of it needs HTTP/2 features, and that connection reuse and latency under HTTP/1.1 stay acceptable. Two more differences: Shadewright's feedback reader accepts pages up to 30 times 400 KiB and decodes one item at a time, while the package reads a whole 2xx body up to the limit (2 MiB by default), so Stage B needs a streaming read or a larger per-request limit with its memory cost stated; and Shadewright's `CheckWrite` allows a write when the context carries no checkpoint, while the package refuses every write when its check is nil, so the adapter must decide what an unguarded context does. The pull request for that move lists and tests each behaviour difference.

## Issue dispositions

Armory issues, epic 7:

- 8 (core types, error kinds, `Token` and credentials) and 10 (pinned-origin transport and the write check): covered by this record.
- 11 to 19, 21 and 22: each carries its own design notes; this record decides none of them.

## Sources

Read on 2026-11-26.

- Armory, pull request 29: `githost/` (all files, tests included).
- GitHub REST API (version 2022-11-28): [Pulls](https://docs.github.com/en/rest/pulls/pulls?apiVersion=2022-11-28), [Check runs](https://docs.github.com/en/rest/checks/runs?apiVersion=2022-11-28), [Commit statuses](https://docs.github.com/en/rest/commits/statuses?apiVersion=2022-11-28), [Git commits](https://docs.github.com/en/rest/git/commits?apiVersion=2022-11-28).
- Azure DevOps REST API 7.1: [Pushes - Create](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pushes/create?view=azure-devops-rest-7.1), [Refs - Update Refs](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/refs/update-refs?view=azure-devops-rest-7.1), [Pull Requests - Update](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-requests/update?view=azure-devops-rest-7.1).
- Shadewright, on `main`: `internal/providers/github/`, [ADR-0048](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0048-redaction-in-a-shared-module.md). Sablecourt, on `main`: `internal/gate/mcp/`, `cmd/sablecourtd/main.go`.
