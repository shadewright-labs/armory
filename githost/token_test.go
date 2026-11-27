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
	"log"
	"log/slog"
	"os"
	"os/exec"
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

// leaks reports whether s holds the canary, plainly or as Basic credentials would carry it.
func leaks(s string) bool {
	return strings.Contains(s, canary) ||
		strings.Contains(s, base64.StdEncoding.EncodeToString([]byte(canary))) ||
		strings.Contains(s, base64.StdEncoding.EncodeToString([]byte(":"+canary)))
}

// recordState is a fmt.State that keeps what Format writes.
type recordState struct {
	bytes.Buffer
	flags string
}

func (*recordState) Width() (int, bool)     { return 0, false }
func (*recordState) Precision() (int, bool) { return 0, false }
func (s *recordState) Flag(c int) bool {
	return c >= 0 && c < 128 && strings.IndexByte(s.flags, byte(c)) >= 0
}
func (s *recordState) Write(p []byte) (int, error) { return s.Buffer.Write(p) }

// TestTokenEachVerb: for each of %v, %+v, %#v, %q, %s, %x and %d, a token of either scheme formats as
// the placeholder when Format is called directly and through fmt, and leaves no trace of its value in
// error text (wrapped, joined and printed with the verb), the log and slog packages, or a recovered
// panic.
func TestTokenEachVerb(t *testing.T) {
	verbs := []struct {
		verb  string
		r     rune
		flags string
	}{
		{"%v", 'v', ""}, {"%+v", 'v', "+"}, {"%#v", 'v', "#"}, {"%q", 'q', ""}, {"%s", 's', ""},
		{"%x", 'x', ""}, {"%d", 'd', ""},
	}
	for _, scheme := range []Scheme{Bearer, Basic} {
		tok := NewToken(scheme, canary, time.Time{})
		if leaks(tok.String()) || leaks(tok.GoString()) || tok.String() != placeholder {
			t.Fatalf("String %q, GoString %q", tok.String(), tok.GoString())
		}
		for _, v := range verbs {
			t.Run(fmt.Sprintf("%d %s", scheme, v.verb), func(t *testing.T) {
				var st recordState
				st.flags = v.flags
				tok.Format(&st, v.r)
				if st.String() != placeholder {
					t.Errorf("Format(%s) wrote %q, want the placeholder", v.verb, st.String())
				}
				out := map[string]string{
					"Sprintf value":   fmt.Sprintf(v.verb, tok),
					"Sprintf pointer": fmt.Sprintf(v.verb, &tok),
				}
				if got := out["Sprintf value"]; got != placeholder {
					t.Errorf("Sprintf(%s) = %q, want the placeholder", v.verb, got)
				}
				base := fmt.Errorf("token "+v.verb, tok)
				wrapped := fmt.Errorf("call: %w", base)
				out["error"] = base.Error()
				out["wrapped error"] = wrapped.Error()
				out["joined error"] = errors.Join(errors.New("x"), base).Error()
				out["error printed with the verb"] = fmt.Sprintf(v.verb, wrapped)
				var logs bytes.Buffer
				log.New(&logs, "", 0).Printf(v.verb, tok)
				slog.New(slog.NewTextHandler(&logs, nil)).Error("m", "err", base, "tok", tok, "f", fmt.Sprintf(v.verb, tok))
				slog.New(slog.NewJSONHandler(&logs, nil)).Error("m", "err", base, "tok", tok, "ptr", &tok)
				out["logs"] = logs.String()
				out["recovered panic"] = recovered(func() { panic(tok) }, v.verb)
				out["recovered panic pointer"] = recovered(func() { panic(&tok) }, v.verb)
				out["recovered panic error"] = recovered(func() { panic(base) }, v.verb)
				for where, s := range out {
					if leaks(s) {
						t.Errorf("%s: token value in %.200q", where, s)
					}
				}
			})
		}
	}
}

// recovered runs f, which panics, and formats the recovered value with verb.
func recovered(f func(), verb string) (out string) {
	defer func() { out = fmt.Sprintf(verb, recover()) }()
	f()
	return ""
}

// panicEnv names the variable that makes TestTokenUncaughtPanic panic in a child process.
const panicEnv = "GITHOST_TOKEN_PANIC_CASE"

// TestTokenUncaughtPanic: the runtime's own report of an uncaught panic, which prints a Stringer or error
// with its method and any other value by type and address, holds no token value. Each case panics in a
// child process of the test binary.
func TestTokenUncaughtPanic(t *testing.T) {
	cases := map[string]func(Token){
		"value":   func(tok Token) { panic(tok) },
		"address": func(tok Token) { panic(&tok) },
		"struct":  func(tok Token) { panic(struct{ T Token }{tok}) },
		"error":   func(tok Token) { panic(fmt.Errorf("wrapped: %v", tok)) },
	}
	if name := os.Getenv(panicEnv); name != "" {
		cases[name](NewToken(Basic, canary, time.Time{}))
		return
	}
	for name := range cases {
		t.Run(name, func(t *testing.T) {
			// #nosec G204 G702 -- runs this test binary again, with fixed arguments
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestTokenUncaughtPanic$", "-test.count=1")
			cmd.Env = append(os.Environ(), panicEnv+"="+name)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "panic:") {
				t.Fatalf("child did not panic: %v, %.300q", err, out)
			}
			if leaks(string(out)) {
				t.Fatalf("token value in the panic report %.500q", out)
			}
		})
	}
}
