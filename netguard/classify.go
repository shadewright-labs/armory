package netguard

import "net/netip"

// Protocol prefixes of steps 3 to 5 of ADR-0004. They are rules of the protocols, not rows of the table.
var (
	loopback4  = netip.MustParsePrefix("127.0.0.0/8")
	linkLocal4 = netip.MustParsePrefix("169.254.0.0/16")
	multicast4 = netip.MustParsePrefix("224.0.0.0/4")
	broadcast4 = netip.AddrFrom4([4]byte{255, 255, 255, 255})
	linkLocal6 = netip.MustParsePrefix("fe80::/10")
	multicast6 = netip.MustParsePrefix("ff00::/8")

	private4 = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
	uniqueLocal6 = netip.MustParsePrefix("fc00::/7")

	// nat64WellKnown is the only embedded form Classify reads: the IPv4 address is in the last 32 bits.
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
	// opaque6 are the embedded forms whose IPv4 address is not read. :: and ::1 inside ::/96 are decided
	// by step 3 first.
	opaque6 = []netip.Prefix{
		netip.MustParsePrefix("2002::/16"),       // 6to4, RFC 3056, deprecated by RFC 7526
		netip.MustParsePrefix("::/96"),           // IPv4-compatible, deprecated by RFC 4291
		netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64, RFC 8215
		netip.MustParsePrefix("2001::/32"),       // Teredo, RFC 4380
		netip.MustParsePrefix("::ffff:0:0:0/96"), // SIIT IPv4-translated
	}
)

// Classify returns the class of ip by the steps of ADR-0004. A readable embedded IPv4 address gives the
// class of that address.
func Classify(ip netip.Addr) Class {
	switch {
	case !ip.IsValid():
		return Invalid
	case ip.Zone() != "":
		// Checked before unmapping, so ::ffff:8.8.8.8%eth0 is Zoned.
		return Zoned
	}
	ip = ip.Unmap()
	if ip.Is4() {
		return classify4(ip)
	}
	return classify6(ip)
}

// IsPublic reports whether Classify(ip) is Public. ADR-0004 names this function Public, which the Class
// constant of the same name already takes.
func IsPublic(ip netip.Addr) bool { return Classify(ip) == Public }

// classify4 runs steps 3 to 7 on an IPv4 address.
func classify4(ip netip.Addr) Class {
	switch {
	case ip.IsUnspecified():
		return Unspecified
	case loopback4.Contains(ip):
		return Loopback
	case linkLocal4.Contains(ip):
		return LinkLocal
	case multicast4.Contains(ip), ip == broadcast4:
		return Multicast
	case inAny(private4, ip):
		return Private
	}
	if c, ok := tableClass(ip); ok {
		return c
	}
	return Public
}

// classify6 runs steps 3 to 7 on an IPv6 address that is not IPv4-mapped.
func classify6(ip netip.Addr) Class {
	switch {
	case ip == netip.IPv6Unspecified():
		return Unspecified
	case ip == netip.IPv6Loopback():
		return Loopback
	case linkLocal6.Contains(ip):
		return LinkLocal
	case multicast6.Contains(ip):
		return Multicast
	case nat64WellKnown.Contains(ip):
		return classify4(last32(ip))
	case inAny(opaque6, ip):
		return Opaque
	case uniqueLocal6.Contains(ip):
		return Private
	}
	if c, ok := tableClass(ip); ok {
		return c
	}
	if inAny(regionalPrefixes, ip) {
		return Public
	}
	return Undelegated
}

// tableClass is step 6: any special-purpose row gives NonGlobal, or Deprecated for a deprecated row. A row
// inside another row never changes the answer, except that a deprecated row wins.
func tableClass(ip netip.Addr) (Class, bool) {
	found := false
	for _, r := range specialRows {
		if !r.prefix.Contains(ip) {
			continue
		}
		if r.deprecated {
			return Deprecated, true
		}
		found = true
	}
	if found {
		return NonGlobal, true
	}
	return Invalid, false
}

// last32 returns the IPv4 address in the last 32 bits of an IPv6 address.
func last32(ip netip.Addr) netip.Addr {
	b := ip.As16()
	return netip.AddrFrom4([4]byte(b[12:]))
}

func inAny(ps []netip.Prefix, ip netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
