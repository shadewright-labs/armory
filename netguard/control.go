package netguard

import (
	"context"
	"net/netip"
	"strings"
	"syscall"
)

// MaxControlAddrLen is the longest address string Control accepts.
const MaxControlAddrLen = 80

// Control is a net.Dialer Control function. It allows a connection only when network is tcp, tcp4
// or tcp6 (TCP only; every other network is refused), address is at most 80 bytes, its host is an IP
// literal of the network's family with no zone, its port is a decimal number from 1 to 65535 with no
// sign or leading zero, and p allows the address. Every refusal is a *Refusal and matches ErrRefused; a
// malformed network or address also matches ErrInvalidAddr. Control validates p first, as Allow does:
// when p does not validate, every refusal also matches ErrInvalidPolicy, whatever the address. Control
// never reads or changes c.
//
// net.Dialer calls Control with the IP literal it resolved, after resolution and before connect, for
// every address it tries and for every new connection, so a name that resolves to a public address once
// and to a loopback address later is refused on the connection that would reach the loopback address.
func (p Policy) Control(network, address string, _ syscall.RawConn) error {
	ip, ok := controlAddr(network, address)
	if !ok {
		// Validate first, as Allow does, so an invalid Policy is reported whatever the address.
		return &Refusal{Class: Invalid, invalidPolicy: p.Validate() != nil, invalidAddr: true}
	}
	return p.Allow(ip)
}

// ControlContext is Control for net.Dialer.ControlContext. It returns ctx.Err() unchanged when ctx is
// already done, and otherwise behaves as Control.
func (p Policy) ControlContext(ctx context.Context, network, address string, c syscall.RawConn) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.Control(network, address, c)
}

// controlAddr parses the network and address net.Dialer passes to Control, and reports false for anything
// malformed.
func controlAddr(network, address string) (netip.Addr, bool) {
	if len(address) > MaxControlAddrLen {
		return netip.Addr{}, false
	}
	var want4, want6 bool
	switch network {
	case "tcp":
	case "tcp4":
		want4 = true
	case "tcp6":
		want6 = true
	default:
		return netip.Addr{}, false
	}
	host, port, found := strings.CutLast(address, ":")
	if !found || !validPort(port) {
		return netip.Addr{}, false
	}
	if strings.HasPrefix(host, "[") {
		if !strings.HasSuffix(host, "]") {
			return netip.Addr{}, false
		}
		host = host[1 : len(host)-1]
		if strings.IndexByte(host, ':') < 0 {
			return netip.Addr{}, false // an IPv4 literal in brackets
		}
	} else if strings.IndexByte(host, ':') >= 0 {
		return netip.Addr{}, false // an IPv6 literal without brackets
	}
	if strings.IndexByte(host, '%') >= 0 {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return netip.Addr{}, false
	}
	if (want4 && !ip.Is4()) || (want6 && !ip.Is6()) {
		return netip.Addr{}, false
	}
	return ip, true
}

// validPort reports whether s is a decimal port from 1 to 65535 with no sign or leading zero.
func validPort(s string) bool {
	if s == "" || len(s) > 5 || s[0] == '0' {
		return false
	}
	n := 0
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n >= 1 && n <= 65535
}
