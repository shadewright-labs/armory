package netguard

import (
	"net/netip"
	"strings"
)

// MaxAddrLen is the longest string ParseAddr accepts.
const MaxAddrLen = 64

// parseError is the fixed error of ParseAddr. It never quotes the input.
type parseError struct{ reason string }

func (e *parseError) Error() string { return "netguard: invalid address: " + e.reason }
func (e *parseError) Unwrap() error { return ErrInvalidAddr }

// ParseAddr parses a textual IP address strictly: at most 64 bytes, no zone, no leading zeros in an
// IPv4 part, no shortened or numeric IPv4 forms (127.1, 0x7f000001). It is the entry point for
// untrusted strings. It also refuses brackets, ports and host names. Its error matches ErrInvalidAddr and
// never quotes s.
func ParseAddr(s string) (netip.Addr, error) {
	if len(s) > MaxAddrLen {
		return netip.Addr{}, &parseError{"too long"}
	}
	if strings.IndexByte(s, '%') >= 0 {
		return netip.Addr{}, &parseError{"zone not allowed"}
	}
	// netip.ParseAddr accepts only dotted-quad IPv4 with no leading zeros and the RFC 4291 IPv6 forms,
	// so 127.1, 0x7f000001, 010.0.0.1, [::1] and ::1:443 are all refused here.
	ip, err := netip.ParseAddr(s)
	if err != nil || ip.Zone() != "" {
		return netip.Addr{}, &parseError{"not an IP address"}
	}
	return ip, nil
}
