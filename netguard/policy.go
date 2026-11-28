package netguard

import (
	"errors"
	"net/netip"
)

// Errors a refusal matches with errors.Is. Every refusal matches ErrRefused.
var (
	// ErrRefused matches every refusal from Allow, Control and ControlContext. A caller that marks a dial
	// failure as "not sent" tests errors.Is(err, ErrRefused).
	ErrRefused = errors.New("netguard: address refused")
	// ErrInvalidPolicy matches the error of Validate, and a refusal because the Policy does not validate.
	ErrInvalidPolicy = errors.New("netguard: invalid policy")
	// ErrInvalidAddr matches the error of ParseAddr, and a refusal by Control of a malformed network or
	// address.
	ErrInvalidAddr = errors.New("netguard: invalid address")
)

// Limits of the Policy lists.
const (
	// MaxDeny is the largest number of prefixes Policy.Deny may hold.
	MaxDeny = 1024
	// MaxNAT64Prefixes is the largest number of prefixes Policy.AllowNAT64Prefixes may hold.
	MaxNAT64Prefixes = 16
)

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

// localNAT64 is the local-use NAT64 prefix of RFC 8215, one of the places an operator prefix may sit.
var localNAT64 = netip.MustParsePrefix("64:ff9b:1::/48")

// policyError is the error of Validate. Its text is fixed and names no prefix.
type policyError struct{ reason string }

func (e *policyError) Error() string { return "netguard: invalid policy: " + e.reason }
func (e *policyError) Unwrap() error { return ErrInvalidPolicy }

// Validate reports whether p is usable. It refuses a prefix in either list that is invalid or has host
// bits set, a Deny prefix that is IPv4-mapped (addresses are unmapped before the check, so it would never
// match), a NAT64 prefix that is not an IPv6 /96 inside 64:ff9b:1::/48, inside fc00::/7, or inside a
// prefix designated to a regional registry with no special-purpose row overlapping it, and a list longer
// than MaxDeny or MaxNAT64Prefixes. A Deny prefix that covers everything is accepted, since it only
// refuses more. Its error matches ErrInvalidPolicy.
func (p Policy) Validate() error {
	if len(p.Deny) > MaxDeny {
		return &policyError{"too many deny prefixes"}
	}
	if len(p.AllowNAT64Prefixes) > MaxNAT64Prefixes {
		return &policyError{"too many NAT64 prefixes"}
	}
	for _, d := range p.Deny {
		if !d.IsValid() || d.Masked() != d {
			return &policyError{"deny prefix is invalid or has host bits set"}
		}
		if d.Addr().Is4In6() {
			return &policyError{"deny prefix is IPv4-mapped"}
		}
	}
	for _, n := range p.AllowNAT64Prefixes {
		if !n.IsValid() || n.Masked() != n {
			return &policyError{"NAT64 prefix is invalid or has host bits set"}
		}
		if !n.Addr().Is6() || n.Addr().Is4In6() || n.Bits() != 96 {
			return &policyError{"NAT64 prefix is not an IPv6 /96"}
		}
		if !nat64PrefixPlace(n) {
			return &policyError{"NAT64 prefix is outside the places an operator prefix can sit"}
		}
	}
	return nil
}

// nat64PrefixPlace reports whether a /96 lies inside the local-use NAT64 prefix, inside unique-local
// space, or inside a regional registry prefix that no special-purpose row overlaps.
func nat64PrefixPlace(n netip.Prefix) bool {
	if localNAT64.Contains(n.Addr()) || uniqueLocal6.Contains(n.Addr()) {
		return true
	}
	if !inAny(regionalPrefixes, n.Addr()) {
		return false
	}
	for _, r := range specialRows {
		if r.prefix.Overlaps(n) {
			return false
		}
	}
	for _, o := range opaque6 {
		if o.Overlaps(n) {
			return false
		}
	}
	return !nat64WellKnown.Overlaps(n)
}

// Allow returns nil when p allows ip. Otherwise it returns a *Refusal, which matches ErrRefused, and
// also ErrInvalidPolicy when p does not validate. The rules apply in this order, and the first that
// decides wins: an invalid Policy is refused; an invalid or zoned address is refused; an address, or an
// IPv4 address readable inside it, in a Deny prefix is refused; an address in an AllowNAT64Prefixes
// prefix is allowed only when the IPv4 address in its last 32 bits is Public; a Public address is
// allowed; with AllowPrivate, an address that is itself private-use IPv4 or unique-local IPv6 (after
// unmapping) is allowed; anything else is refused.
func (p Policy) Allow(ip netip.Addr) error {
	if err := p.Validate(); err != nil {
		return &Refusal{Class: Classify(ip), invalidPolicy: true}
	}
	class := Classify(ip)
	if class == Invalid || class == Zoned {
		return &Refusal{Class: class}
	}
	ip = ip.Unmap()
	operator := p.operatorPrefix(ip)
	if p.denied(ip, operator) {
		return &Refusal{Class: class, Denied: true}
	}
	if operator {
		inner := classify4(last32(ip))
		if inner == Public {
			return nil
		}
		return &Refusal{Class: inner}
	}
	switch {
	case class == Public:
		return nil
	case class == Private && p.AllowPrivate && (inAny(private4, ip) || uniqueLocal6.Contains(ip)):
		return nil
	}
	return &Refusal{Class: class}
}

// operatorPrefix reports whether ip is inside a prefix of AllowNAT64Prefixes.
func (p Policy) operatorPrefix(ip netip.Addr) bool {
	return ip.Is6() && inAny(p.AllowNAT64Prefixes, ip)
}

// denied reports whether ip, or an IPv4 address readable inside it, is inside a Deny prefix. ip is
// already unmapped.
func (p Policy) denied(ip netip.Addr, operator bool) bool {
	if len(p.Deny) == 0 {
		return false
	}
	if inAny(p.Deny, ip) {
		return true
	}
	if ip.Is6() && (nat64WellKnown.Contains(ip) || operator) {
		return inAny(p.Deny, last32(ip))
	}
	return false
}

// Refusal is the error of a refused address. Its text is fixed per reason and never contains the
// address, the port, the network name or a prefix.
type Refusal struct {
	// Class is the class of the address, or of the IPv4 address in its last 32 bits when it is inside a
	// prefix of AllowNAT64Prefixes. It is Invalid for a malformed network or address.
	Class Class
	// Denied reports that the address, or an IPv4 address readable inside it, matched Deny.
	Denied bool

	invalidPolicy bool
	invalidAddr   bool
}

// Error returns fixed text for the reason of the refusal.
func (r *Refusal) Error() string {
	switch {
	case r.invalidPolicy:
		return "netguard: address refused: invalid policy"
	case r.invalidAddr:
		return "netguard: address refused: invalid network or address"
	case r.Denied:
		return "netguard: address refused: denied prefix"
	default:
		return "netguard: address refused: " + r.Class.String()
	}
}

// Unwrap returns ErrRefused, plus ErrInvalidPolicy or ErrInvalidAddr when they apply.
func (r *Refusal) Unwrap() []error {
	errs := []error{ErrRefused}
	if r.invalidPolicy {
		errs = append(errs, ErrInvalidPolicy)
	}
	if r.invalidAddr {
		errs = append(errs, ErrInvalidAddr)
	}
	return errs
}
