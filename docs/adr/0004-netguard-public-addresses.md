# ADR-0004: Package `netguard`, which IP addresses count as public and how the table stays current

- Status: Accepted (2026-11-27)
- Date: 2026-11-27

This record decides the classification rule of the planned `netguard` package (epic 49, issue 50): which addresses a product may dial when it means "the public internet", how addresses that embed an IPv4 address are judged, what a product may widen and in which order the rules apply, how the guard runs at dial time, and how the range table is traced to the IANA registries and kept current. No code exists yet.

## Context

Shadewright and Sablecourt both refuse to send a credential or a request body to an address that is not public: a model endpoint or a provider host that resolves to a cluster address, a link-local address or a loopback port must get nothing. Cloud metadata services are only partly covered (see "Limits: metadata services"). Each product keeps its own range list, and the two lists differ.

- Shadewright, `internal/providers/model/openaiwire/client.go`: `publicAddress`, the `Guard` type and its `Control` dial hook, `PublicControl`, `ErrDialRefused`, and `PrivateServiceControl` for an explicitly named in-cluster Service. It judges NAT64 (`64:ff9b::/96`), 6to4 (`2002::/16`) and IPv4-compatible (`::/96`) addresses by the IPv4 address inside them, refuses Teredo, local-use NAT64 and SIIT outright, and lists `3fff::/20`. It relies on `netip.Addr.IsGlobalUnicast` for IPv6, which is true for any address outside a few fixed blocks, so `5f00::/16` (SRv6 SIDs, added to the registry in 2024) and `100:0:0:1::/64` (dummy prefix, added in 2025) pass. It refuses all of `192.0.0.0/24` and `2001::/23`, including the rows inside them that the registry marks globally reachable. `PrivateServiceControl` allows a private address only when the address itself is private: `64:ff9b::a00:1` and `2002:a00:1::1` (`10.0.0.1` inside NAT64 and 6to4) are refused there too.
- Sablecourt, `internal/gate/councilproxy/proxy.go`: `specialAddresses` (11 prefixes) and `publicAddress`. It allows every IPv6 address inside `2000::/3` that is not listed, so documentation prefix `3fff::/20` passes, and it omits `192.88.99.0/24` (deprecated 6to4 relay anycast). It refuses NAT64 and 6to4 outright, so a host on an IPv6-only network behind DNS64 cannot reach a provider. It resolves the name with a trailing dot, refuses the whole request when any returned address is not public, and then dials the checked IP literal, so a second lookup cannot swap the address. It unmaps an address before checking its zone, so `::ffff:8.8.8.8%eth0` loses its zone before the check.

The in-cluster range checks in Shadewright (`openaicompatible/declarations.go`, `vertexai/sts/sts.go`, and `Guard.InCluster`) are product policy and stay in Shadewright as arguments.

Two lists that already disagree, each with a known miss, are the reason this belongs in Armory: one table, traced to the registries with a date, and one test that fails when a registry row is missing.

Invariants touched: 1 (the package knows no product's ranges; extra prefixes are arguments), 2 (standard library only: `net/netip`, `net`, `syscall`, also in tests), 3 (no network calls and no resolver; error text never quotes an address), 4 (the string entry points are bounded and fuzzed) and 5 (the classes and the policy type become public API).

## Decision

### Sources

The table follows three IANA registries, read on 2026-11-27:

| Registry | Last updated | Pinned copy |
| --- | --- | --- |
| [IANA IPv4 Special-Purpose Address Registry](https://www.iana.org/assignments/iana-ipv4-special-registry/) | 2025-10-09 | `netguard/testdata/iana/iana-ipv4-special-registry-1.csv` |
| [IANA IPv6 Special-Purpose Address Registry](https://www.iana.org/assignments/iana-ipv6-special-registry/) | 2025-10-09 | `netguard/testdata/iana/iana-ipv6-special-registry-1.csv` |
| [IPv6 Global Unicast Address Assignments](https://www.iana.org/assignments/ipv6-unicast-address-assignments/) | 2025-10-10 | `netguard/testdata/iana/ipv6-unicast-address-assignments.csv` |

The pinned copies are the CSV files IANA publishes, committed unchanged, with the "Last updated" date of each recorded in `netguard/testdata/iana/DATES` and in exported constants.

### How an address is classified

`Classify` applies these steps in order and stops at the first that gives a class. Every address gets exactly one class.

1. Not valid: `Invalid`. Carries a zone: `Zoned`. The zone is checked before anything else, so `::ffff:8.8.8.8%eth0` is `Zoned` and is never unmapped first.
2. IPv4-mapped (`::ffff:0:0/96`): unmapped, and the rest of the steps run on the IPv4 address. This is what the kernel dials.
3. Protocol classes: unspecified (`0.0.0.0`, `::`) is `Unspecified`; loopback (`127.0.0.0/8`, `::1`) is `Loopback`; link-local (`169.254.0.0/16`, `fe80::/10`) is `LinkLocal`; multicast (`224.0.0.0/4`, `ff00::/8`) and the IPv4 limited broadcast (`255.255.255.255`) are `Multicast`.
4. Embedded IPv4 forms (the table in the next section). A readable form takes the class of the IPv4 address inside it, found by running steps 3 to 7 on that address. An unreadable form is `Opaque`. This step comes before the special-purpose rows, so the Globally Reachable column of `64:ff9b::/96` (`True`), `2002::/16` and `2001::/32` (`N/A`) never decides the class of an address in them.
5. Private-use (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`) and unique-local (`fc00::/7`): `Private`.
6. The special-purpose rows decide. Any address in an IANA special-purpose row gives `NonGlobal` or `Deprecated`. No row gives `Public`, whatever its Globally Reachable column says. An address inside a deprecated row (a termination date and empty attributes: `192.88.99.0/24`, `2001:10::/28`) gives `Deprecated`, any other row gives `NonGlobal`. The rows marked `True` are anycast or service addresses that the nearest operator answers, often the local gateway: PCP anycast (`192.0.0.9`, `2001:1::1`), `192.0.0.10`, AMT and AS112. Both products refuse `192.0.0.0/24` and `2001::/23` today, and this record keeps that. It also needs no most-specific-row matching: a row inside another row never changes the answer.
7. What is left: IPv6 inside a prefix the unicast assignments registry designates to a regional internet registry (AFRINIC, APNIC, ARIN, LACNIC, RIPE NCC) is `Public`, other IPv6 is `Undelegated`, and other IPv4 is `Public`.

Step 7 fails closed for IPv6: a block IANA has not delegated (`3000::/5`, `3fff::/20` before its row existed, `5f00::/16`, all of `4000::/3` and beyond) is not public by default, so a future special-purpose allocation in undelegated space is refused before the table learns about it. IPv4 needs no such step: the IPv4 space outside the special rows is fully allocated, and `240.0.0.0/4` has its own row.

Steps 1 to 5 are protocol rules written in code. Step 6 is the table, which must match the two special-purpose registries row for row, except for the embedded-form rows that step 4 handles (`::ffff:0:0/96`, `64:ff9b::/96`, `64:ff9b:1::/48`, `2001::/32`, `2002::/16`). `::/96` and `::ffff:0:0:0/96` are not in the special-purpose registry and are protocol rules only.

### Addresses that embed an IPv4 address

| Form | Prefix | Rule |
| --- | --- | --- |
| IPv4-mapped | `::ffff:0:0/96` | Unmapped and judged as the IPv4 address (step 2). |
| NAT64 well-known prefix (RFC 6052) | `64:ff9b::/96` | Readable: the IPv4 address in the last 32 bits. A DNS64 answer for a public host connects, one for `10.0.0.1` does not. |
| 6to4 (RFC 3056, deprecated by RFC 7526) | `2002::/16` | `Opaque`: DNS64 never synthesises a 6to4 address, so no product has a reason to dial one. |
| NAT64 network-specific prefix | named by the caller in `Policy.AllowNAT64Prefixes`, `/96` only | Readable under that policy only: the IPv4 address in the last 32 bits. `Classify` does not know such a prefix. |
| IPv4-compatible (deprecated by RFC 4291) | `::/96`, except `::` and `::1` | `Opaque`. |
| Local-use NAT64 (RFC 8215) | `64:ff9b:1::/48` | `Opaque`: the embedding is set by the local operator and cannot be read reliably. A product that knows its operator's `/96` inside it names that prefix in `Policy.AllowNAT64Prefixes`. |
| Teredo (RFC 4380) | `2001::/32` | `Opaque`: the embedded IPv4 is obfuscated and the server address is a second embedded address. |
| SIIT IPv4-translated | `::ffff:0:0:0/96` | `Opaque`. |

Shadewright judges IPv4-compatible addresses by their IPv4 part. This record refuses them: the form was deprecated in 2006, nothing current uses it, and where the packet goes depends on tunnel configuration of the host. Refusing it is the stricter choice and costs nothing known.

RFC 6052 (section 3.1) forbids the well-known NAT64 prefix for non-global IPv4 addresses. An address such as `64:ff9b::a00:1` is therefore classed `Private` (the class of `10.0.0.1`), but no policy allows it: `AllowPrivate` covers only an address that is private itself (see "Policy and precedence").

### Policy and precedence

The zero `Policy` allows `Public` addresses only. A product widens it with two fields, each named for what it widens, and narrows it with one. `Allow` applies these rules in order and stops at the first that decides:

1. `Validate` fails: refused (`ErrInvalidPolicy`).
2. The address is invalid or zoned: refused. This comes before the NAT64 prefix steps, so a zoned address inside an operator prefix never reaches them.
3. The address, or any IPv4 address readable inside it (IPv4-mapped, well-known NAT64, or a prefix in `AllowNAT64Prefixes`), is inside a `Deny` prefix: refused. `Deny` wins over both widening fields.
4. The address is inside a prefix in `AllowNAT64Prefixes`: the IPv4 address in its last 32 bits is classified, and the address is allowed only if that IPv4 address is `Public`. An embedded private IPv4 address is refused even with `AllowPrivate`.
5. `Classify` gives `Public`: allowed.
6. `Classify` gives `Private`, `AllowPrivate` is set, and the address itself (after unmapping an IPv4-mapped address) is inside `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` or `fc00::/7`: allowed. A `Private` class reached through NAT64 is refused.
7. Anything else: refused.

`Validate` refuses a `Policy` when:

- a prefix in either list is invalid or has host bits set (`10.0.0.1/8`);
- a `Deny` prefix is IPv4-mapped (`::ffff:10.0.0.0/104`): addresses are unmapped before the check, so such a prefix would never match. The caller writes `10.0.0.0/8`. A `Deny` prefix that covers everything (`0.0.0.0/0`, `::/0`) is accepted, since it only refuses more;
- a prefix in `AllowNAT64Prefixes` is not an IPv6 `/96`, or lies outside the three places an operator NAT64 prefix can sit: inside `64:ff9b:1::/48`, inside `fc00::/7`, or inside a prefix designated to a regional registry with no special-purpose row overlapping it. This refuses link-local, multicast, documentation, IPv4-mapped, IPv4-compatible, SIIT, Teredo, 6to4 and the well-known prefix itself, so a configured prefix cannot turn `fe80::808:808` or `::808:808` into a public address;
- a list is longer than its limit (1024 for `Deny`, 16 for `AllowNAT64Prefixes`).

### Limits: metadata services

`netguard` does not block every cloud metadata service. `169.254.169.254` is link-local and refused, but `168.63.129.16` (Azure) is `Public`, and `fd00:ec2::254` (AWS) and `fd20:ce::254` (Google Cloud) are unique-local, so `AllowPrivate` allows them. Neither product blocks them today. A product that dials a declared private service passes such addresses in `Deny`, which wins over `AllowPrivate`.

Shadewright's `Guard{AllowPrivateService: true}` ignores `InCluster` today. Under this record `Deny` always applies, so Shadewright keeps its current behaviour by passing no `Deny` for a declared Service (see Adoption).

### Public API

```go
package netguard

// Class is the category of an address. The zero value is Invalid. Callers that switch on Class need a
// default branch: a later minor version may add a value.
type Class uint8

const (
	Invalid     Class = iota // not a valid address
	Public                   // public unicast: may be dialled as the public internet
	Private                  // private-use IPv4 or unique-local IPv6
	Loopback
	LinkLocal
	Multicast                // also the IPv4 limited broadcast
	Unspecified
	NonGlobal                // in a special-purpose row (never Public, whatever its Globally Reachable column says)
	Deprecated               // in a deprecated special-purpose row
	Undelegated              // IPv6 outside the space IANA has designated to a regional registry
	Opaque                   // embeds an IPv4 address that cannot be read reliably
	Zoned                    // carries a zone, such as fe80::1%eth0
)

// String returns the lower-case name of c ("public", "nonglobal"), or "unknown" for a value not listed.
func (c Class) String() string

// Classify returns the class of ip by the steps of ADR-0004. A readable embedded IPv4 address gives the
// class of that address.
func Classify(ip netip.Addr) Class

// Public reports whether Classify(ip) is Public.
func Public(ip netip.Addr) bool

// ParseAddr parses a textual IP address strictly: at most 64 bytes, no zone, no leading zeros in an
// IPv4 part, no shortened or numeric IPv4 forms (127.1, 0x7f000001). It is the entry point for
// untrusted strings.
func ParseAddr(s string) (netip.Addr, error)

// Policy is a product's dial rule. The zero Policy allows Public addresses only. Deny wins over both
// Allow fields.
type Policy struct {
	// AllowPrivate also allows an address that is itself private-use IPv4 or unique-local IPv6, for a
	// service the product has named explicitly. It never allows a private address embedded in NAT64.
	AllowPrivate bool
	// AllowNAT64Prefixes names the operator's network-specific NAT64 prefixes (IPv6 /96, at most 16).
	// An address inside one is allowed when the IPv4 address in its last 32 bits is Public.
	AllowNAT64Prefixes []netip.Prefix
	// Deny lists further prefixes to refuse, such as pod or Service ranges. An address is refused when
	// it, or an IPv4 address readable inside it, is inside one. At most 1024 prefixes.
	Deny []netip.Prefix
}

// Validate reports whether p is usable; see ADR-0004 for the rules. Its error matches ErrInvalidPolicy.
func (p Policy) Validate() error

// Allow returns nil when p allows ip. Otherwise it returns a *Refusal, which matches ErrRefused, and
// also ErrInvalidPolicy when p does not validate.
func (p Policy) Allow(ip netip.Addr) error

// Control is a net.Dialer Control function. It allows a connection only when network is tcp, tcp4
// or tcp6 (TCP only; every other network is refused), address is at most 80 bytes, its host is an IP literal of the network's
// family with no zone, its port is a decimal number from 1 to 65535 with no sign or leading zero, and
// p allows the address. Every refusal is a *Refusal and matches ErrRefused; a malformed network or
// address also matches ErrInvalidAddr.
func (p Policy) Control(network, address string, c syscall.RawConn) error

// ControlContext is Control for net.Dialer.ControlContext. It returns ctx.Err() unchanged when ctx is
// already done, and otherwise behaves as Control.
func (p Policy) ControlContext(ctx context.Context, network, address string, c syscall.RawConn) error

// Refusal is the error of a refused address. Its text is fixed per reason and never contains the
// address, the port, the network name or a prefix.
type Refusal struct {
	Class  Class // the class of the address, Invalid for a malformed network or address
	Denied bool  // the address matched Deny
}

func (r *Refusal) Error() string
func (r *Refusal) Unwrap() []error // ErrRefused, plus ErrInvalidPolicy or ErrInvalidAddr when they apply

var (
	ErrRefused       = errors.New("netguard: address refused")
	ErrInvalidPolicy = errors.New("netguard: invalid policy")
	ErrInvalidAddr   = errors.New("netguard: invalid address")
)

// Dates of the IANA registries the table follows.
const (
	IPv4SpecialUpdated = "2025-10-09"
	IPv6SpecialUpdated = "2025-10-09"
	IPv6UnicastUpdated = "2025-10-10"
)
```

`Allow`, `Control` and `ControlContext` run `Validate` first and refuse when it fails, so a product that never calls `Validate` still fails closed. A caller marks a dial failure as "not sent" with `errors.Is(err, netguard.ErrRefused)`, which holds for every refusal.

### Dial-time checking and DNS rebinding

`netguard` has no resolver and reads no name. `net.Dialer` calls `Control` with the resolved IP literal of each connection attempt, after resolution and before connect, for every address of a name (both families under Happy Eyeballs) and again for every new connection. A name that resolves to a public address on one lookup and to `127.0.0.1` on the next is refused on the connection that would reach `127.0.0.1`.

Checking the address when the name is resolved and then dialling the name again is not safe: the second lookup can return a different address. A product uses one of two forms:

- Resolve the name itself (with a trailing dot, so search domains cannot change the target), call `Allow` on every returned address, refuse the whole request if any address is refused, and dial only a checked IP literal. This is what Sablecourt does today. It refuses an answer that mixes a public address with `127.0.0.1`, which `Control` alone does not: a dialer with `Control` refuses the loopback attempt but still connects to the public one.
- Dial the name through a `net.Dialer` whose `Control` is `Policy.Control`. The dialer checks every address it connects to, including connections an HTTP client opens on its own for retries and new requests.

The first form may also set `Control` on its dialer, as a second check on the literal it dials.

What `Control` does not cover, and each product handles in its own transport:

- A pooled keep-alive connection is not dialled again, so `Control` does not run on reuse. A product that changes its `Policy` closes the idle connections of transports built with the old one. An active HTTP/2 connection outlives a `Policy` change, and Go does not coalesce connections across hosts.
- A redirect to another public host passes `Control`. A client that sends a credential disables redirects (`CheckRedirect` returning `http.ErrUseLastResponse`), as Shadewright's model clients do.
- Through a proxy, `Control` sees the proxy, not the target. A transport that relies on the guard sets `Proxy` to `nil`, because `http.DefaultTransport` reads `HTTP_PROXY`, `HTTPS_PROXY` and `ALL_PROXY` from the environment. Shadewright's `openaiwire.NewHTTPClient` sets it to `nil`; the Git host transport of ADR-0001 is a separate question for that package.
- A `DialTLSContext` on `http.Transport` replaces `DialContext`, so a product that sets one dials through the guarded `net.Dialer` inside it.

## Adoption

### Shadewright 0.8.0

`openaiwire.publicAddress`, `reservedIPv4`, `reservedIPv6`, `PublicControl` and the embedded-form code, and `embeddedIPv4Forms` in `openaicompatible/declarations.go` and `vertexai/sts/sts.go`, are removed. `Guard{InCluster: p}` becomes `netguard.Policy{Deny: p}`, and `PrivateServiceControl` becomes `Policy{AllowPrivate: true}` with no `Deny`, which keeps today's rule that `InCluster` does not apply to a declared Service (`openaicompatible` builds its guard with `AllowPrivateService: cfg.Service != nil`). The in-cluster lists in `openaicompatible`, `vertexai` and `vertexai/sts` are passed as `Deny`. `foundry` and its Entra client use the zero `Policy`. `openaiwire.ErrDialRefused` is replaced by `netguard.ErrRefused` at every `errors.Is` site (`openaicompatible`, `vertexai`, `foundry`), and a refusal stays non-retryable and "not sent". What changes for Shadewright's users:

- Newly refused: `5f00::/16`, `100:0:0:1::/64`, IPv6 in undelegated space (for example `3000::/5`), IPv4-compatible addresses with a public IPv4 part, and 6to4 addresses (`2002::/16`).
- Newly allowed: nothing from the special-purpose rows. `192.0.0.0/24` and `2001::/23` stay refused whole.
- Unchanged for a declared Service: a private address embedded in NAT64 or 6to4 stays refused.
- A cluster on an IPv6-only network with an operator NAT64 prefix sets it in the chart so it reaches `Policy.AllowNAT64Prefixes`. That is a new chart value and needs its own Shadewright issue.

The Shadewright pull request lists these differences in its description and adds a test per difference.

### Sablecourt after 0.1.0

`councilproxy.publicAddress` and `specialAddresses` are replaced by `netguard.Policy{}.Allow` inside the existing loop: `providerConnect` keeps resolving with a trailing dot, keeps refusing the request when any returned address is refused, and keeps dialling the checked literal. It adds `Policy{}.Control` to the dialer as a second check. What changes:

- Newly refused: `3fff::/20`, `192.88.99.0/24`, IPv6 in undelegated space inside `2000::/3`, and a zoned IPv4-mapped address (the zone is now checked before unmapping).
- Newly allowed: NAT64 addresses whose IPv4 address is public, unless Sablecourt denies them. Sablecourt passes `64:ff9b::/96` in `Deny` until its own issue decides otherwise, so it is not weaker after the switch. 6to4 stays refused, now as `Opaque`.
- Its proxy table tests keep running against the new call, and a test for `3fff::1` and `192.88.99.1` is added first, failing on the current code.

## How the table is kept current

- The table is hand-written Go (`netguard/table.go`), one line per registry row with its name and RFC in a comment. It is not generated: a reviewer reads a one-line diff when IANA adds a row.
- `TestTableMatchesRegistry` parses the three pinned CSV files. It fails when a special-purpose row (other than the five embedded-form rows of step 4) has no table line, or a table line has no registry row. For each such row it probes three addresses inside the row that no protocol rule (steps 1 to 5) covers, chosen by the test from the row's first, middle and last address and moved inward past any covered range, and checks the class is `Deprecated` for a deprecated row and `NonGlobal` for any other, whatever its Globally Reachable column says. The embedded-form rows are tested by `TestEmbedded` with public, private and loopback IPv4 addresses inside them. For every assignments row designated to a regional registry it checks that boundary addresses outside any special row are `Public`, and for every row designated to IANA, 6to4 or documentation that they are not.
- `TestRegistryDates` checks that the three constants match `testdata/iana/DATES`.
- A weekly workflow (`.github/workflows/iana-registries.yml`) downloads the three CSV files with a size limit and compares them with the pinned copies. A difference fails the job and opens an issue that names the registry and the changed rows, with the rows in a fenced block; when an open issue for the same registry exists, the run comments on it instead of opening another. The network call is in CI only; the package makes none.
- Updating means: replace the pinned CSV, update `DATES` and the constants, change the table until the test passes, and say in the pull request which rows changed and what that allows or refuses. A change that newly allows an address is a `feat` (a behaviour change for both products) and is reviewed as a security change; one that only refuses more is a `fix`. Adding a registry row can only refuse more.
- A row IANA adds between the weekly check and an Armory release is refused by step 7 when it lies in undelegated IPv6 space, which is where recent rows went (`3fff::/20`, `5f00::/16`). A new row inside space already delegated to a regional registry, or a new IPv4 row, is allowed until the table is updated. Because no row gives `Public` (step 6), that window is only for new rows marked `False`: a new row marked `True` is refused once added anyway, so updating the table never newly allows an address. The window is accepted.

## Alternatives considered

- Keep a list in each product. No shared release to wait for, but the two copies already differ, each has a known miss, and every registry change is applied twice.
- Allow all of `2000::/3` except listed blocks (Sablecourt's rule). Simpler, but every new special-purpose allocation inside `2000::/3` passes until someone adds it, as `3fff::/20` does today. Failing closed on undelegated space costs a false refusal when IANA delegates a new block to a regional registry; the last one was `2410::/12` on 2024-11-01, and the weekly check reports it.
- Use only the `netip` predicates (`IsGlobalUnicast`, `IsPrivate`, `IsLoopback`). They miss documentation, benchmarking, shared address space, `240.0.0.0/4` and every IPv6 special row, and they say nothing about delegation.
- Refuse every embedded form (Sablecourt today). Simpler to reason about, but a product on an IPv6-only network behind DNS64 cannot reach any IPv4-only provider. The embedded IPv4 address is at a fixed offset in the forms this record reads, so judging it is exact.
- Judge IPv4-compatible addresses by their IPv4 part (Shadewright today). Rejected above: no current use, and refusal is stricter.
- Let `AllowPrivate` also allow a private address inside NAT64. It would widen Shadewright's `PrivateServiceControl`, and RFC 6052 forbids that embedding for non-global addresses.
- Let a special-purpose row marked globally reachable give `Public`, with the most specific row winning. The rows are anycast or service addresses answered by the nearest operator, often the local gateway, and both products refuse their blocks today. Settled while reviewing this record: no row gives `Public`.
- Let `AllowPrivate` win over `Deny`, as Shadewright's `AllowPrivateService` ignores `InCluster`. A shared library with an allow that silently disables a deny is hard to audit; the product that wants the old effect passes no `Deny`.
- Accept any IPv6 `/96` as an operator NAT64 prefix. It lets a configuration error turn link-local, multicast or IPv4-compatible addresses into public ones.
- Generate the table from the CSV with `go generate`. The registry CSV has footnote markers, multi-prefix cells (`192.0.0.170/32, 192.0.0.171/32`) and `N/A` values that need hand rules anyway; a generator would hide those rules in code that runs only on update. The test against the pinned copy gives the same guarantee with a readable table.
- A third-party module. None found that tracks the IANA registries with a date, implements fail-closed IPv6 delegation and has no further dependencies; each product would inherit its update schedule.
- A resolver or a dialer in Armory. A dialer would carry timeouts, keep-alives and proxy policy that differ per product. `Control` plugs into the product's own dialer and `Allow` into its own resolve loop.

## Consequences

- One table for both products, dated and tested against the registries. A missed row is caught by a test, not by a reader of the code.
- The classes, `Policy`, `Refusal`, the limits and the error values are public API. Adding a `Class` value later is a minor change; callers that switch on `Class` need a default branch, and the package doc says so.
- Every widening is a named field with a fixed place in the order, and `Deny` always wins. A product that wants an allow to override a deny leaves the deny out.
- A registry update can change what a product may dial. It reaches a product when the product bumps its Armory version, so a newly refused range waits for both bumps.
- Failing closed on undelegated IPv6 refuses a legitimate host in a block IANA delegates after the pinned date, until the table is updated and released.
- Redirects, proxies, pooled connections and custom TLS dialers stay the product's job; `netguard` checks only the addresses it is shown.
- Metadata services are covered only where their address is link-local; the others need a product `Deny` (see "Limits: metadata services").
- `Control` is for TCP only.
- The weekly workflow adds a scheduled job with network access to the repository, a new file under `.github/workflows/` that is reviewed with `actionlint` and `zizmor`.
- The `netguard` scope is already in `.github/workflows/pr-title.yml`, the issue templates and `AGENTS.md` (pull request 65).

## Tests

Test files are under `netguard/`.

- `TestTableMatchesRegistry` and `TestRegistryDates`, described above.
- `TestOrder`: one case per step of the classification, including `64:ff9b::7f00:1` (`Loopback`, step 4 before the `True` row), `::ffff:8.8.8.8` (`Public`, step 2 before the `False` row), `192.88.99.2` and `192.88.99.1` (both `Deprecated`, inside the deprecated row), and `192.0.0.9` (`NonGlobal`).
- `TestKnownMisses`: `5f00::1`, `3fff::1`, `100:0:0:1::1`, `192.88.99.1`, `192.88.99.2`, `192.0.0.9`, `192.0.0.10`, `192.31.196.1`, `2001:1::1`, `2001:4:112::1` and `2620:4f:8000::1` are not `Public`; `8.8.8.8` and `2001:4860:4860::8888` are.
- `TestEmbedded`: a table over each form in the embedded-address table. `64:ff9b::808:808` is `Public`; `64:ff9b::a00:1` is `Private`; `2002:808:808::1` and `2002:a00:1::1` are `Opaque`; `::ffff:127.0.0.1` is `Loopback`; `::808:808`, `64:ff9b:1::808:808`, `2001:0:4136:e378:8000:63bf:3fff:fdd2` and `::ffff:0:808:808` are `Opaque`; `::` and `::1` keep `Unspecified` and `Loopback`.
- `TestZones`: `fe80::1%eth0`, `2001:4860::1%1`, `::ffff:8.8.8.8%eth0` and `64:ff9b::808:808%eth0` are `Zoned`, and `Control` refuses `[2001:4860::1%1]:443`.
- `TestPolicy`, one case per rule of the precedence list: the zero address and `fe80::1%eth0` are refused, and so is a zoned address inside a configured NAT64 prefix; `Deny` refuses an address and an embedded IPv4 address inside a denied prefix; `Policy{AllowPrivate: true, Deny: [10.0.0.0/8]}` refuses `10.0.0.1`; `AllowPrivate` allows `10.0.0.1`, `::ffff:10.0.0.1` and `fd00::1` and still refuses loopback, link-local, `NonGlobal`, `64:ff9b::a00:1` and `2002:a00:1::1`; under `AllowNAT64Prefixes: [64:ff9b:1:a::/96]`, `64:ff9b:1:a::808:808` is allowed and `64:ff9b:1:a::a00:1` is refused with and without `AllowPrivate`, and the same holds for a prefix inside `fd00::/8`. `Validate` refuses an unmasked prefix, an IPv4-mapped `Deny` prefix, a `/64` or IPv4 NAT64 prefix, NAT64 prefixes at `fe80::/96`, `ff00::/96`, `::/96`, `::ffff:0:0/96`, `64:ff9b::/96`, `2001::/96`, `2002::/96` and `2001:db8:64::/96`, and lists over the limits; it accepts `Deny: [0.0.0.0/0]`.
- `TestControl`: a `net.Dialer` with `Policy{}.Control` and a fake resolver that answers a name with `127.0.0.1` gets `ErrRefused`, and a loopback listener records no accepted connection. For rebinding, a resolver answers the public `8.8.8.8` on the first lookup (not a documentation address, which step 6 refuses) and `127.0.0.1` on the second; the test wraps `Control` to record that the first call returned nil and then stops the dial with a test sentinel before any packet leaves, and the second dial is refused with no connection on the loopback listener. A mixed answer (`8.8.8.8` and `::1`) shows `Control` runs on every attempted address and refuses the loopback one. A non-IP host, an unknown or non-TCP network (`unix`, `ip4:icmp`, `udp`), a missing, zero, signed, non-numeric, leading-zero or out-of-range port, an IPv6 literal on `tcp4`, an address over 80 bytes, and a done context in `ControlContext` are refused. The fake resolver builds DNS answers from standard library code only (invariant 2), with no `golang.org/x/net` import.
- `TestErrorText`: for every error from `Allow`, `Control`, `ParseAddr` and `Validate`, the whole chain (each `Unwrap`) formatted with `%v`, `%+v` and `%#v` contains no address, port, network name or prefix from the input.
- `FuzzParseAddr`: never panics; any accepted string is at most 64 bytes, has no zone, and parses with `netip.ParseAddr` to the same address.
- `FuzzClassify` over 4-byte and 16-byte inputs: never panics; when the class is `Public`, the address (unmapped, or the readable embedded IPv4 address) is not inside private-use, unique-local or a protocol class, it is inside no special-purpose row of the pinned registries, and an IPv6 address with no row lies in regional registry space; an IPv4-mapped address has the class of its IPv4 address.
- `FuzzControl` over `network` and `address` strings: never panics, never allows a string that `Allow` would refuse for the parsed address, and never allows an address over 80 bytes.

## Open questions

1. NAT64 prefix lengths: decided 2026-11-27. `netguard` accepts `/96` prefixes only. Other RFC 6052 lengths (`/32` to `/64`) are added when a product needs them; the change is additive.
2. Weekly registry check: decided 2026-11-27. The workflow opens an issue on a difference. The job that downloads and compares the registries has no write permission. A second job has only `issues: write` on the default token and posts bounded row text. This is decided before the workflow pull request.

## Sources

Read on 2026-11-27.

- The three IANA registries in the table above.
- RFC 6890 (special-purpose registries), RFC 8190 (their Globally Reachable column), RFC 6052 and RFC 8215 (NAT64 prefixes), RFC 3056 and RFC 7526 (6to4 and its deprecation), RFC 4291 (IPv4-compatible and IPv4-mapped forms), RFC 4380 (Teredo), RFC 9602 (SRv6 SIDs), RFC 9637 (`3fff::/20`).
- Shadewright, on `main`: `internal/providers/model/openaiwire/client.go`, `openaicompatible/provider.go` and `declarations.go`, `vertexai/provider.go`, `vertexai/sts/sts.go`, `foundry/foundry.go`. Sablecourt, on `main`: `internal/gate/councilproxy/proxy.go`, and Sablecourt issue 104 (the `3fff::/20` miss).
