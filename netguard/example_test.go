package netguard_test

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/shadewright-labs/armory/netguard"
)

func ExampleClassify() {
	for _, s := range []string{"8.8.8.8", "10.0.0.1", "64:ff9b::808:808", "3fff::1", "192.88.99.1", "3000::1"} {
		fmt.Println(s, netguard.Classify(netip.MustParseAddr(s)))
	}
	// Output:
	// 8.8.8.8 public
	// 10.0.0.1 private
	// 64:ff9b::808:808 public
	// 3fff::1 nonglobal
	// 192.88.99.1 deprecated
	// 3000::1 undelegated
}

func ExampleIsPublic() {
	fmt.Println(netguard.IsPublic(netip.MustParseAddr("2001:4860:4860::8888")))
	fmt.Println(netguard.IsPublic(netip.MustParseAddr("169.254.169.254")))
	// Output:
	// true
	// false
}

func ExampleParseAddr() {
	for _, s := range []string{"8.8.8.8", "127.1", "fe80::1%eth0"} {
		ip, err := netguard.ParseAddr(s)
		fmt.Println(ip, err)
	}
	// Output:
	// 8.8.8.8 <nil>
	// invalid IP netguard: invalid address: not an IP address
	// invalid IP netguard: invalid address: zone not allowed
}

func ExamplePolicy_Allow() {
	// A product that dials a named in-cluster service: private addresses are allowed, the pod range and a
	// metadata service on a unique-local address are not.
	p := netguard.Policy{
		AllowPrivate: true,
		Deny:         []netip.Prefix{netip.MustParsePrefix("10.244.0.0/16"), netip.MustParsePrefix("fd00:ec2::254/128")},
	}
	for _, s := range []string{"10.96.0.10", "10.244.1.5", "fd00:ec2::254", "127.0.0.1"} {
		err := p.Allow(netip.MustParseAddr(s))
		fmt.Println(s, err == nil, errors.Is(err, netguard.ErrRefused))
	}
	// Output:
	// 10.96.0.10 true false
	// 10.244.1.5 false true
	// fd00:ec2::254 false true
	// 127.0.0.1 false true
}

func ExamplePolicy_Control() {
	// The dialer calls Control with each address it resolved, before it connects.
	d := &net.Dialer{Timeout: 10 * time.Second, Control: netguard.Policy{}.Control}
	_ = d

	// Called directly, as the dialer would call it:
	fmt.Println(netguard.Policy{}.Control("tcp4", "8.8.8.8:443", nil))
	fmt.Println(netguard.Policy{}.Control("tcp4", "127.0.0.1:443", nil))
	fmt.Println(netguard.Policy{}.Control("udp4", "8.8.8.8:53", nil))
	// Output:
	// <nil>
	// netguard: address refused: loopback
	// netguard: address refused: invalid network or address
}
