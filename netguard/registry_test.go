package netguard

import (
	"bufio"
	"encoding/csv"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// This file reads the pinned IANA registries under testdata/iana and builds a reference classifier from them
// and from the protocol rules of ADR-0004, written here a second time. Every answer it gives comes from the
// registry rows, not from the package table, so a table line that is missing, extra or wrong shows up as a
// difference between Classify and the reference.

const ianaDir = "testdata/iana"

// registryRow is one prefix of a special-purpose registry row. A cell that lists two prefixes gives two rows.
type registryRow struct {
	prefix     netip.Prefix
	name       string
	deprecated bool
	globally   string // the Globally Reachable cell without its footnote marker
}

// assignmentRow is one row of the IPv6 Global Unicast Address Assignments registry.
type assignmentRow struct {
	prefix      netip.Prefix
	designation string
}

var footnote = regexp.MustCompile(`\s*\[\d+\]`)

func stripFootnote(s string) string { return strings.TrimSpace(footnote.ReplaceAllString(s, "")) }

func readCSV(tb testing.TB, name string) []map[string]string {
	tb.Helper()
	f, err := os.Open(filepath.Join(ianaDir, name)) //nolint:gosec // G304: the names are fixtures of this test
	if err != nil {
		tb.Fatalf("open %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(bufio.NewReader(f)).ReadAll()
	if err != nil {
		tb.Fatalf("read %s: %v", name, err)
	}
	if len(rows) < 2 {
		tb.Fatalf("%s: no rows", name)
	}
	out := make([]map[string]string, 0, len(rows)-1)
	for _, r := range rows[1:] {
		m := make(map[string]string, len(r))
		for i, h := range rows[0] {
			m[h] = r[i]
		}
		out = append(out, m)
	}
	return out
}

func readSpecial(tb testing.TB, name string) []registryRow {
	tb.Helper()
	var out []registryRow
	for _, r := range readCSV(tb, name) {
		attrs := []string{r["Source"], r["Destination"], r["Forwardable"], r["Globally Reachable"], r["Reserved-by-Protocol"]}
		empty := !slices.ContainsFunc(attrs, func(s string) bool { return strings.TrimSpace(s) != "" })
		deprecated := r["Termination Date"] != "N/A" && empty
		for cell := range strings.SplitSeq(stripFootnote(r["Address Block"]), ",") {
			p, err := netip.ParsePrefix(strings.TrimSpace(cell))
			if err != nil {
				tb.Fatalf("%s: address block %q: %v", name, cell, err)
			}
			out = append(out, registryRow{prefix: p, name: r["Name"], deprecated: deprecated,
				globally: stripFootnote(r["Globally Reachable"])})
		}
	}
	return out
}

func readAssignments(tb testing.TB) []assignmentRow {
	tb.Helper()
	rows := readCSV(tb, "ipv6-unicast-address-assignments.csv")
	out := make([]assignmentRow, 0, len(rows))
	for _, r := range rows {
		p, err := netip.ParsePrefix(r["Prefix"])
		if err != nil {
			tb.Fatalf("assignments: prefix %q: %v", r["Prefix"], err)
		}
		out = append(out, assignmentRow{prefix: p, designation: r["Designation"]})
	}
	return out
}

// registries holds the pinned rows, read once per test binary.
type registries struct {
	v4, v6      []registryRow
	assignments []assignmentRow
}

var (
	pinnedOnce sync.Once
	pinned     registries
)

func loadRegistries(tb testing.TB) registries {
	tb.Helper()
	pinnedOnce.Do(func() {
		pinned = registries{
			v4:          readSpecial(tb, "iana-ipv4-special-registry-1.csv"),
			v6:          readSpecial(tb, "iana-ipv6-special-registry-1.csv"),
			assignments: readAssignments(tb),
		}
	})
	if len(pinned.v4) == 0 || len(pinned.v6) == 0 || len(pinned.assignments) == 0 {
		tb.Fatal("pinned registries not loaded")
	}
	return pinned
}

// The five rows that step 4 (embedded forms) handles instead of the table.
var embeddedRows = mustPrefixList("::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "2001::/32", "2002::/16")

func isEmbeddedRow(p netip.Prefix) bool { return slices.Contains(embeddedRows, p) }

var regionalRegistries = []string{"AFRINIC", "APNIC", "ARIN", "LACNIC", "RIPE NCC"}

func mustPrefixList(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// The protocol rules of steps 3 to 5 of ADR-0004, written from the ADR text.
var (
	refLoopback4  = mustPrefixList("127.0.0.0/8")
	refLinkLocal4 = mustPrefixList("169.254.0.0/16")
	refMulticast4 = mustPrefixList("224.0.0.0/4", "255.255.255.255/32")
	refPrivate4   = mustPrefixList("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")
	refLinkLocal6 = mustPrefixList("fe80::/10")
	refMulticast6 = mustPrefixList("ff00::/8")
	refPrivate6   = mustPrefixList("fc00::/7")
	refOpaque6    = mustPrefixList("2002::/16", "::/96", "64:ff9b:1::/48", "2001::/32", "::ffff:0:0:0/96")
	refNAT64      = netip.MustParsePrefix("64:ff9b::/96")
)

func inAnyPrefix(ps []netip.Prefix, ip netip.Addr) bool {
	return slices.ContainsFunc(ps, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// reference classifies ip by ADR-0004 from the pinned registries.
func (r registries) reference(ip netip.Addr) Class {
	switch {
	case !ip.IsValid():
		return Invalid
	case ip.Zone() != "":
		return Zoned
	}
	ip = ip.Unmap()
	if ip.Is4() {
		return r.reference4(ip)
	}
	switch {
	case ip == netip.IPv6Unspecified():
		return Unspecified
	case ip == netip.IPv6Loopback():
		return Loopback
	case inAnyPrefix(refLinkLocal6, ip):
		return LinkLocal
	case inAnyPrefix(refMulticast6, ip):
		return Multicast
	case refNAT64.Contains(ip):
		b := ip.As16()
		return r.reference4(netip.AddrFrom4([4]byte(b[12:])))
	case inAnyPrefix(refOpaque6, ip):
		return Opaque
	case inAnyPrefix(refPrivate6, ip):
		return Private
	}
	if c, ok := rowClass(r.v6, ip); ok {
		return c
	}
	for _, a := range r.assignments {
		if a.prefix.Contains(ip) && slices.Contains(regionalRegistries, a.designation) {
			return Public
		}
	}
	return Undelegated
}

func (r registries) reference4(ip netip.Addr) Class {
	switch {
	case ip == netip.IPv4Unspecified():
		return Unspecified
	case inAnyPrefix(refLoopback4, ip):
		return Loopback
	case inAnyPrefix(refLinkLocal4, ip):
		return LinkLocal
	case inAnyPrefix(refMulticast4, ip):
		return Multicast
	case inAnyPrefix(refPrivate4, ip):
		return Private
	}
	if c, ok := rowClass(r.v4, ip); ok {
		return c
	}
	return Public
}

func rowClass(rows []registryRow, ip netip.Addr) (Class, bool) {
	found, deprecated := false, false
	for _, row := range rows {
		if isEmbeddedRow(row.prefix) || !row.prefix.Contains(ip) {
			continue
		}
		found = true
		deprecated = deprecated || row.deprecated
	}
	switch {
	case !found:
		return Invalid, false
	case deprecated:
		return Deprecated, true
	default:
		return NonGlobal, true
	}
}

// covered reports whether steps 1 to 5 decide the class of ip, so the table never sees it.
func covered(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is4() {
		return ip == netip.IPv4Unspecified() || inAnyPrefix(refLoopback4, ip) || inAnyPrefix(refLinkLocal4, ip) ||
			inAnyPrefix(refMulticast4, ip) || inAnyPrefix(refPrivate4, ip)
	}
	return ip == netip.IPv6Unspecified() || ip == netip.IPv6Loopback() || inAnyPrefix(refLinkLocal6, ip) ||
		inAnyPrefix(refMulticast6, ip) || refNAT64.Contains(ip) || inAnyPrefix(refOpaque6, ip) ||
		inAnyPrefix(refPrivate6, ip) || netip.MustParsePrefix("::ffff:0:0/96").Contains(ip)
}

// coveringPrefixes lists the ranges covered reports, so a probe can jump past one in a single step.
var coveringPrefixes = slices.Concat(
	mustPrefixList("0.0.0.0/32", "::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96"),
	refLoopback4, refLinkLocal4, refMulticast4, refPrivate4,
	refLinkLocal6, refMulticast6, refPrivate6, refOpaque6)

// lastAddr returns the last address of p.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr()
	b := a.AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	out, _ := netip.AddrFromSlice(b)
	return out
}

// middleAddr returns the address halfway through p.
func middleAddr(p netip.Prefix) netip.Addr {
	if p.Bits() == p.Addr().BitLen() {
		return p.Addr()
	}
	b := p.Masked().Addr().AsSlice()
	i := p.Bits()
	b[i/8] |= 1 << (7 - i%8)
	out, _ := netip.AddrFromSlice(b)
	return out
}

// inward moves ip inside row past every covered range, forward or backward, and reports false when it
// leaves the row.
func inward(row netip.Prefix, ip netip.Addr, forward bool) (netip.Addr, bool) {
	for row.Contains(ip) {
		i := slices.IndexFunc(coveringPrefixes, func(p netip.Prefix) bool { return p.Contains(ip) })
		if i < 0 {
			if covered(ip) {
				panic("covered and coveringPrefixes disagree")
			}
			return ip, true
		}
		c := coveringPrefixes[i]
		if forward {
			ip = lastAddr(c).Next()
		} else {
			ip = c.Masked().Addr().Prev()
		}
		if !ip.IsValid() {
			return ip, false
		}
	}
	return ip, false
}

// TestTableMatchesRegistry ties the table to the pinned special-purpose registries row for row, probes each
// row with addresses no protocol rule covers, and checks the regional registry prefixes against the
// assignments registry.
func TestTableMatchesRegistry(t *testing.T) {
	reg := loadRegistries(t)

	var want []netip.Prefix
	byPrefix := map[netip.Prefix]registryRow{}
	for _, row := range slices.Concat(reg.v4, reg.v6) {
		if isEmbeddedRow(row.prefix) {
			continue
		}
		want = append(want, row.prefix)
		byPrefix[row.prefix] = row
	}
	got := make([]netip.Prefix, 0, len(specialRows))
	for _, r := range specialRows {
		got = append(got, r.prefix)
		row, ok := byPrefix[r.prefix]
		if !ok {
			t.Errorf("table line %s has no registry row", r.prefix)
			continue
		}
		if r.deprecated != row.deprecated {
			t.Errorf("table line %s: deprecated %v, registry %v", r.prefix, r.deprecated, row.deprecated)
		}
	}
	for _, p := range want {
		if !slices.Contains(got, p) {
			t.Errorf("registry row %s (%s) has no table line", p, byPrefix[p].name)
		}
	}
	for i, p := range got {
		if slices.Contains(got[:i], p) {
			t.Errorf("table line %s appears twice", p)
		}
	}

	for _, row := range slices.Concat(reg.v4, reg.v6) {
		if isEmbeddedRow(row.prefix) {
			continue
		}
		starts := []struct {
			ip      netip.Addr
			forward bool
		}{
			{row.prefix.Masked().Addr(), true},
			{middleAddr(row.prefix), true},
			{lastAddr(row.prefix), false},
		}
		for _, s := range starts {
			ip, ok := inward(row.prefix, s.ip, s.forward)
			if !ok {
				continue // the protocol rules cover the whole row, as for 10.0.0.0/8 or ::1/128
			}
			// Deprecated for an address in a deprecated row (192.88.99.2 sits inside 192.88.99.0/24), NonGlobal
			// for any other, whatever the Globally Reachable column says.
			wantClass, _ := rowClass(append(reg.v4, reg.v6...), ip)
			if got := Classify(ip); got != wantClass {
				t.Errorf("row %s (%s, globally reachable %q): Classify(%s) = %v, want %v",
					row.prefix, row.name, row.globally, ip, got, wantClass)
			}
		}
	}

	var wantRIR []netip.Prefix
	for _, a := range reg.assignments {
		if slices.Contains(regionalRegistries, a.designation) {
			wantRIR = append(wantRIR, a.prefix)
		}
	}
	for _, p := range wantRIR {
		if !slices.Contains(regionalPrefixes, p) {
			t.Errorf("assignments row %s has no line in regionalPrefixes", p)
		}
	}
	for _, p := range regionalPrefixes {
		if !slices.Contains(wantRIR, p) {
			t.Errorf("regionalPrefixes line %s is not a regional registry row", p)
		}
	}

	// Boundary addresses of each assignments row, outside any special row: Public for a regional registry,
	// never Public for IANA, 6to4 or documentation.
	for _, a := range reg.assignments {
		for _, ip := range []netip.Addr{a.prefix.Masked().Addr(), lastAddr(a.prefix)} {
			if _, inRow := rowClass(reg.v6, ip); inRow || covered(ip) {
				continue
			}
			got := Classify(ip)
			if slices.Contains(regionalRegistries, a.designation) {
				if got != Public {
					t.Errorf("assignments row %s (%s): Classify(%s) = %v, want public", a.prefix, a.designation, ip, got)
				}
			} else if got == Public {
				t.Errorf("assignments row %s (%s): Classify(%s) = public", a.prefix, a.designation, ip)
			}
		}
	}
}

// TestRegistryBoundaries compares Classify with the reference on the first, middle and last address of every
// row of the three registries, and on the address just outside each end.
func TestRegistryBoundaries(t *testing.T) {
	reg := loadRegistries(t)
	prefixes := make([]netip.Prefix, 0, len(reg.v4)+len(reg.v6)+len(reg.assignments)+len(coveringPrefixes)+4)
	for _, row := range slices.Concat(reg.v4, reg.v6) {
		prefixes = append(prefixes, row.prefix)
	}
	for _, a := range reg.assignments {
		prefixes = append(prefixes, a.prefix)
	}
	prefixes = append(prefixes, coveringPrefixes...)
	prefixes = append(prefixes, mustPrefixList("2000::/3", "4000::/3", "fec0::/10", "100::/8")...)
	n := 0
	for _, p := range prefixes {
		first, last := p.Masked().Addr(), lastAddr(p)
		for _, ip := range []netip.Addr{first, middleAddr(p), last, first.Prev(), last.Next()} {
			if !ip.IsValid() {
				continue
			}
			n++
			if got, want := Classify(ip), reg.reference(ip); got != want {
				t.Errorf("around %s: Classify(%s) = %v, want %v", p, ip, got, want)
			}
		}
	}
	if n < 300 {
		t.Fatalf("only %d boundary addresses checked", n)
	}
}

// TestRegistryDates checks the exported dates against testdata/iana/DATES.
func TestRegistryDates(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(ianaDir, "DATES"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			t.Fatalf("DATES: malformed line %q", line)
		}
		got[f[0]] = f[1]
	}
	want := map[string]string{
		"iana-ipv4-special-registry-1.csv":     IPv4SpecialUpdated,
		"iana-ipv6-special-registry-1.csv":     IPv6SpecialUpdated,
		"ipv6-unicast-address-assignments.csv": IPv6UnicastUpdated,
	}
	if len(got) != len(want) {
		t.Errorf("DATES has %d entries, want %d", len(got), len(want))
	}
	for file, date := range want {
		if got[file] != date {
			t.Errorf("DATES %s = %q, constant %q", file, got[file], date)
		}
		if _, err := os.Stat(filepath.Join(ianaDir, file)); err != nil {
			t.Errorf("pinned copy %s: %v", file, err)
		}
	}
}
