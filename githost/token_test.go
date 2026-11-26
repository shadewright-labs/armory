package githost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/githost/internal/secret"
)

// canary is a synthetic token value; no output may contain it.
const canary = "CANARY-7f3c9e1b-token-value"

func TestTokenNeverPrints(t *testing.T) {
	tok := NewToken(Bearer, canary, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	type exported struct{ T Token }
	type unexported struct{ t Token }
	type pointer struct{ T *Token }
	values := map[string]any{
		"value":              tok,
		"pointer":            &tok,
		"exported field":     exported{tok},
		"unexported field":   unexported{tok},
		"pointer field":      pointer{&tok},
		"slice":              []Token{tok},
		"map":                map[string]Token{"k": tok},
		"pointer to struct":  &unexported{tok},
		"interface in slice": []any{tok},
	}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T", "%p", "%10.3s", "%-8v", "%08x",
		"%t", "%e", "%c", "%U", "%b", "%o"}
	for name, v := range values {
		var out []string
		for _, verb := range verbs {
			out = append(out, fmt.Sprintf(verb, v))
		}
		out = append(out, fmt.Sprint(v), fmt.Sprintln(v))
		if data, err := json.Marshal(v); err == nil {
			out = append(out, string(data))
		}
		if data, err := xml.Marshal(v); err == nil {
			out = append(out, string(data))
		}
		var gb bytes.Buffer
		_ = gob.NewEncoder(&gb).Encode(v)
		out = append(out, gb.String())
		var logs bytes.Buffer
		slog.New(slog.NewJSONHandler(&logs, nil)).Info("m", "v", v)
		slog.New(slog.NewTextHandler(&logs, nil)).Info("m", "v", v)
		out = append(out, logs.String())
		for _, s := range out {
			if strings.Contains(s, canary) || strings.Contains(s, base64.StdEncoding.EncodeToString([]byte(canary))) {
				t.Fatalf("%s: token value in output %.200q", name, s)
			}
		}
	}
	// Errors that wrap a token's printing keep the placeholder too.
	err := fmt.Errorf("failed with %v", tok)
	if strings.Contains(err.Error(), canary) || !strings.Contains(err.Error(), placeholder) {
		t.Fatalf("error text %q", err)
	}
}

func TestTokenMarshalersReturnPlaceholder(t *testing.T) {
	tok := NewToken(Basic, canary, time.Time{})
	cases := map[string]func() (string, error){
		"String":   func() (string, error) { return tok.String(), nil },
		"GoString": func() (string, error) { return tok.GoString(), nil },
		"MarshalJSON": func() (string, error) {
			b, err := tok.MarshalJSON()
			return string(b), err
		},
		"MarshalText": func() (string, error) {
			b, err := tok.MarshalText()
			return string(b), err
		},
	}
	for name, f := range cases {
		got, err := f()
		if err != nil || !strings.Contains(got, placeholder) || strings.Contains(got, canary) {
			t.Errorf("%s = %q, %v; want the placeholder", name, got, err)
		}
	}
}

// TestTokenReveal: the internal path hands out the scheme and value, and nothing for a zero token or
// another type.
func TestTokenReveal(t *testing.T) {
	exp := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	cases := []struct {
		name   string
		v      any
		ok     bool
		scheme Scheme
	}{
		{"bearer", NewToken(Bearer, canary, exp), true, Bearer},
		{"basic", NewToken(Basic, canary, exp), true, Basic},
		{"zero", Token{}, false, 0},
		{"empty value", NewToken(Bearer, "", exp), false, 0},
		{"pointer", func() any { tok := NewToken(Bearer, canary, exp); return &tok }(), false, 0},
		{"string", canary, false, 0},
	}
	for _, c := range cases {
		scheme, value, ok := secret.Reveal(c.v)
		want := ""
		if c.ok {
			want = canary
		}
		if ok != c.ok || Scheme(scheme) != c.scheme || value != want {
			t.Errorf("%s: Reveal = %d, %t; want %d, %t", c.name, scheme, ok, c.scheme, c.ok)
		}
	}
	if got := NewToken(Bearer, canary, exp).Expires(); !got.Equal(exp) {
		t.Errorf("Expires = %v, want %v", got, exp)
	}
	if !(Token{}).Expires().IsZero() || !(Token{}).IsZero() || NewToken(Bearer, canary, exp).IsZero() {
		t.Error("zero token reports wrong")
	}
}

func TestCredentialFunc(t *testing.T) {
	want := Scope{Repo: RepoRef{Host: GitHub, Owner: "o", Name: "n"}, Write: true}
	var got Scope
	var c Credential = CredentialFunc(func(_ context.Context, s Scope) (Token, error) {
		got = s
		return NewToken(Bearer, canary, time.Time{}), errors.New("x")
	})
	tok, err := c.Token(t.Context(), want)
	if got != want || tok.IsZero() || err == nil {
		t.Errorf("CredentialFunc passed %+v, returned %v, %v", got, tok, err)
	}
}
