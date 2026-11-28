# netguard

Package `netguard` (`github.com/shadewright-labs/armory/netguard`) decides whether an IP address is a public unicast address that a product may dial when it means "the public internet", and refuses the others when a connection is dialled. It has no resolver, makes no network calls and does not log. The rule is set by [ADR-0004](../adr/0004-netguard-public-addresses.md).

## Classes

`Classify(ip netip.Addr) Class` gives every address exactly one class. It applies these steps in order and stops at the first that decides:

1. An invalid address is `Invalid`. An address with a zone is `Zoned`, checked before anything else, so `::ffff:8.8.8.8%eth0` is `Zoned`.
2. An IPv4-mapped address (`::ffff:0:0/96`) is unmapped, and the remaining steps run on the IPv4 address.
3. Protocol classes: `Unspecified` (`0.0.0.0`, `::`), `Loopback` (`127.0.0.0/8`, `::1`), `LinkLocal` (`169.254.0.0/16`, `fe80::/10`) and `Multicast` (`224.0.0.0/4`, `ff00::/8` and `255.255.255.255`).
4. Addresses that embed an IPv4 address, in the table below.
5. Private-use IPv4 (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`) and unique-local IPv6 (`fc00::/7`) are `Private`.
6. An address in any row of the IANA IPv4 or IPv6 Special-Purpose Address Registry is `Deprecated` for a deprecated row (`192.88.99.0/24`, `2001:10::/28`) and `NonGlobal` for any other. No row gives `Public`, including the rows the registry marks globally reachable, such as PCP anycast `192.0.0.9` or AS112 `192.31.196.0/24`.
7. IPv6 inside a prefix that IANA has designated to a regional internet registry (AFRINIC, APNIC, ARIN, LACNIC, RIPE NCC) is `Public`, any other IPv6 address is `Undelegated`, and any other IPv4 address is `Public`.

Step 7 fails closed for IPv6. Space IANA has not delegated, such as `3000::/5` or `4000::/3`, is not public, so a new special-purpose block in that space is refused before the table lists it.

`IsPublic(ip)` reports whether `Classify(ip)` is `Public`. `Class.String` gives the lower-case name (`public`, `nonglobal`). A `switch` on `Class` needs a `default` branch, because a later minor version may add a class.

### Embedded IPv4 addresses

| Form | Prefix | Class |
| --- | --- | --- |
| IPv4-mapped | `::ffff:0:0/96` | The class of the IPv4 address (step 2) |
| NAT64 well-known prefix (RFC 6052) | `64:ff9b::/96` | The class of the IPv4 address in the last 32 bits |
| NAT64 network-specific prefix | named in `Policy.AllowNAT64Prefixes` | Judged by `Policy.Allow` only; `Classify` does not know it |
| 6to4 (RFC 3056) | `2002::/16` | `Opaque` |
| IPv4-compatible (RFC 4291) | `::/96`, except `::` and `::1` | `Opaque` |
| Local-use NAT64 (RFC 8215) | `64:ff9b:1::/48` | `Opaque` |
| Teredo (RFC 4380) | `2001::/32` | `Opaque` |
| SIIT IPv4-translated | `::ffff:0:0:0/96` | `Opaque` |

So `64:ff9b::808:808` (a DNS64 answer for `8.8.8.8`) is `Public` and `64:ff9b::a00:1` (for `10.0.0.1`) is `Private`.

## Policy

A `Policy` is a product's dial rule. The zero `Policy` allows `Public` addresses only.

- `Deny []netip.Prefix` (at most `MaxDeny`, 1024): prefixes to refuse, such as pod or Service ranges or a metadata service address. An address is refused when it, or an IPv4 address readable inside it (IPv4-mapped, well-known NAT64, or an operator NAT64 prefix), is inside one. `Deny` wins over both fields below.
- `AllowPrivate bool`: also allows an address that is itself private-use IPv4 or unique-local IPv6, for a service the product has named. It never allows a private IPv4 address embedded in NAT64.
- `AllowNAT64Prefixes []netip.Prefix` (at most `MaxNAT64Prefixes`, 16): the operator's network-specific NAT64 prefixes, IPv6 `/96` only. An address inside one is allowed when the IPv4 address in its last 32 bits is `Public`, and refused otherwise, even with `AllowPrivate`.

`Policy.Allow(ip)` applies these rules in order and stops at the first that decides:

1. The policy does not validate: refused.
2. The address is invalid or zoned: refused.
3. The address, or an IPv4 address readable inside it, is inside a `Deny` prefix: refused.
4. The address is inside an `AllowNAT64Prefixes` prefix: allowed only when the embedded IPv4 address is `Public`.
5. The class is `Public`: allowed.
6. The class is `Private`, `AllowPrivate` is set and the address itself (after unmapping) is private-use or unique-local: allowed.
7. Anything else: refused.

`Policy.Validate` refuses a prefix that is invalid or has host bits set, an IPv4-mapped `Deny` prefix (addresses are unmapped before the check, so write `10.0.0.0/8`), a list over its limit, and a NAT64 prefix that is not an IPv6 `/96` inside `64:ff9b:1::/48`, inside `fc00::/7`, or inside regional registry space with no special-purpose row overlapping it. `Allow`, `Control` and `ControlContext` run `Validate` first, so a product that never calls it still fails closed.

## Dialling

`Policy.Control` is a `net.Dialer` `Control` function, and `Policy.ControlContext` the same for `ControlContext`:

```go
d := &net.Dialer{Timeout: 10 * time.Second, Control: netguard.Policy{}.Control}
```

The dialer calls it with the IP literal it resolved, after resolution and before it connects, for every address it tries and for every new connection. A name that resolves to a public address on one lookup and to `127.0.0.1` on the next is refused on the connection that would reach `127.0.0.1`. `Control` allows only `tcp`, `tcp4` and `tcp6`, an address of at most 80 bytes whose host is an IP literal of the network's family with no zone, and a decimal port from 1 to 65535 with no sign or leading zero. `ControlContext` returns `ctx.Err()` unchanged when the context is already done.

A product that resolves names itself resolves with a trailing dot, calls `Allow` on every returned address, refuses the whole request when any address is refused, and dials only a checked literal. `Control` alone refuses only the bad addresses of a mixed answer and still connects to a public one.

`Control` does not cover these, and each product handles them in its own transport:

- A pooled keep-alive connection is not dialled again. A product that changes its `Policy` closes the idle connections of transports built with the old one.
- A redirect to another public host passes `Control`. A client that sends a credential disables redirects.
- Through a proxy, `Control` sees the proxy. A transport that relies on the guard sets `Proxy` to `nil`, because `http.DefaultTransport` reads proxy settings from the environment.
- A `DialTLSContext` on `http.Transport` replaces `DialContext`, so it must dial through the guarded `net.Dialer`.

Cloud metadata services are covered only where their address is link-local (`169.254.169.254`). `168.63.129.16` is `Public`, and the unique-local metadata addresses pass `AllowPrivate`; a product passes them in `Deny`.

## Errors

Every refusal is a `*Refusal` that matches `ErrRefused`, with the address's `Class` and whether `Deny` matched. A refusal because the policy does not validate also matches `ErrInvalidPolicy`, and a malformed network or address passed to `Control` also matches `ErrInvalidAddr`. `Validate` returns an error that matches `ErrInvalidPolicy`.

`ParseAddr(s string)` is the strict entry point for untrusted strings: at most 64 bytes, no zone, brackets, port or host name, and no shortened or numeric IPv4 forms such as `127.1` or `0x7f000001`. Its error matches `ErrInvalidAddr`.

Error text is fixed. It never contains the address, the port, the network name or a prefix, so it is safe to log.

## How the table stays current

The table in `netguard/table.go` is hand-written, one line per registry row with its name and RFC. Copies of the three IANA registries it follows are pinned under `netguard/testdata/iana`, with their dates in `DATES` and in the constants `IPv4SpecialUpdated`, `IPv6SpecialUpdated` and `IPv6UnicastUpdated`:

| Registry | Last updated |
| --- | --- |
| [IANA IPv4 Special-Purpose Address Registry](https://www.iana.org/assignments/iana-ipv4-special-registry/) | 2025-10-09 |
| [IANA IPv6 Special-Purpose Address Registry](https://www.iana.org/assignments/iana-ipv6-special-registry/) | 2025-10-09 |
| [IPv6 Global Unicast Address Assignments](https://www.iana.org/assignments/ipv6-unicast-address-assignments/) | 2025-10-10 |

`TestTableMatchesRegistry` fails when a registry row has no table line or a table line has no row, and probes each row and each regional registry prefix. `TestRegistryBoundaries` compares `Classify` with a reference built from the pinned files on the first, middle and last address of every row and on the address just outside each end. The weekly workflow `iana-registries.yml` compares the live registries with the pinned copies and opens an issue when one changes (see [CI](../ci.md#scheduled-runs)).
