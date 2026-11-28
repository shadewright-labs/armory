package netguard

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func FuzzParseAddr(f *testing.F) {
	for _, s := range []string{
		pub4, loop6, mapped1234, zonedLinkLocal, "127.1", "0x7f000001", "010.0.0.1", "[::1]",
		"1.2.3.4:80", doc6, "", nat64Public, "::ffff:0:808:808", strings.Repeat("1", 80),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ip, err := ParseAddr(s)
		if err != nil {
			if ip.IsValid() {
				t.Fatalf("ParseAddr(%q) returned an address with an error", s)
			}
			return
		}
		if len(s) > MaxAddrLen || strings.Contains(s, "%") || ip.Zone() != "" {
			t.Fatalf("ParseAddr(%q) accepted a long or zoned string", s)
		}
		want, err := netip.ParseAddr(s)
		if err != nil || want != ip {
			t.Fatalf("ParseAddr(%q) = %v, netip.ParseAddr = %v, %v", s, ip, want, err)
		}
		_ = Classify(ip)
	})
}

func FuzzClassify(f *testing.F) {
	for _, s := range []string{
		pub4, "10.0.0.1", relay6a44, mappedPublic, "64:ff9b::a00:1", "2001:4860::1", "3fff::1",
		"2002:808:808::1", "fd00::1", "5f00::1", "2620:4f:8000::1", "::", "255.255.255.255",
	} {
		f.Add(netip.MustParseAddr(s).AsSlice())
	}
	reg := loadRegistries(f)
	allRows := slices.Concat(reg.v4, reg.v6)
	f.Fuzz(func(t *testing.T, b []byte) {
		ip, ok := netip.AddrFromSlice(b)
		if !ok {
			return
		}
		got := Classify(ip)
		if want := reg.reference(ip); got != want {
			t.Fatalf("Classify(%s) = %v, reference %v", ip, got, want)
		}
		if ip.Is4In6() && got != Classify(ip.Unmap()) {
			t.Fatalf("Classify(%s) = %v, its IPv4 address %v", ip, got, Classify(ip.Unmap()))
		}
		if got != Public {
			return
		}
		judged := ip.Unmap()
		if refNAT64.Contains(judged) {
			judged = last32(judged)
		}
		if covered(judged) {
			t.Fatalf("Classify(%s) = public, but a protocol rule covers %s", ip, judged)
		}
		for _, r := range allRows {
			if r.prefix.Contains(judged) {
				t.Fatalf("Classify(%s) = public, inside registry row %s", ip, r.prefix)
			}
		}
		if judged.Is6() && !slices.ContainsFunc(reg.assignments, func(a assignmentRow) bool {
			return a.prefix.Contains(judged) && slices.Contains(regionalRegistries, a.designation)
		}) {
			t.Fatalf("Classify(%s) = public outside regional registry space", ip)
		}
	})
}

func FuzzControl(f *testing.F) {
	for _, c := range [][2]string{
		{netTCP, pub4HTTPS}, {netTCP4, "127.0.0.1:80"}, {netTCP6, loop6HTTPS}, {netTCP6, "[64:ff9b::808:808]:1"},
		{netTCP, nameHTTPS}, {netUDP, pub4DNS}, {netTCP6, "[fe80::1%eth0]:443"}, {netTCP, "8.8.8.8:0443"},
		{netTCP6, "[fd00:64::808:808]:443"},
	} {
		f.Add(c[0], c[1])
	}
	policies := []Policy{{}, {AllowPrivate: true}, {AllowNAT64Prefixes: prefixes("fd00:64::/96")},
		{Deny: prefixes("8.8.0.0/16")}}
	f.Fuzz(func(t *testing.T, network, address string) {
		for _, p := range policies {
			if p.Control(network, address, nil) != nil {
				continue
			}
			if len(address) > MaxControlAddrLen {
				t.Fatalf("Control allowed %d bytes", len(address))
			}
			i := strings.LastIndexByte(address, ':')
			host := strings.TrimSuffix(strings.TrimPrefix(address[:i], "["), "]")
			ip, err := netip.ParseAddr(host)
			if err != nil {
				t.Fatalf("Control allowed %q, %q with an unparsable host", network, address)
			}
			if err := p.Allow(ip); err != nil {
				t.Fatalf("Control allowed %q, which Allow refuses: %v", address, err)
			}
		}
	})
}
