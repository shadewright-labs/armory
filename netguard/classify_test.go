package netguard

import (
	"net/netip"
	"testing"
)

type classCase struct {
	addr string
	want Class
}

func runClassCases(t *testing.T, cases []classCase) {
	t.Helper()
	for _, tc := range cases {
		ip := netip.MustParseAddr(tc.addr)
		if got := Classify(ip); got != tc.want {
			t.Errorf("Classify(%s) = %v, want %v", tc.addr, got, tc.want)
		}
		if got := IsPublic(ip); got != (tc.want == Public) {
			t.Errorf("IsPublic(%s) = %v", tc.addr, got)
		}
	}
}

// TestOrder has one case per step of the classification of ADR-0004.
func TestOrder(t *testing.T) {
	if got := Classify(netip.Addr{}); got != Invalid {
		t.Errorf("Classify(zero) = %v, want invalid", got)
	}
	runClassCases(t, []classCase{
		// Step 1: zones before anything else.
		{zonedLinkLocal, Zoned},
		{"::ffff:8.8.8.8%eth0", Zoned},
		// Step 2: IPv4-mapped is unmapped before the ::ffff:0:0/96 row (marked False) could decide.
		{mappedPublic, Public},
		{"::ffff:10.0.0.1", Private},
		// Step 3: protocol classes.
		{unspec4, Unspecified},
		{"::", Unspecified},
		{"127.0.0.1", Loopback},
		{"127.255.255.255", Loopback},
		{loop6, Loopback},
		{"169.254.169.254", LinkLocal},
		{"fe80::1", LinkLocal},
		{"febf:ffff::1", LinkLocal},
		{"224.0.0.1", Multicast},
		{"239.255.255.255", Multicast},
		{"255.255.255.255", Multicast},
		{"ff02::1", Multicast},
		// Step 4 before the 64:ff9b::/96 row (marked True).
		{"64:ff9b::7f00:1", Loopback},
		{nat64Public, Public},
		// Step 5.
		{"10.0.0.1", Private},
		{"172.16.0.1", Private},
		{"172.31.255.255", Private},
		{"192.168.1.1", Private},
		{"fc00::1", Private},
		{"fd12:3456::1", Private},
		// Step 6: deprecated rows, including a row inside a deprecated row, and rows marked True.
		{"192.88.99.1", Deprecated},
		{relay6a44, Deprecated},
		{"2001:10::1", Deprecated},
		{"192.0.0.9", NonGlobal},
		{"0.1.2.3", NonGlobal},
		{"100.64.0.1", NonGlobal},
		{"240.0.0.1", NonGlobal},
		{"255.255.255.254", NonGlobal},
		{doc6, NonGlobal},
		{"2001:20::1", NonGlobal},
		// Step 7.
		{pub4, Public},
		{"1.1.1.1", Public},
		{"2001:4860:4860::8888", Public},
		{"2a00:1450::1", Public},
		{"3000::1", Undelegated},
		{"4000::1", Undelegated},
		{"fec0::1", Undelegated},
		{"2d00::1", Undelegated},
	})
}

// TestKnownMisses covers the addresses one or both of the old product lists let through.
func TestKnownMisses(t *testing.T) {
	runClassCases(t, []classCase{
		{"5f00::1", NonGlobal},
		{"3fff::1", NonGlobal},
		{"100:0:0:1::1", NonGlobal},
		{"192.88.99.1", Deprecated},
		{relay6a44, Deprecated},
		{"192.0.0.9", NonGlobal},
		{"192.0.0.10", NonGlobal},
		{"192.31.196.1", NonGlobal},
		{"192.52.193.1", NonGlobal},
		{"192.175.48.1", NonGlobal},
		{"2001:1::1", NonGlobal},
		{"2001:4:112::1", NonGlobal},
		{"2620:4f:8000::1", NonGlobal},
		{pub4, Public},
		{"2001:4860:4860::8888", Public},
	})
}

// TestEmbedded covers each form of the embedded-address table of ADR-0004.
func TestEmbedded(t *testing.T) {
	runClassCases(t, []classCase{
		{"::ffff:127.0.0.1", Loopback},
		{mappedPublic, Public},
		{nat64Public, Public},
		{"64:ff9b::a00:1", Private},
		{"64:ff9b::7f00:1", Loopback},
		{"64:ff9b::c000:209", NonGlobal}, // 192.0.2.9
		{"2002:808:808::1", Opaque},
		{"2002:a00:1::1", Opaque},
		{"::808:808", Opaque},
		{"::a00:1", Opaque},
		{"64:ff9b:1::808:808", Opaque},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", Opaque},
		{"::ffff:0:808:808", Opaque},
		{"::", Unspecified},
		{loop6, Loopback},
	})
}

// TestZones checks that a zone gives Zoned whatever the address.
func TestZones(t *testing.T) {
	for _, s := range []string{zonedLinkLocal, "2001:4860::1%1", "::ffff:8.8.8.8%eth0", "64:ff9b::808:808%eth0"} {
		ip := netip.MustParseAddr(s)
		if got := Classify(ip); got != Zoned {
			t.Errorf("Classify(%s) = %v, want zoned", s, got)
		}
		if err := (Policy{AllowPrivate: true}).Allow(ip); err == nil {
			t.Errorf("Allow(%s) = nil", s)
		}
	}
	if err := (Policy{}).Control(netTCP6, "[2001:4860::1%1]:443", nil); err == nil {
		t.Error("Control allowed a zoned address")
	}
}

func TestClassString(t *testing.T) {
	want := []string{"invalid", "public", "private", "loopback", "linklocal", "multicast", "unspecified",
		"nonglobal", "deprecated", "undelegated", "opaque", "zoned"}
	for i, w := range want {
		if got := Class(i).String(); got != w {
			t.Errorf("Class(%d).String() = %q, want %q", i, got, w)
		}
	}
	if got := (Zoned + 1).String(); got != "unknown" {
		t.Errorf("unlisted class String() = %q", got)
	}
}

func TestParseAddr(t *testing.T) {
	ok := map[string]string{
		pub4:              pub4,
		doc6:              doc6,
		mapped1234:        mapped1234,
		unspec4:           unspec4,
		"2001:DB8:0:0::1": doc6,
	}
	for in, want := range ok {
		ip, err := ParseAddr(in)
		if err != nil || ip.String() != want {
			t.Errorf("ParseAddr(%q) = %v, %v; want %s", in, ip, err, want)
		}
	}
	long := "1111:2222:3333:4444:5555:6666:7777:8888" + string(make([]byte, 30))
	for _, in := range []string{
		"", "127.1", "0x7f000001", "2130706433", "010.0.0.1", "1.2.3.04", "::ffff:1.2.3.04", zonedLinkLocal,
		"[::1]", "::1:443x", "1.2.3.4:443", loop6HTTPS, "example.com", " 8.8.8.8", "8.8.8.8 ", "1.2.3.4.5",
		"1.2.3.256", long,
	} {
		if ip, err := ParseAddr(in); err == nil {
			t.Errorf("ParseAddr(%q) = %v, want an error", in, ip)
		}
	}
}
