# ADR-0004: Package `netguard`, which IP addresses count as public and how the table stays current

- Status: Proposed
- Date: 2026-11-27

This record decides the classification rule of the planned `netguard` package (epic 49, issue 50): which addresses a product may dial when it means "the public internet", how addresses that embed an IPv4 address are judged, how the guard runs at dial time, and how the range table is traced to the IANA registries and kept current. No code exists yet.

## Context

Shadewright and Sablecourt both refuse to send a credential or a request body to an address that is not public: a model endpoint or a provider host that resolves to a cluster, a metadata service or a loopback port must get nothing. Each product keeps its own range list, and the two lists differ.

- Shadewright, `internal/providers/model/openaiwire/client.go`: `publicAddress`, the `Guard` type and its `Control` dial hook, and `PrivateServiceControl` for an explicitly named in-cluster Service. It judges NAT64 (`64:ff9b::/96`), 6to4 (`2002::/16`) and IPv4-compatible (`::/96`) addresses by the IPv4 address inside them, refuses Teredo, local-use NAT64 and SIIT outright, and lists `3fff::/20`. It relies on `netip.Addr.IsGlobalUnicast` for IPv6, which is true for any address outside a few fixed blocks, so `5f00::/16` (SRv6 SIDs, added to the registry in 2024) and `100:0:0:1::/64` (dummy prefix, added in 2025) pass. It refuses all of `192.0.0.0/24` and `2001::/23`, including the rows inside them that the registry marks globally reachable.
- Sablecourt, `internal/gate/councilproxy/proxy.go`: `specialAddresses` (11 prefixes) and `publicAddress`. It allows every IPv6 address inside `2000::/3` that is not listed, so documentation prefix `3fff::/20` passes, and it omits `192.88.99.0/24` (deprecated 6to4 relay anycast). It refuses NAT64 and 6to4 outright, so a host on an IPv6-only network behind DNS64 cannot reach a provider. It resolves the name, checks every address and then dials the checked IP literal, so a second lookup cannot swap the address.

The in-cluster range checks in Shadewright (`openaicompatible/declarations.go`, `vertexai/sts/sts.go`, and `Guard.InCluster`) are product policy and stay in Shadewright as arguments.

Two lists that already disagree, each with a known miss, are the reason this belongs in Armory: one table, traced to the registries with a date, and one test that fails when a registry row is missing.

Invariants touched: 1 (the package knows no product's ranges; extra prefixes are arguments), 2 (standard library only: `net/netip`, `net`, `syscall`), 3 (no network calls and no resolver; error text never quotes an address), 4 (the string entry point is bounded and fuzzed) and 5 (the classes and the policy type become public API).

## Decision

### Sources

The table follows three IANA registries, read on 2026-11-27:

| Registry | Last updated | Pinned copy |
| --- | --- | --- |
| [IANA IPv4 Special-Purpose Address Registry](https://www.iana.org/assignments/iana-ipv4-special-registry/) | 2025-10-09 | `netguard/testdata/iana/iana-ipv4-special-registry-1.csv` |
| [IANA IPv6 Special-Purpose Address Registry](https://www.iana.org/assignments/iana-ipv6-special-registry/) | 2025-10-09 | `netguard/testdata/iana/iana-ipv6-special-registry-1.csv` |
| [IPv6 Global Unicast Address Assignments](https://www.iana.org/assignments/ipv6-unicast-address-assignments/) | 2025-10-10 | `netguard/testdata/iana/ipv6-unicast-address-assignments.csv` |

The pinned copies are the CSV files IANA publishes, committed unchanged, with the "Last updated" date of each recorded in `netguard/testdata/iana/DATES` and in exported constants.

### What "public" means

An address is public when all of these hold:

1. It is valid and carries no zone.
2. It is unicast: not unspecified, loopback, link-local, multicast or the IPv4 limited broadcast.
3. It is not private-use (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`) or unique-local (`fc00::/7`).
4. The most specific special-purpose row that contains it, if any, has Globally Reachable `True`. A more specific row overrides a wider one: `192.0.0.9/32` (PCP anycast, `True`) is public although `192.0.0.0/24` is `False`.
5. Rows IANA marks deprecated (a termination date and empty attributes: `192.88.99.0/24`, `2001:10::/28`) are refused, with every address inside them. `192.88.99.2/32` (6a44 relay, `False`) is refused either way.
6. For IPv6 with no special-purpose row: the address lies inside a prefix the unicast assignments registry designates to a regional internet registry (AFRINIC, APNIC, ARIN, LACNIC, RIPE NCC). Everything else fails closed, so a block IANA has not delegated (`3000::/5`, `3fff::/20` before its row existed, `5f00::/16`, all of `4000::/3` and beyond) is not public by default. A future special-purpose allocation in undelegated space is refused before the table learns about it.

IPv4 has no step 6: the IPv4 space outside the special rows is fully allocated, and `240.0.0.0/4` has its own row.

Rows with Globally Reachable `N/A` are handled by the embedded-address rules below: Teredo (`2001::/32`) is refused and 6to4 (`2002::/16`) is judged by its IPv4 address.

### Addresses that embed an IPv4 address

| Form | Prefix | Rule |
| --- | --- | --- |
| IPv4-mapped | `::ffff:0:0/96` | Unmapped and judged as the IPv4 address. This is what the kernel dials. |
| NAT64 well-known prefix (RFC 6052) | `64:ff9b::/96` | Judged by the IPv4 address in the last 32 bits. A DNS64 answer for a public host connects, one for `10.0.0.1` does not. |
| 6to4 (RFC 3056) | `2002::/16` | Judged by the IPv4 address in bits 16 to 47. |
| NAT64 network-specific prefix | given by the caller, `/96` only | Judged by the IPv4 address in the last 32 bits, like the well-known prefix (see `Policy.NAT64`). |
| IPv4-compatible (deprecated by RFC 4291) | `::/96`, except `::` and `::1` | Refused. |
| Local-use NAT64 (RFC 8215) | `64:ff9b:1::/48` | Refused: the embedding is set by the local operator and cannot be read reliably. |
| Teredo (RFC 4380) | `2001::/32` | Refused: the embedded IPv4 is obfuscated and the server address is a second embedded address. |
| SIIT IPv4-translated | `::ffff:0:0:0/96` | Refused. |

Shadewright judges IPv4-compatible addresses by their IPv4 part. This record refuses them: the form was deprecated in 2006, nothing current uses it, and where the packet goes depends on tunnel configuration of the host. Refusing it is the stricter choice and costs nothing known.

An operator NAT64 that uses a network-specific prefix makes any IPv4 address reachable through ordinary-looking IPv6 addresses. `netguard` cannot recognise such a prefix on its own; a product running in that network names it in `Policy.NAT64`, or the private IPv4 space behind it is reachable. Only `/96` prefixes are accepted, because the other RFC 6052 lengths split the IPv4 address around the reserved `u` octet.

### Public API

```go
package netguard

// Class is the category of an address. The zero value is Invalid.
type Class uint8

const (
	Invalid     Class = iota // not a valid address
	Public                   // public unicast: may be dialled as the public internet
	Private                  // private-use IPv4 or unique-local IPv6
	Loopback
	LinkLocal
	Multicast                // also the IPv4 limited broadcast
	Unspecified
	NonGlobal                // in a special-purpose row with Globally Reachable False
	Deprecated               // in a deprecated special-purpose row
	Undelegated              // IPv6 outside the space IANA has designated to a regional registry
	Opaque                   // embeds an IPv4 address that cannot be read reliably
	Zoned                    // carries a zone, such as fe80::1%eth0
)

func (c Class) String() string

// Classify returns the class of ip. A readable embedded IPv4 address gives the class of that address.
func Classify(ip netip.Addr) Class

// Public reports whether Classify(ip) is Public.
func Public(ip netip.Addr) bool

// ParseAddr parses a textual IP address strictly: at most 64 bytes, no zone, no leading zeros in an
// IPv4 part, no shortened or numeric IPv4 forms (127.1, 0x7f000001). It is the entry point for
// untrusted strings.
func ParseAddr(s string) (netip.Addr, error)

// Policy is a product's dial rule. The zero Policy allows Public addresses only.
type Policy struct {
	// AllowPrivate also allows Private addresses, for a service the product has named explicitly.
	AllowPrivate bool
	// Deny lists further prefixes to refuse, such as pod or Service ranges. An address is refused when
	// it, or the IPv4 address embedded in it, is inside one. At most 1024 prefixes.
	Deny []netip.Prefix
	// NAT64 lists the operator's network-specific NAT64 prefixes. Each is an IPv6 /96. At most 16.
	NAT64 []netip.Prefix
}

// Validate refuses a Policy with an invalid or unmasked prefix, a NAT64 prefix that is not an IPv6 /96,
// or more prefixes than the limits above.
func (p Policy) Validate() error

// Allow returns nil when p allows ip, else an error that matches ErrRefused.
func (p Policy) Allow(ip netip.Addr) error

// Control is a net.Dialer Control function. It allows a connection only when the network is tcp, tcp4,
// tcp6, udp, udp4 or udp6 and the address is an IP literal and port that p allows.
func (p Policy) Control(network, address string, c syscall.RawConn) error

// ControlContext is Control for net.Dialer.ControlContext.
func (p Policy) ControlContext(ctx context.Context, network, address string, c syscall.RawConn) error

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

`Allow` and `Control` call `Validate` and refuse when it fails. The error of `Allow` wraps the class (`errors.As` into `*Refusal{Class}`) and never contains the address, the port or the network name.

### Dial-time checking and DNS rebinding

`netguard` has no resolver and reads no name. The check belongs at dial time: `net.Dialer` calls `Control` with the resolved IP literal of each connection attempt, after resolution and before connect, for every address of a name (both families under Happy Eyeballs) and again for every new connection. A name that resolves to a public address on one lookup and to `127.0.0.1` on the next is refused on the connection that would reach `127.0.0.1`.

Checking the address when the name is resolved and then dialling the name again is not safe: the second lookup can return a different address. A product either dials through a `net.Dialer` whose `Control` is `Policy.Control`, or does what Sablecourt does today: resolve, call `Allow` on every address, and dial only the checked IP literal. `Control` is the recommended form because it also covers redirects, retries and connection reuse that the product's own code does not see.

`Control` checks the address the process connects to. Through an HTTP proxy that is the proxy, not the target, so a product that relies on the guard sets no proxy on that transport (Shadewright already does).

## Adoption

### Shadewright 0.8.0

`openaiwire.publicAddress`, `reservedIPv4`, `reservedIPv6` and the embedded-form code are removed. `Guard{InCluster: p}` becomes `netguard.Policy{Deny: p}` and `PrivateServiceControl` becomes `Policy{AllowPrivate: true}`. The in-cluster lists in `openaicompatible` and `vertexai/sts` are passed as `Deny`. What changes for Shadewright's users:

- Newly refused: `5f00::/16`, `100:0:0:1::/64`, IPv6 in undelegated space (for example `3000::/5`), and IPv4-compatible addresses with a public IPv4 part.
- Newly allowed: the globally reachable rows inside `192.0.0.0/24` (`192.0.0.9/32`, `192.0.0.10/32`) and inside `2001::/23` (`2001:1::1/128` to `2001:1::3/128`, `2001:3::/32`, `2001:4:112::/48`, `2001:20::/28`, `2001:30::/28`).
- A cluster on an IPv6-only network with an operator NAT64 prefix sets it in the chart so it reaches `Policy.NAT64`. That is a new chart value and needs its own Shadewright issue.

The Shadewright pull request lists these differences in its description and adds a test per difference.

### Sablecourt after 0.1.0

`councilproxy.publicAddress` and `specialAddresses` are replaced by `netguard.Policy{}.Allow`, or by `Control` on the proxy's dialer. What changes:

- Newly refused: `3fff::/20`, `192.88.99.0/24`, IPv6 in undelegated space inside `2000::/3`.
- Newly allowed: NAT64 and 6to4 addresses whose IPv4 address is public, and the globally reachable rows inside `192.0.0.0/24` and `2001::/23`. If Sablecourt wants to keep refusing NAT64 and 6to4 outright it passes `64:ff9b::/96` and `2002::/16` in `Deny`; that choice is Sablecourt's and is recorded on its own issue.
- Its proxy table tests keep running against the new call, and a test for `3fff::1` and `192.88.99.1` is added first, failing on the current code.

## How the table is kept current

- The table is hand-written Go (`netguard/table.go`), one line per registry row with its name and RFC in a comment. It is not generated: a reviewer reads a one-line diff when IANA adds a row.
- `TestTableMatchesRegistry` parses the three pinned CSV files. For every special-purpose row it checks the first, last and a middle address of the block against the class the row implies, and it fails when a row of the pinned registry has no table line or a table line has no registry row. For every assignments row designated to a regional registry it checks that the boundary addresses are `Public`, and for every row designated to IANA, 6to4 or documentation that they are not.
- `TestRegistryDates` checks that the three constants match `testdata/iana/DATES`.
- A weekly workflow (`.github/workflows/iana-registries.yml`) downloads the three CSV files and compares them with the pinned copies. A difference fails the job and opens an issue that names the registry and the changed rows. The network call is in CI only; the package makes none.
- Updating means: replace the pinned CSV, update `DATES` and the constants, change the table until the test passes, and say in the pull request which rows changed and what that allows or refuses. A change that newly allows an address is a `feat` (a behaviour change for both products) and is reviewed as a security change; one that only refuses more is a `fix`.
- A row IANA adds between the weekly check and an Armory release is refused by step 6 when it lies in undelegated IPv6 space, which is where recent rows went (`3fff::/20`, `5f00::/16`). A new row inside space already delegated to a regional registry, or a new IPv4 row, is allowed until the table is updated. That window is accepted.

## Alternatives considered

- Keep a list in each product. No shared release to wait for, but the two copies already differ, each has a known miss, and every registry change is applied twice.
- Allow all of `2000::/3` except listed blocks (Sablecourt's rule). Simpler, but every new special-purpose allocation inside `2000::/3` passes until someone adds it, as `3fff::/20` does today. Failing closed on undelegated space costs a false refusal when IANA delegates a new block to a regional registry; the last one was `2410::/12` on 2024-11-01, and the weekly check reports it.
- Use only the `netip` predicates (`IsGlobalUnicast`, `IsPrivate`, `IsLoopback`). They miss documentation, benchmarking, shared address space, `240.0.0.0/4` and every IPv6 special row, and they say nothing about delegation.
- Refuse every embedded form (Sablecourt today). Simpler to reason about, but a product on an IPv6-only network behind DNS64 cannot reach any IPv4-only provider. The embedded IPv4 address is at a fixed offset in the forms this record reads, so judging it is exact.
- Judge IPv4-compatible addresses by their IPv4 part (Shadewright today). Rejected above: no current use, and refusal is stricter.
- Generate the table from the CSV with `go generate`. The registry CSV has footnote markers, multi-prefix cells (`192.0.0.170/32, 192.0.0.171/32`) and `N/A` values that need hand rules anyway; a generator would hide those rules in code that runs only on update. The test against the pinned copy gives the same guarantee with a readable table.
- A third-party module. None found that tracks the IANA registries with a date, implements fail-closed IPv6 delegation and has no further dependencies; each product would inherit its update schedule.
- A resolver or a dialer in Armory. A dialer would carry timeouts, keep-alives and proxy policy that differ per product. `Control` plugs into the product's own dialer and is all the guard needs.

## Consequences

- One table for both products, dated and tested against the registries. A missed row is caught by a test, not by a reader of the code.
- The classes, `Policy`, its limits and the error values are public API. Adding a `Class` value later is a minor change; callers that switch on `Class` need a default branch, and the package doc says so.
- A registry update can change what a product may dial. It reaches a product when the product bumps its Armory version, so a newly refused range waits for both bumps.
- Failing closed on undelegated IPv6 refuses a legitimate host in a block IANA delegates after the pinned date, until the table is updated and released.
- The weekly workflow adds a scheduled job with network access to the repository, a new file under `.github/workflows/` that is reviewed with `actionlint` and `zizmor`.
- The first code pull request adds the `netguard` scope to `.github/workflows/pr-title.yml`, the issue templates and `AGENTS.md`.

## Tests

Test files are under `netguard/`.

- `TestTableMatchesRegistry` and `TestRegistryDates`, described above.
- `TestKnownMisses`: `5f00::1`, `3fff::1`, `100:0:0:1::1`, `192.88.99.1` and `192.88.99.2` are not `Public`; `192.0.0.9`, `192.31.196.1`, `2001:4:112::1` and `2620:4f:8000::1` are.
- `TestEmbedded`: a table over each form in the embedded-address table. `64:ff9b::808:808` and `2002:808:808::1` are `Public`; `64:ff9b::a00:1` and `2002:a00:1::1` are `Private`; `::ffff:127.0.0.1` is `Loopback`; `::808:808`, `64:ff9b:1::808:808`, `2001:0:4136:e378:8000:63bf:3fff:fdd2` and `::ffff:0:808:808` are `Opaque`; `::` and `::1` keep `Unspecified` and `Loopback`.
- `TestZones`: `fe80::1%eth0` and `2001:4860::1%1` are `Zoned`, and `Control` refuses `[2001:4860::1%1]:443`.
- `TestPolicy`: `Deny` refuses an address and an embedded IPv4 address inside a denied prefix; `AllowPrivate` allows `10.0.0.1` and `fd00::1` and still refuses loopback, link-local and `NonGlobal`; `NAT64` judges `2001:db8:64::a00:1` under a configured `2001:db8:64::/96` as `Private`; `Validate` refuses an unmasked prefix, a `/64` NAT64 prefix, an IPv4 NAT64 prefix and lists over the limits.
- `TestControl`: a `net.Dialer` with `Policy{}.Control` and a fake resolver that answers a name with `127.0.0.1` gets `ErrRefused`, and a loopback listener records no accepted connection. A resolver that answers `203.0.113.7` then `127.0.0.1` for the same name is refused on the connection to `127.0.0.1` (rebinding). A non-IP host, an unknown network (`unix`, `ip4:icmp`) and a missing port are refused.
- `TestErrorText`: no error from `Allow`, `Control`, `ParseAddr` or `Validate` contains the address, the port or the network name.
- `FuzzParseAddr`: never panics; any accepted string is at most 64 bytes, has no zone, and parses with `netip.ParseAddr` to the same address. `FuzzClassify` over 16-byte inputs: `Public` implies `IsGlobalUnicast`, not `IsPrivate`, and not inside any `False` or deprecated row of the pinned registries; an IPv4-mapped address has the class of its IPv4 address.

## Open questions

1. Should rows that are globally reachable but are identifiers rather than locators (`2001:20::/28` ORCHIDv2, `2001:30::/28` DETs) be public? This record follows the registry and allows them; no product dials them on purpose.
2. Should `netguard` accept network-specific NAT64 prefixes of the other RFC 6052 lengths (`/32` to `/64`)? Only `/96` is accepted until a product needs another length.
3. Should the weekly registry check open an issue or only fail its job? The record says it opens one; that needs an issue-write token on a scheduled workflow.

## Sources

Read on 2026-11-27.

- The three IANA registries in the table above.
- RFC 6890 (special-purpose registries), RFC 8190 (their Globally Reachable column), RFC 6052 and RFC 8215 (NAT64 prefixes), RFC 3056 and RFC 7526 (6to4 and its deprecation), RFC 4291 (IPv4-compatible and IPv4-mapped forms), RFC 4380 (Teredo), RFC 9602 (SRv6 SIDs), RFC 9637 (`3fff::/20`).
- Shadewright, on `main`: `internal/providers/model/openaiwire/client.go`. Sablecourt, on `main`: `internal/gate/councilproxy/proxy.go`, and Sablecourt issue 104 (the `3fff::/20` miss).
