package netguard

import "net/netip"

// Dates of the IANA registries the table follows: the "Last updated" date of each pinned copy under
// testdata/iana.
const (
	// IPv4SpecialUpdated is the date of the IANA IPv4 Special-Purpose Address Registry the table follows.
	IPv4SpecialUpdated = "2025-10-09"
	// IPv6SpecialUpdated is the date of the IANA IPv6 Special-Purpose Address Registry the table follows.
	IPv6SpecialUpdated = "2025-10-09"
	// IPv6UnicastUpdated is the date of the IANA IPv6 Global Unicast Address Assignments registry the
	// regional registry prefixes follow.
	IPv6UnicastUpdated = "2025-10-10"
)

// row is one line of the special-purpose table.
type row struct {
	prefix     netip.Prefix
	deprecated bool
}

// specialRows is step 6 of ADR-0004: one line per row of the IANA IPv4 and IPv6 Special-Purpose Address
// Registries (dates above), in registry order, with the row name and RFC. A cell that lists two prefixes
// gives two lines. The second field marks a deprecated row (a termination date and empty attributes). The
// rows that steps 3 and 5 decide first (loopback, link-local, private-use) are listed too, so the table
// matches the registry row for row. TestTableMatchesRegistry fails when a line is missing or extra.
var specialRows = []row{
	// IPv4 Special-Purpose Address Registry.
	{netip.MustParsePrefix("0.0.0.0/8"), false},          // This network, RFC 791, Section 3.2
	{netip.MustParsePrefix("0.0.0.0/32"), false},         // This host on this network, RFC 1122, Section 3.2.1.3
	{netip.MustParsePrefix("10.0.0.0/8"), false},         // Private-Use, RFC 1918
	{netip.MustParsePrefix("100.64.0.0/10"), false},      // Shared Address Space, RFC 6598
	{netip.MustParsePrefix("127.0.0.0/8"), false},        // Loopback, RFC 1122, Section 3.2.1.3
	{netip.MustParsePrefix("169.254.0.0/16"), false},     // Link Local, RFC 3927
	{netip.MustParsePrefix("172.16.0.0/12"), false},      // Private-Use, RFC 1918
	{netip.MustParsePrefix("192.0.0.0/24"), false},       // IETF Protocol Assignments, RFC 6890, Section 2.1
	{netip.MustParsePrefix("192.0.0.0/29"), false},       // IPv4 Service Continuity Prefix, RFC 7335
	{netip.MustParsePrefix("192.0.0.8/32"), false},       // IPv4 dummy address, RFC 7600
	{netip.MustParsePrefix("192.0.0.9/32"), false},       // Port Control Protocol Anycast, RFC 7723
	{netip.MustParsePrefix("192.0.0.10/32"), false},      // Traversal Using Relays around NAT Anycast, RFC 8155
	{netip.MustParsePrefix("192.0.0.170/32"), false},     // NAT64/DNS64 Discovery, RFC 8880; RFC 7050
	{netip.MustParsePrefix("192.0.0.171/32"), false},     // NAT64/DNS64 Discovery, RFC 8880; RFC 7050
	{netip.MustParsePrefix("192.0.2.0/24"), false},       // Documentation (TEST-NET-1), RFC 5737
	{netip.MustParsePrefix("192.31.196.0/24"), false},    // AS112-v4, RFC 7535
	{netip.MustParsePrefix("192.52.193.0/24"), false},    // AMT, RFC 7450
	{netip.MustParsePrefix("192.88.99.0/24"), true},      // Deprecated (6to4 Relay Anycast), RFC 7526
	{netip.MustParsePrefix("192.88.99.2/32"), false},     // 6a44-relay anycast address, RFC 6751
	{netip.MustParsePrefix("192.168.0.0/16"), false},     // Private-Use, RFC 1918
	{netip.MustParsePrefix("192.175.48.0/24"), false},    // Direct Delegation AS112 Service, RFC 7534
	{netip.MustParsePrefix("198.18.0.0/15"), false},      // Benchmarking, RFC 2544
	{netip.MustParsePrefix("198.51.100.0/24"), false},    // Documentation (TEST-NET-2), RFC 5737
	{netip.MustParsePrefix("203.0.113.0/24"), false},     // Documentation (TEST-NET-3), RFC 5737
	{netip.MustParsePrefix("240.0.0.0/4"), false},        // Reserved, RFC 1112, Section 4
	{netip.MustParsePrefix("255.255.255.255/32"), false}, // Limited Broadcast, RFC 8190; RFC 919, Section 7

	// IPv6 Special-Purpose Address Registry. Not table lines (step 4): ::ffff:0:0/96 (IPv4-mapped),
	// 64:ff9b::/96 and 64:ff9b:1::/48 (IPv4-IPv6 translation), 2001::/32 (Teredo), 2002::/16 (6to4).
	{netip.MustParsePrefix("::1/128"), false},           // Loopback Address, RFC 4291
	{netip.MustParsePrefix("::/128"), false},            // Unspecified Address, RFC 4291
	{netip.MustParsePrefix("100::/64"), false},          // Discard-Only Address Block, RFC 6666
	{netip.MustParsePrefix("100:0:0:1::/64"), false},    // Dummy IPv6 Prefix, RFC 9780
	{netip.MustParsePrefix("2001::/23"), false},         // IETF Protocol Assignments, RFC 2928
	{netip.MustParsePrefix("2001:1::1/128"), false},     // Port Control Protocol Anycast, RFC 7723
	{netip.MustParsePrefix("2001:1::2/128"), false},     // Traversal Using Relays around NAT Anycast, RFC 8155
	{netip.MustParsePrefix("2001:1::3/128"), false},     // DNS-SD Service Registration Protocol Anycast, RFC 9665
	{netip.MustParsePrefix("2001:2::/48"), false},       // Benchmarking, RFC 5180; RFC Errata 1752
	{netip.MustParsePrefix("2001:3::/32"), false},       // AMT, RFC 7450
	{netip.MustParsePrefix("2001:4:112::/48"), false},   // AS112-v6, RFC 7535
	{netip.MustParsePrefix("2001:10::/28"), true},       // Deprecated (previously ORCHID), RFC 4843
	{netip.MustParsePrefix("2001:20::/28"), false},      // ORCHIDv2, RFC 7343
	{netip.MustParsePrefix("2001:30::/28"), false},      // Drone Remote ID Protocol Entity Tags (DETs), RFC 9374
	{netip.MustParsePrefix("2001:db8::/32"), false},     // Documentation, RFC 3849
	{netip.MustParsePrefix("2620:4f:8000::/48"), false}, // Direct Delegation AS112 Service, RFC 7534
	{netip.MustParsePrefix("3fff::/20"), false},         // Documentation, RFC 9637
	{netip.MustParsePrefix("5f00::/16"), false},         // Segment Routing (SRv6) SIDs, RFC 9602
	{netip.MustParsePrefix("fc00::/7"), false},          // Unique-Local, RFC 4193; RFC 8190
	{netip.MustParsePrefix("fe80::/10"), false},         // Link-Local Unicast, RFC 4291
}

// regionalPrefixes is step 7 of ADR-0004: every prefix the IPv6 Global Unicast Address Assignments registry
// designates to a regional internet registry (AFRINIC, APNIC, ARIN, LACNIC, RIPE NCC). IPv6 outside these
// prefixes and outside every special-purpose row is Undelegated.
var regionalPrefixes = []netip.Prefix{
	netip.MustParsePrefix("2001:200::/23"),  // APNIC
	netip.MustParsePrefix("2001:400::/23"),  // ARIN
	netip.MustParsePrefix("2001:600::/23"),  // RIPE NCC
	netip.MustParsePrefix("2001:800::/22"),  // RIPE NCC
	netip.MustParsePrefix("2001:c00::/23"),  // APNIC
	netip.MustParsePrefix("2001:e00::/23"),  // APNIC
	netip.MustParsePrefix("2001:1200::/23"), // LACNIC
	netip.MustParsePrefix("2001:1400::/22"), // RIPE NCC
	netip.MustParsePrefix("2001:1800::/23"), // ARIN
	netip.MustParsePrefix("2001:1a00::/23"), // RIPE NCC
	netip.MustParsePrefix("2001:1c00::/22"), // RIPE NCC
	netip.MustParsePrefix("2001:2000::/19"), // RIPE NCC
	netip.MustParsePrefix("2001:4000::/23"), // RIPE NCC
	netip.MustParsePrefix("2001:4200::/23"), // AFRINIC
	netip.MustParsePrefix("2001:4400::/23"), // APNIC
	netip.MustParsePrefix("2001:4600::/23"), // RIPE NCC
	netip.MustParsePrefix("2001:4800::/23"), // ARIN
	netip.MustParsePrefix("2001:4a00::/23"), // RIPE NCC
	netip.MustParsePrefix("2001:4c00::/23"), // RIPE NCC
	netip.MustParsePrefix("2001:5000::/20"), // RIPE NCC
	netip.MustParsePrefix("2001:8000::/19"), // APNIC
	netip.MustParsePrefix("2001:a000::/20"), // APNIC
	netip.MustParsePrefix("2001:b000::/20"), // APNIC
	netip.MustParsePrefix("2003::/18"),      // RIPE NCC
	netip.MustParsePrefix("2400::/12"),      // APNIC
	netip.MustParsePrefix("2410::/12"),      // APNIC
	netip.MustParsePrefix("2600::/12"),      // ARIN
	netip.MustParsePrefix("2610::/23"),      // ARIN
	netip.MustParsePrefix("2620::/23"),      // ARIN
	netip.MustParsePrefix("2630::/12"),      // ARIN
	netip.MustParsePrefix("2800::/12"),      // LACNIC
	netip.MustParsePrefix("2a00::/12"),      // RIPE NCC
	netip.MustParsePrefix("2a10::/12"),      // RIPE NCC
	netip.MustParsePrefix("2c00::/12"),      // AFRINIC
}
