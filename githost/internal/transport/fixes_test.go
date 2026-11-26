package transport

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/githost"
)

// TestRedirectOnWrite: a 3xx answer to a write, a 303 after a POST above all, may follow a write the
// host carried out. It is never followed and never read as "nothing was written": it matches
// ErrOutcomeUnknown with its status kept, is not retried, and the target gets nothing.
func TestRedirectOnWrite(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusNotModified, http.StatusTemporaryRedirect, http.StatusPermanentRedirect, 399} {
		for _, idem := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d idempotent=%v", code, idem), func(t *testing.T) {
				s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
					w.Header().Set("Location", "/receipt")
					w.WriteHeader(code)
				})
				h := newHarness(t, s, nil)
				resp, err := h.c.Do(t.Context(), post(idem))
				var he *githost.Error
				if !errors.Is(err, githost.ErrOutcomeUnknown) || !errors.As(err, &he) || he.Status != code {
					t.Fatalf("Do = %+v, %v; want ErrOutcomeUnknown with status %d", resp, err, code)
				}
				if errors.Is(err, githost.ErrRetryable) || resp.Status != 0 {
					t.Errorf("Do = %+v, %v: retryable or with a response", resp, err)
				}
				assertScrubbed(t, s, err)
				if n := s.hits.Load(); n != 1 || len(h.checks) != 1 {
					t.Errorf("host got %d requests after %d checks, want 1 and 1", n, len(h.checks))
				}
			})
		}
	}
}

// TestNotModifiedOnlyConditional: 304 is an answer only to a GET or HEAD that carried If-None-Match or
// If-Modified-Since. Any other 304 is an error with its status.
func TestNotModifiedOnlyConditional(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		inm     string
		header  http.Header
		ok      bool
		unknown bool
	}{
		{"GET with If-None-Match", http.MethodGet, etagV1, nil, true, false},
		{"HEAD with If-None-Match", http.MethodHead, etagV1, nil, true, false},
		{"GET with If-Modified-Since", http.MethodGet, "",
			http.Header{"If-Modified-Since": {"Wed, 25 Nov 2026 12:00:00 GMT"}}, true, false},
		{"GET unconditional", http.MethodGet, "", nil, false, false},
		{"HEAD unconditional", http.MethodHead, "", nil, false, false},
		{"POST with If-None-Match", http.MethodPost, etagV1, nil, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(http.StatusNotModified)
			})
			h := newHarness(t, s, func(cfg *Config) { cfg.Header = c.header })
			r := get()
			r.Method, r.IfNoneMatch = c.method, c.inm
			resp, err := h.c.Do(t.Context(), r)
			if c.ok {
				if err != nil || resp.Status != http.StatusNotModified {
					t.Fatalf("Do = %+v, %v; want a 304", resp, err)
				}
				return
			}
			var he *githost.Error
			if !errors.As(err, &he) || he.Status != http.StatusNotModified || errors.Is(err, githost.ErrRetryable) ||
				errors.Is(err, githost.ErrOutcomeUnknown) != c.unknown || resp.Status != 0 {
				t.Fatalf("Do = %+v, %v; want a status 304 error (unknown=%v)", resp, err, c.unknown)
			}
			if n := s.hits.Load(); n != 1 {
				t.Errorf("host got %d requests, want 1", n)
			}
		})
	}
}

// TestMalformedLocation: a redirect whose Location cannot be parsed keeps the status it came with, is
// not a network failure, and is not retried.
func TestMalformedLocation(t *testing.T) {
	for _, loc := range []string{"%", "http://[::1", "https://evil.example.test/%zz"} {
		t.Run(loc, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Location", loc)
				w.WriteHeader(http.StatusFound)
			})
			h := newHarness(t, s, nil)
			_, err := h.c.Do(t.Context(), get())
			var he *githost.Error
			if !errors.As(err, &he) || he.Status != http.StatusFound || errors.Is(err, githost.ErrRetryable) {
				t.Fatalf("Do = %v, want a status 302 error", err)
			}
			if n := s.hits.Load(); n != 1 {
				t.Errorf("host got %d requests, want 1", n)
			}
			if len(h.events) != 1 || h.events[0].Status != http.StatusFound {
				t.Errorf("events %+v, want one with status 302", h.events)
			}
		})
	}
}

// TestRefusalBeforeCancellation: an answer that says the write was not carried out keeps saying so when
// the call's context ends right after it arrives. Only an answer that does not say so is unknown.
func TestRefusalBeforeCancellation(t *testing.T) {
	cases := []struct {
		status  int
		unknown bool
	}{
		{http.StatusTooManyRequests, false},
		{http.StatusNotFound, false},
		{http.StatusConflict, false},
		{http.StatusUnprocessableEntity, false},
		{http.StatusServiceUnavailable, true},
		{http.StatusRequestTimeout, true},
		{http.StatusSeeOther, true},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(c.status) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h := newHarness(t, s, func(cfg *Config) {
				cfg.RoundTripper = endAfter{next: s.srv.Client().Transport, end: cancel}
			})
			_, err := h.c.Do(ctx, post(false))
			if errors.Is(err, githost.ErrOutcomeUnknown) != c.unknown {
				t.Fatalf("Do = %v, want unknown %v", err, c.unknown)
			}
			var he *githost.Error
			if !c.unknown && !errors.Is(err, context.Canceled) && (!errors.As(err, &he) || he.Status != c.status) {
				t.Errorf("Do = %v, want the cancellation or the status", err)
			}
			if len(h.events) != 1 || h.events[0].Status != c.status {
				t.Errorf("events %+v", h.events)
			}
		})
	}
}

// TestHeadersCheckedBeforeTheWrite: a request whose headers Go would refuse to send is refused before
// the credential, the write check and the send, so it is never an unknown outcome.
func TestHeadersCheckedBeforeTheWrite(t *testing.T) {
	cases := map[string]func(*Request){
		"accept with a line feed":         func(r *Request) { r.Accept = "line1\nline2" },
		"accept with a carriage return":   func(r *Request) { r.Accept = "a\rb" },
		"if-none-match with a NUL":        func(r *Request) { r.IfNoneMatch = "\"a\x00\"" },
		"content type with a line feed":   func(r *Request) { r.ContentType = "application/json\nX: y" },
		"content type with DEL":           func(r *Request) { r.ContentType = "a\x7fb" },
		"accept with a leading line feed": func(r *Request) { r.Accept = "\nx" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
			h := newHarness(t, s, nil)
			r := post(false)
			mod(&r)
			_, err := h.c.Do(t.Context(), r)
			if !errors.Is(err, githost.ErrRejected) || errors.Is(err, githost.ErrOutcomeUnknown) {
				t.Fatalf("Do = %v, want ErrRejected only", err)
			}
			if n := s.hits.Load(); n != 0 || len(h.checks) != 0 || h.creds.Load() != 0 {
				t.Errorf("hits %d, checks %d, credential asked %d times; want none", n, len(h.checks), h.creds.Load())
			}
		})
	}
	t.Run("token with a line feed", func(t *testing.T) {
		s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
		h := newHarness(t, s, func(cfg *Config) { cfg.Credential = tokenCred("a\nb") })
		_, err := h.c.Do(t.Context(), post(false))
		if err == nil || errors.Is(err, githost.ErrOutcomeUnknown) {
			t.Fatalf("Do = %v, want an error that is not an unknown outcome", err)
		}
		if n := s.hits.Load(); n != 0 || len(h.checks) != 0 {
			t.Errorf("hits %d, checks %d; want none", n, len(h.checks))
		}
	})
}

// TestNewRefusesBadHeaders: fixed headers and the user agent that Go would refuse to send, and the
// Accept-Encoding the client sets itself, are refused when the client is built.
func TestNewRefusesBadHeaders(t *testing.T) {
	cases := map[string]Config{
		"value with a line feed": {Header: http.Header{hAPIVersion: {"1\n2"}}},
		"name with a space":      {Header: http.Header{"X Api": {"1"}}},
		"empty name":             {Header: http.Header{"": {"1"}}},
		"name with a colon":      {Header: http.Header{"X:y": {"1"}}},
		"user agent with a NUL":  {UserAgent: "a\x00b"},
		"accept-encoding":        {Header: http.Header{"Accept-Encoding": {"gzip"}}},
	}
	for name, cfg := range cases {
		cfg.Host, cfg.Credential = apiHost, tokenCred(tokenCanary)
		if _, err := New(cfg); !errors.Is(err, githost.ErrInvalidConfig) {
			t.Errorf("%s: New = %v, want ErrInvalidConfig", name, err)
		}
	}
}

// TestBodyBoundEdges: the bound counts the bytes that arrive; it holds at the integer limit, does not
// apply to a HEAD answer's declared length, and is not escaped by a compressed answer.
func TestBodyBoundEdges(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("x"))
	_ = zw.Close()
	cases := []struct {
		name     string
		method   string
		limit    int64
		handler  func(w http.ResponseWriter, r *http.Request)
		wantBody string
		wantErr  error
	}{
		{"largest limit", http.MethodPost, math.MaxInt64, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "abc")
		}, "abc", nil},
		{"largest limit, read", http.MethodGet, math.MaxInt64, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "abc")
		}, "abc", nil},
		{"HEAD with a declared length", http.MethodHead, 1, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
		}, "", nil},
		{"gzip answer over the bound", http.MethodGet, 1, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept-Encoding") != "identity" {
				http.Error(w, "identity not asked for", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", strconv.Itoa(gz.Len()))
			_, _ = w.Write(gz.Bytes())
		}, "", githost.ErrTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, r *http.Request, _ int) { c.handler(w, r) })
			h := newHarness(t, s, nil)
			r := get()
			if c.method == http.MethodPost {
				r = post(false)
			}
			r.Method, r.Limit = c.method, c.limit
			resp, err := h.c.Do(t.Context(), r)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("Do = %q, %v; want %v", resp.Body, err, c.wantErr)
				}
				return
			}
			if err != nil || string(resp.Body) != c.wantBody {
				t.Fatalf("Do = %q, %v; want %q", resp.Body, err, c.wantBody)
			}
		})
	}
}

// TestBackoffAtTheLimits: a base delay near the integer limit neither overflows nor panics in the
// default jitter; every wait stays within MaxWait.
func TestBackoffAtTheLimits(t *testing.T) {
	for _, bd := range []time.Duration{1 << 62, math.MaxInt64, 1 << 40} {
		t.Run(bd.String(), func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			h := newHarness(t, s, func(c *Config) {
				c.BaseDelay, c.Jitter, c.MaxAttempts = bd, nil, 3
			})
			var err error
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("Do panicked: %v", p)
					}
				}()
				_, err = h.c.Do(t.Context(), get())
			}()
			if !errors.Is(err, githost.ErrRetryable) {
				t.Fatalf("Do = %v, want the last 503", err)
			}
			if len(h.waits) != 2 {
				t.Fatalf("waits %v, want 2", h.waits)
			}
			for _, w := range h.waits {
				if w < DefaultMaxWait/2 || w > DefaultMaxWait {
					t.Errorf("wait %s, want within [MaxWait/2, MaxWait]", w)
				}
			}
		})
	}
}

// TestOpLabelChecked: an operation label that is not a short fixed label is refused, and its text never
// reaches the error.
func TestOpLabelChecked(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
	h := newHarness(t, s, nil)
	for _, op := range []string{"first\nsecond", "a b", strings.Repeat("a", 65), "x\x1b[31m", "é"} {
		r := get()
		r.Op = op
		_, err := h.c.Do(t.Context(), r)
		if !errors.Is(err, githost.ErrRejected) || strings.Contains(err.Error(), op) {
			t.Errorf("Do(op %q) = %q, want ErrRejected without the label", op, err)
		}
	}
	if n := s.hits.Load(); n != 0 {
		t.Errorf("host got %d requests", n)
	}
}

// TestHostPortOnlyOnLoopback: a port is allowed only on a loopback host, and only a valid one.
func TestHostPortOnlyOnLoopback(t *testing.T) {
	for _, h := range []string{"service.test:8443", "service.test:", "service.test:99999", "127.0.0.1:",
		"127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:08443", "127.0.0.1:+80", "[::1]:", "10.0.0.1:8443"} {
		if _, err := New(Config{Host: h, Credential: tokenCred(tokenCanary)}); !errors.Is(err, githost.ErrInvalidConfig) {
			t.Errorf("New(%q) = %v, want ErrInvalidConfig", h, err)
		}
	}
	for _, h := range []string{"service.test", "127.0.0.1:8443", "[::1]:8443", "localhost:8443", "127.0.0.1:65535"} {
		if _, err := New(Config{Host: h, Credential: tokenCred(tokenCanary)}); err != nil {
			t.Errorf("New(%q) = %v", h, err)
		}
	}
}

// TestTypedNilCallbacks: a nil function behind the Credential interface is refused when the client is
// built, and a credential that returns a typed nil error does not panic the call.
func TestTypedNilCallbacks(t *testing.T) {
	if _, err := New(Config{Host: apiHost, Credential: githost.CredentialFunc(nil)}); !errors.Is(err,
		githost.ErrInvalidConfig) {
		t.Errorf("New with a nil CredentialFunc = %v, want ErrInvalidConfig", err)
	}
	for name, cerr := range map[string]error{
		caseRetryable: (*githost.RetryableError)(nil),
		"error":       (*githost.Error)(nil),
		"refusal":     (*githost.Refusal)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
			h := newHarness(t, s, func(cfg *Config) {
				cfg.Credential = githost.CredentialFunc(func(context.Context, githost.Scope) (githost.Token, error) {
					return githost.Token{}, cerr
				})
				cfg.MaxAttempts = 2
			})
			var err error
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("Do panicked: %v", p)
					}
				}()
				_, err = h.c.Do(t.Context(), get())
			}()
			if err == nil {
				t.Fatal("Do succeeded")
			}
			_ = err.Error()
		})
	}
}
