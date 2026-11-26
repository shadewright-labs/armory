package githost

import (
	"fmt"
	"time"

	"github.com/shadewright-labs/armory/githost/internal/secret"
)

// Scheme is how a [Token] is sent.
type Scheme int

const (
	// Bearer sends the token as "Authorization: Bearer <value>": GitHub tokens, Azure DevOps Entra tokens.
	Bearer Scheme = iota + 1
	// Basic sends the token as the password of Basic authentication with an empty user name: an Azure
	// DevOps personal access token. Not confirmed against the host yet.
	Basic
)

// placeholder is what a Token prints, formats and marshals as.
const placeholder = "[redacted token]"

// Token is a short-lived secret for one host. It prints, formats and marshals as a placeholder with every
// verb and encoder. No exported function or method returns its value or writes it anywhere: the
// package's own transport sets it in the Authorization header of a request, after it has checked the
// request's scheme and host against the one origin the client is pinned to.
//
// When a Token is an unexported field of another struct, fmt walks that struct without calling Token's
// methods. A pointer is not enough to hide the value then: with a verb a pointer does not take, such as
// %s, fmt prints the pointed-to value. So the value is returned by a function, which fmt and every
// encoder print as an address or refuse.
type Token struct {
	s *tokenData
}

type tokenData struct {
	scheme  Scheme
	expires time.Time
	value   func() string
}

func init() {
	secret.Reveal = func(v any) (int, string, bool) {
		t, ok := v.(Token)
		if !ok || t.IsZero() {
			return 0, "", false
		}
		return int(t.s.scheme), t.s.value(), true
	}
}

// NewToken wraps value. The product builds tokens; a client asks for one per request and drops it.
func NewToken(s Scheme, value string, expires time.Time) Token {
	return Token{s: &tokenData{scheme: s, expires: expires, value: func() string { return value }}}
}

// IsZero reports whether t holds no value.
func (t Token) IsZero() bool { return t.s == nil || t.s.value == nil || t.s.value() == "" }

// Expires returns when the token stops being valid; zero when the source did not say.
func (t Token) Expires() time.Time {
	if t.s == nil {
		return time.Time{}
	}
	return t.s.expires
}

// String returns a placeholder.
func (Token) String() string { return placeholder }

// GoString returns a placeholder, for %#v.
func (Token) GoString() string { return "githost.Token{" + placeholder + "}" }

// Format prints a placeholder for every verb and flag.
func (Token) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(placeholder)) }

// MarshalJSON returns a placeholder as a JSON string.
func (Token) MarshalJSON() ([]byte, error) { return []byte(`"` + placeholder + `"`), nil }

// MarshalText returns a placeholder.
func (Token) MarshalText() ([]byte, error) { return []byte(placeholder), nil }
