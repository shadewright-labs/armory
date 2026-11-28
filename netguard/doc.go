// Package netguard decides whether an IP address is a public unicast address a product may dial when it
// means "the public internet", and refuses the others at dial time.
//
// [Classify] gives every address exactly one [Class], by these steps of ADR-0004, stopping at the first
// that decides:
//
//  1. Not valid: [Invalid]. Carries a zone: [Zoned]. The zone is checked before anything else, so
//     ::ffff:8.8.8.8%eth0 is Zoned and is never unmapped first.
//  2. IPv4-mapped (::ffff:0:0/96): unmapped, and the rest of the steps run on the IPv4 address.
//  3. Protocol classes: [Unspecified], [Loopback], [LinkLocal], and [Multicast] (which includes the IPv4
//     limited broadcast).
//  4. Embedded IPv4 forms. The NAT64 well-known prefix 64:ff9b::/96 takes the class of the IPv4 address
//     in its last 32 bits. 6to4 (2002::/16), IPv4-compatible (::/96), local-use NAT64 (64:ff9b:1::/48),
//     Teredo (2001::/32) and SIIT (::ffff:0:0:0/96) are [Opaque].
//  5. Private-use IPv4 and unique-local IPv6: [Private].
//  6. Any row of the IANA IPv4 and IPv6 Special-Purpose Address Registries: [Deprecated] for a
//     deprecated row, [NonGlobal] for any other. No row gives Public, whatever its Globally Reachable
//     column says.
//  7. IPv6 inside a prefix the IANA IPv6 Global Unicast Address Assignments registry designates to a
//     regional internet registry is [Public], other IPv6 is [Undelegated], other IPv4 is [Public].
//
// Step 7 fails closed for IPv6: a block IANA has not delegated is not public, so a new special-purpose
// allocation in undelegated space is refused before the table learns about it. The table follows the
// registry dates in [IPv4SpecialUpdated], [IPv6SpecialUpdated] and [IPv6UnicastUpdated]; a test ties it to
// pinned copies of the registries row for row.
//
// Callers that switch on [Class] need a default branch: a later minor version may add a value.
//
// A [Policy] is a product's dial rule. The zero Policy allows Public addresses only. [Policy.Deny] adds
// prefixes to refuse and wins over both widening fields, [Policy.AllowPrivate] allows an address that is
// itself private-use or unique-local, and [Policy.AllowNAT64Prefixes] names operator NAT64 prefixes whose
// embedded IPv4 address is judged. [Policy.Allow] checks one address; [Policy.Control] and
// [Policy.ControlContext] plug into a net.Dialer, so the check runs on the address the dialer resolved,
// for every address it tries and every new connection. A name that resolves to a public address on one
// lookup and to 127.0.0.1 on the next is refused on the connection that would reach 127.0.0.1.
//
// The package has no resolver and makes no network calls. It does not cover pooled connections (Control
// runs only when a connection is dialled), redirects to another public host, proxies (Control sees the
// proxy, not the target) or a custom DialTLSContext that bypasses the guarded dialer: those stay with the
// caller's transport. Cloud metadata services are covered only where their address is link-local
// (169.254.169.254); others, such as 168.63.129.16 or a unique-local address, need a Deny prefix.
//
// Every refusal is a [*Refusal] and matches [ErrRefused]. Error text is fixed: it never contains the
// address, the port, the network name or a prefix, so it is safe to log. [ParseAddr] is the strict entry
// point for untrusted strings.
package netguard
