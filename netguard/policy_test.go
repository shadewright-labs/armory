package netguard

import (
	"errors"
	"net/netip"
	"testing"
)

func prefixes(s ...string) []netip.Prefix { return mustPrefixList(s...) }

// TestPolicy has one case per rule of the precedence list of ADR-0004.
func TestPolicy(t *testing.T) {
	operator := prefixes("64:ff9b:1:a::/96")
	ulaOperator := prefixes("fd00:64::/96")
	cases := []struct {
		name   string
		p      Policy
		addr   netip.Addr
		allow  bool
		denied bool
		class  Class
	}{
		// Rule 2: invalid or zoned, before the NAT64 prefix steps.
		{"zero address", Policy{AllowPrivate: true}, netip.Addr{}, false, false, Invalid},
		{"zoned link-local", Policy{AllowPrivate: true}, netip.MustParseAddr(zonedLinkLocal), false, false, Zoned},
		{"zoned in operator prefix", Policy{AllowNAT64Prefixes: operator},
			netip.MustParseAddr("64:ff9b:1:a::808:808%eth0"), false, false, Zoned},
		// Rule 3: Deny wins over both widening fields, also for an embedded IPv4 address.
		{"deny address", Policy{Deny: prefixes("8.8.8.0/24")}, netip.MustParseAddr(pub4), false, true, Public},
		{"deny mapped address", Policy{Deny: prefixes("8.8.8.0/24")}, netip.MustParseAddr(mappedPublic),
			false, true, Public},
		{"deny embedded NAT64", Policy{Deny: prefixes("8.8.8.0/24")}, netip.MustParseAddr(nat64Public),
			false, true, Public},
		{"deny embedded operator", Policy{Deny: prefixes("8.8.8.0/24"), AllowNAT64Prefixes: operator},
			netip.MustParseAddr("64:ff9b:1:a::808:808"), false, true, Opaque},
		{"deny IPv6 address", Policy{Deny: prefixes("2001:4860::/32")}, netip.MustParseAddr("2001:4860::1"),
			false, true, Public},
		{"deny wins over AllowPrivate", Policy{AllowPrivate: true, Deny: prefixes("10.0.0.0/8")},
			netip.MustParseAddr("10.0.0.1"), false, true, Private},
		{"deny everything", Policy{Deny: prefixes("0.0.0.0/0")}, netip.MustParseAddr(pub4), false, true, Public},
		{"deny other prefix", Policy{Deny: prefixes("9.9.9.0/24")}, netip.MustParseAddr(pub4), true, false, Public},
		// Rule 4: operator NAT64 prefixes.
		{"operator public", Policy{AllowNAT64Prefixes: operator}, netip.MustParseAddr("64:ff9b:1:a::808:808"),
			true, false, Public},
		{"operator private", Policy{AllowNAT64Prefixes: operator}, netip.MustParseAddr("64:ff9b:1:a::a00:1"),
			false, false, Private},
		{"operator private with AllowPrivate", Policy{AllowNAT64Prefixes: operator, AllowPrivate: true},
			netip.MustParseAddr("64:ff9b:1:a::a00:1"), false, false, Private},
		{"operator loopback", Policy{AllowNAT64Prefixes: operator}, netip.MustParseAddr("64:ff9b:1:a::7f00:1"),
			false, false, Loopback},
		{"outside operator prefix", Policy{AllowNAT64Prefixes: operator}, netip.MustParseAddr("64:ff9b:1:b::808:808"),
			false, false, Opaque},
		{"ULA operator public", Policy{AllowNAT64Prefixes: ulaOperator}, netip.MustParseAddr("fd00:64::808:808"),
			true, false, Public},
		{"ULA operator private", Policy{AllowNAT64Prefixes: ulaOperator, AllowPrivate: true},
			netip.MustParseAddr("fd00:64::a00:1"), false, false, Private},
		// Rule 5.
		{"public", Policy{}, netip.MustParseAddr(pub4), true, false, Public},
		{"public NAT64", Policy{}, netip.MustParseAddr(nat64Public), true, false, Public},
		{"public IPv6", Policy{}, netip.MustParseAddr("2001:4860:4860::8888"), true, false, Public},
		// Rule 6: AllowPrivate only for an address that is itself private.
		{"private refused", Policy{}, netip.MustParseAddr("10.0.0.1"), false, false, Private},
		{"private allowed", Policy{AllowPrivate: true}, netip.MustParseAddr("10.0.0.1"), true, false, Private},
		{"mapped private allowed", Policy{AllowPrivate: true}, netip.MustParseAddr("::ffff:10.0.0.1"),
			true, false, Private},
		{"ULA allowed", Policy{AllowPrivate: true}, netip.MustParseAddr("fd00::1"), true, false, Private},
		{"NAT64 private refused", Policy{AllowPrivate: true}, netip.MustParseAddr("64:ff9b::a00:1"),
			false, false, Private},
		{"6to4 private refused", Policy{AllowPrivate: true}, netip.MustParseAddr("2002:a00:1::1"),
			false, false, Opaque},
		// Rule 7.
		{"loopback with AllowPrivate", Policy{AllowPrivate: true}, netip.MustParseAddr("127.0.0.1"),
			false, false, Loopback},
		{"link-local with AllowPrivate", Policy{AllowPrivate: true}, netip.MustParseAddr("169.254.169.254"),
			false, false, LinkLocal},
		{"nonglobal with AllowPrivate", Policy{AllowPrivate: true}, netip.MustParseAddr("100.64.0.1"),
			false, false, NonGlobal},
		{"undelegated", Policy{AllowPrivate: true}, netip.MustParseAddr("3000::1"), false, false, Undelegated},
	}
	for _, tc := range cases {
		err := tc.p.Allow(tc.addr)
		if tc.allow {
			if err != nil {
				t.Errorf("%s: Allow = %v, want nil", tc.name, err)
			}
			continue
		}
		var r *Refusal
		if !errors.As(err, &r) || !errors.Is(err, ErrRefused) {
			t.Errorf("%s: Allow = %v, want a *Refusal matching ErrRefused", tc.name, err)
			continue
		}
		if r.Denied != tc.denied || r.Class != tc.class {
			t.Errorf("%s: Refusal{Class: %v, Denied: %v}, want {%v, %v}", tc.name, r.Class, r.Denied, tc.class, tc.denied)
		}
		if errors.Is(err, ErrInvalidPolicy) || errors.Is(err, ErrInvalidAddr) {
			t.Errorf("%s: refusal matches an invalid-input error: %v", tc.name, err)
		}
	}
}

func TestValidate(t *testing.T) {
	many := func(n int, s string) []netip.Prefix {
		out := make([]netip.Prefix, n)
		for i := range out {
			out[i] = netip.MustParsePrefix(s)
		}
		return out
	}
	bad := map[string]Policy{
		"unmasked deny":       {Deny: []netip.Prefix{netip.PrefixFrom(netip.MustParseAddr("10.0.0.1"), 8)}},
		"invalid deny":        {Deny: []netip.Prefix{{}}},
		"mapped deny":         {Deny: prefixes("::ffff:10.0.0.0/104")},
		"NAT64 /64":           {AllowNAT64Prefixes: prefixes("64:ff9b:1:a::/64")},
		"NAT64 IPv4":          {AllowNAT64Prefixes: prefixes("10.0.0.0/8")},
		"NAT64 unmasked":      {AllowNAT64Prefixes: []netip.Prefix{netip.PrefixFrom(netip.MustParseAddr("fd00::1"), 96)}},
		"NAT64 invalid":       {AllowNAT64Prefixes: []netip.Prefix{{}}},
		"NAT64 link-local":    {AllowNAT64Prefixes: prefixes("fe80::/96")},
		"NAT64 multicast":     {AllowNAT64Prefixes: prefixes("ff00::/96")},
		"NAT64 compatible":    {AllowNAT64Prefixes: prefixes("::/96")},
		"NAT64 mapped":        {AllowNAT64Prefixes: prefixes("::ffff:0:0/96")},
		"NAT64 well-known":    {AllowNAT64Prefixes: prefixes("64:ff9b::/96")},
		"NAT64 Teredo":        {AllowNAT64Prefixes: prefixes("2001::/96")},
		"NAT64 6to4":          {AllowNAT64Prefixes: prefixes("2002::/96")},
		"NAT64 documentation": {AllowNAT64Prefixes: prefixes("2001:db8:64::/96")},
		"NAT64 AS112":         {AllowNAT64Prefixes: prefixes("2620:4f:8000::/96")},
		"NAT64 undelegated":   {AllowNAT64Prefixes: prefixes("3000::/96")},
		"NAT64 SIIT":          {AllowNAT64Prefixes: prefixes("::ffff:0:0:0/96")},
		"deny over limit":     {Deny: many(MaxDeny+1, "10.0.0.0/8")},
		"NAT64 over limit":    {AllowNAT64Prefixes: many(MaxNAT64Prefixes+1, "fd00::/96")},
	}
	for name, p := range bad {
		err := p.Validate()
		if !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("%s: Validate = %v, want ErrInvalidPolicy", name, err)
		}
		aerr := p.Allow(netip.MustParseAddr(pub4))
		var r *Refusal
		if !errors.As(aerr, &r) || !errors.Is(aerr, ErrRefused) || !errors.Is(aerr, ErrInvalidPolicy) {
			t.Errorf("%s: Allow = %v, want a refusal matching ErrRefused and ErrInvalidPolicy", name, aerr)
		}
		if cerr := p.Control(netTCP4, pub4HTTPS, nil); !errors.Is(cerr, ErrInvalidPolicy) {
			t.Errorf("%s: Control = %v, want ErrInvalidPolicy", name, cerr)
		}
	}
	good := map[string]Policy{
		"zero":                {},
		"deny everything":     {Deny: prefixes("0.0.0.0/0", "::/0")},
		"deny at limit":       {Deny: many(MaxDeny, "10.0.0.0/8")},
		"NAT64 local-use":     {AllowNAT64Prefixes: prefixes("64:ff9b:1:a::/96")},
		"NAT64 ULA":           {AllowNAT64Prefixes: prefixes("fd00:64::/96")},
		"NAT64 regional":      {AllowNAT64Prefixes: prefixes("2001:4860:64::/96")},
		"NAT64 at limit":      {AllowNAT64Prefixes: many(MaxNAT64Prefixes, "fd00::/96")},
		"IPv6 deny":           {Deny: prefixes("fd00::/8", "2001:db8::/32")},
		"AllowPrivate alone":  {AllowPrivate: true},
		"deny and AllowNAT64": {Deny: prefixes("10.0.0.0/8"), AllowNAT64Prefixes: prefixes("fd00:64::/96")},
	}
	for name, p := range good {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
}
