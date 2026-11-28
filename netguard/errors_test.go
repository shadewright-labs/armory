package netguard

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

// chain returns err and every error it unwraps to, depth first.
func chain(err error) []error {
	if err == nil {
		return nil
	}
	out := []error{err}
	switch u := err.(type) { //nolint:errorlint // walking the chain by hand
	case interface{ Unwrap() error }:
		out = append(out, chain(u.Unwrap())...)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			out = append(out, chain(e)...)
		}
	}
	return out
}

// TestErrorText checks that no error text, at any depth and in any format, quotes an address, port,
// network name or prefix from the input.
func TestErrorText(t *testing.T) {
	type input struct {
		err     error
		secrets []string
	}
	var inputs []input
	add := func(err error, secrets ...string) {
		if err == nil {
			t.Fatalf("expected an error for %q", secrets)
		}
		inputs = append(inputs, input{err, secrets})
	}

	for _, s := range []string{"10.20.30.40", "fd12:3456:789a::1", "127.0.0.1", "3fff::abcd", "64:ff9b::a14:1e28"} {
		add(Policy{}.Allow(netip.MustParseAddr(s)), s)
	}
	add(Policy{Deny: prefixes("93.184.0.0/16")}.Allow(netip.MustParseAddr("93.184.216.34")), "93.184", "216.34")
	add(Policy{AllowNAT64Prefixes: prefixes("fd00:abcd::/96")}.Allow(netip.MustParseAddr("fd00:abcd::a14:1e28")),
		"fd00:abcd", "a14:1e28", "10.20.30.40")
	add(Policy{}.Allow(netip.MustParseAddr("fe80::1%secretzone")), "secretzone", "fe80")
	for _, c := range []struct{ network, address string }{
		{netTCP4, "10.20.30.40:31337"},
		{netTCP6, "[fe80::1%secretzone]:31337"},
		{"secretnet", "93.184.216.34:31337"},
		{netTCP, "secret.example.org:31337"},
		{netTCP, "93.184.216.34:313370"},
	} {
		add(Policy{}.Control(c.network, c.address, nil), c.address, "31337", "secret", "93.184", "10.20")
	}
	for _, s := range []string{"secret.example.org", "10.20.30.040", "fe80::1%secretzone", strings.Repeat("7", 70)} {
		_, err := ParseAddr(s)
		add(err, s, "secret", "10.20")
	}
	add(Policy{Deny: []netip.Prefix{netip.PrefixFrom(netip.MustParseAddr("10.20.30.40"), 8)}}.Validate(),
		"10.20", "/8")
	add(Policy{AllowNAT64Prefixes: prefixes("2001:db8:abcd::/96")}.Validate(), "2001:db8", "abcd", "/96")
	add(Policy{Deny: prefixes("::ffff:10.20.0.0/112")}.Validate(), "10.20", "ffff", "/112")
	add(Policy{Deny: prefixes("::ffff:10.20.0.0/112")}.Allow(netip.MustParseAddr("8.8.4.4")), "10.20", "8.8.4.4")

	for _, in := range inputs {
		for _, e := range chain(in.err) {
			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				text := fmt.Sprintf(verb, e)
				for _, s := range in.secrets {
					if strings.Contains(text, s) {
						t.Errorf("error text %q (%s) contains input %q", text, verb, s)
					}
				}
			}
		}
		if !errors.Is(in.err, ErrRefused) && !errors.Is(in.err, ErrInvalidAddr) && !errors.Is(in.err, ErrInvalidPolicy) {
			t.Errorf("error %v matches no exported error", in.err)
		}
	}
}
