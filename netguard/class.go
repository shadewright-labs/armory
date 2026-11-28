package netguard

// Class is the category of an address. The zero value is Invalid. Callers that switch on Class need a
// default branch: a later minor version may add a value.
type Class uint8

// The classes Classify returns. See the package documentation for the order in which they are decided.
const (
	// Invalid is not a valid address (the zero netip.Addr).
	Invalid Class = iota
	// Public is a public unicast address: it may be dialled as the public internet.
	Public
	// Private is private-use IPv4 (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16) or unique-local IPv6
	// (fc00::/7).
	Private
	// Loopback is 127.0.0.0/8 or ::1.
	Loopback
	// LinkLocal is 169.254.0.0/16 or fe80::/10.
	LinkLocal
	// Multicast is 224.0.0.0/4 or ff00::/8, and also the IPv4 limited broadcast 255.255.255.255.
	Multicast
	// Unspecified is 0.0.0.0 or ::.
	Unspecified
	// NonGlobal is an address in an IANA special-purpose row. No row gives Public, whatever its
	// Globally Reachable column says.
	NonGlobal
	// Deprecated is an address in a deprecated special-purpose row (192.88.99.0/24, 2001:10::/28).
	Deprecated
	// Undelegated is IPv6 outside the space IANA has designated to a regional internet registry.
	Undelegated
	// Opaque is an IPv6 address that embeds an IPv4 address the package cannot read reliably (6to4,
	// IPv4-compatible, local-use NAT64, Teredo, SIIT).
	Opaque
	// Zoned is an address that carries a zone, such as fe80::1%eth0.
	Zoned
)

var classNames = [...]string{
	Invalid:     "invalid",
	Public:      "public",
	Private:     "private",
	Loopback:    "loopback",
	LinkLocal:   "linklocal",
	Multicast:   "multicast",
	Unspecified: "unspecified",
	NonGlobal:   "nonglobal",
	Deprecated:  "deprecated",
	Undelegated: "undelegated",
	Opaque:      "opaque",
	Zoned:       "zoned",
}

// String returns the lower-case name of c ("public", "nonglobal"), or "unknown" for a value not listed.
func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return "unknown"
}
