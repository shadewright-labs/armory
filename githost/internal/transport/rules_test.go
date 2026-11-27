package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/githost"
)

// TestRetryAfterZeroRetriesAtOnce: a delay header that asks for none ("0", a past date, a reset that has
// passed) is a valid answer, so the request is sent again at once within MaxAttempts, not after the
// one-minute fallback. An unreadable value still gets the fallback.
func TestRetryAfterZeroRetriesAtOnce(t *testing.T) {
	past := strconv.FormatInt(fixedNow.Add(-time.Minute).Unix(), 10)
	cases := []struct {
		name   string
		status int
		header map[string]string
	}{
		{"429 zero", http.StatusTooManyRequests, map[string]string{hRetry: "0"}},
		{"403 zero", http.StatusForbidden, map[string]string{hRetry: "0"}},
		{"503 zero", http.StatusServiceUnavailable, map[string]string{hRetry: "0"}},
		{"429 past date", http.StatusTooManyRequests,
			map[string]string{hRetry: fixedNow.Add(-time.Hour).Format(http.TimeFormat)}},
		{"403 quota reset passed", http.StatusForbidden,
			map[string]string{hRemaining: "0", hReset: past}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n == 1 {
					for k, v := range c.header {
						w.Header().Set(k, v)
					}
					w.WriteHeader(c.status)
					return
				}
				okBody(w)
			})
			h := newHarness(t, s, nil)
			resp, err := h.c.Do(t.Context(), get())
			if err != nil || resp.Status != http.StatusOK {
				t.Fatalf("Do = %d, %v; want 200 after a retry at once", resp.Status, err)
			}
			if len(h.waits) != 0 || s.hits.Load() != 2 {
				t.Errorf("waits %v, hits %d; want no wait and 2 hits", h.waits, s.hits.Load())
			}
		})
	}
	t.Run("unreadable value waits the fallback", func(t *testing.T) {
		s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set(hRetry, "soon")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		h := newHarness(t, s, nil)
		_, err := h.c.Do(t.Context(), get())
		var re *githost.RetryableError
		if !errors.As(err, &re) || re.RetryAfter != time.Minute || s.hits.Load() != 1 {
			t.Fatalf("Do = %v after %d hits; want retryable after a minute after 1", err, s.hits.Load())
		}
	})
	t.Run("zero stays within MaxAttempts", func(t *testing.T) {
		s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set(hRetry, "0")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		h := newHarness(t, s, func(c *Config) { c.MaxAttempts = 3 })
		_, err := h.c.Do(t.Context(), get())
		var re *githost.RetryableError
		if !errors.As(err, &re) || re.RetryAfter != 0 || s.hits.Load() != 3 {
			t.Fatalf("Do = %v after %d hits; want retryable after 3", err, s.hits.Load())
		}
		assertScrubbed(t, s, err)
	})
}

// TestConditionalHeaderKeysCanonicalised: a fixed header whose key is not in canonical case is checked
// like the canonical one, so a malformed condition cannot pass under a lower-case key and make a 304 an
// answer.
func TestConditionalHeaderKeysCanonicalised(t *testing.T) {
	cases := []struct {
		name    string
		header  http.Header
		wantErr error
	}{
		{"lower-case if-none-match", http.Header{"if-none-match": {"v1"}}, githost.ErrRejected},
		{"upper-case if-none-match", http.Header{"IF-NONE-MATCH": {blankField}}, githost.ErrRejected},
		{"lower-case if-modified-since", http.Header{"if-modified-since": {"yesterday"}}, githost.ErrRejected},
		{"two cases of if-modified-since", http.Header{"if-modified-since": {httpDate},
			hIfModifiedSince: {"Thu, 26 Nov 2026 12:00:00 GMT"}}, githost.ErrRejected},
		{"valid lower-case if-none-match", http.Header{"if-none-match": {etagV1}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(http.StatusNotModified)
			})
			h := newHarness(t, s, func(cfg *Config) { cfg.Header = c.header })
			resp, err := h.c.Do(t.Context(), get())
			if c.wantErr == nil {
				if err != nil || resp.Status != http.StatusNotModified {
					t.Fatalf("Do = %d, %v; want 304", resp.Status, err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) || s.hits.Load() != 0 || h.creds.Load() != 0 {
				t.Fatalf("Do = %d, %v after %d hits, %d creds; want %v before anything", resp.Status, err,
					s.hits.Load(), h.creds.Load(), c.wantErr)
			}
		})
	}
}

// TestContextErrorsKeepIdentity: ADR-0001 says context.Canceled and context.DeadlineExceeded keep their
// identity and a deadline is also a RetryableError, wherever the end of a context or an attempt
// deadline stops a call.
func TestContextErrorsKeepIdentity(t *testing.T) {
	t.Run("attempt deadline before the answer", func(t *testing.T) {
		h := newHarness(t, newServer(t, nil), func(c *Config) {
			c.RoundTripper = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			c.AttemptTimeout = 20 * time.Millisecond
			c.MaxAttempts = 1
		})
		_, err := h.c.Do(t.Context(), get())
		assertDeadline(t, err)
	})
	t.Run("attempt deadline during the body", func(t *testing.T) {
		h := newHarness(t, newServer(t, nil), func(c *Config) {
			c.RoundTripper = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &stallingBody{r: r},
					ContentLength: -1, Request: r}, nil
			})
			c.AttemptTimeout = 20 * time.Millisecond
			c.MaxAttempts = 1
		})
		_, err := h.c.Do(t.Context(), get())
		assertDeadline(t, err)
	})
	t.Run("transport reports a cancellation", func(t *testing.T) {
		h := newHarness(t, newServer(t, nil), func(c *Config) {
			c.RoundTripper = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })
		})
		_, err := h.c.Do(t.Context(), get())
		if !errors.Is(err, context.Canceled) || errors.Is(err, githost.ErrRetryable) {
			t.Fatalf("Do = %v; want context.Canceled, not retryable", err)
		}
	})
	t.Run("call canceled during the credential", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		h := newHarness(t, newServer(t, nil), func(c *Config) {
			c.Credential = githost.CredentialFunc(func(context.Context, githost.Scope) (githost.Token, error) {
				cancel()
				return githost.Token{}, errors.New(checkCanary)
			})
		})
		_, err := h.c.Do(ctx, get())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Do = %v; want context.Canceled", err)
		}
		assertScrubbed(t, nil, err)
	})
	t.Run("call canceled during the write check", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
		h := newHarness(t, s, func(c *Config) {
			c.WriteCheck = func(ctx context.Context, _ Send) error {
				cancel()
				return ctx.Err()
			}
		})
		_, err := h.c.Do(ctx, post(false))
		if !errors.Is(err, githost.ErrStoppedBeforeWrite) || !errors.Is(err, context.Canceled) || s.hits.Load() != 0 {
			t.Fatalf("Do = %v after %d hits; want stopped and canceled before a send", err, s.hits.Load())
		}
	})
	t.Run("call canceled with a retryable answer on the last attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			cancel()
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		h := newHarness(t, s, func(c *Config) { c.MaxAttempts = 1 })
		_, err := h.c.Do(ctx, get())
		if !errors.Is(err, context.Canceled) || errors.Is(err, githost.ErrRetryable) {
			t.Fatalf("Do = %v; want context.Canceled, not retryable", err)
		}
	})
}

func assertDeadline(t *testing.T, err error) {
	t.Helper()
	var re *githost.RetryableError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &re) {
		t.Fatalf("Do = %v; want context.DeadlineExceeded as a RetryableError", err)
	}
	assertScrubbed(t, nil, err)
}

// TestArgumentsFrozenAtCall: Do works on its own copy of the request, so a caller that changes the body
// or query it passed after the call started changes nothing that is sent, on any attempt.
func TestArgumentsFrozenAtCall(t *testing.T) {
	type seen struct{ body, query string }
	got := make(chan seen, 4)
	s := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{string(b), r.URL.RawQuery}
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		okBody(w)
	})
	r := post(true)
	r.Body = []byte(`{"x":"original"}`)
	r.Query = url.Values{"q": {"original"}}
	wantBody, wantQuery := string(r.Body), r.Query.Encode()
	h := newHarness(t, s, func(c *Config) {
		// The credential runs before each attempt; it stands for a caller that changes its inputs while the
		// call is under way.
		c.Credential = githost.CredentialFunc(func(context.Context, githost.Scope) (githost.Token, error) {
			copy(r.Body, `{"x":"CHANGED!"}`)
			r.Query.Set("q", "changed")
			r.Query.Set("extra", "1")
			return githost.NewToken(githost.Bearer, tokenCanary, time.Time{}), nil
		})
	})
	if _, err := h.c.Do(t.Context(), r); err != nil {
		t.Fatalf("Do = %v", err)
	}
	close(got)
	n := 0
	for g := range got {
		n++
		if g.body != wantBody || g.query != wantQuery {
			t.Errorf("attempt %d sent body %q, query %q; want %q, %q", n, g.body, g.query, wantBody, wantQuery)
		}
	}
	if n != 2 {
		t.Errorf("server got %d requests, want 2", n)
	}
}

// TestRequestCopyIsDeep: the copy Do takes shares no slice or map with the caller's request.
func TestRequestCopyIsDeep(t *testing.T) {
	r := post(false)
	r.Query = url.Values{"q": {"a", "b"}}
	f := r.frozen()
	r.Body[0] = 'X'
	r.Query["q"][0] = "changed"
	r.Query.Set("new", "1")
	if f.Body[0] == 'X' || f.Query.Get("q") != "a" || f.Query.Has("new") {
		t.Fatalf("frozen request follows the caller's changes: %q %v", f.Body, f.Query)
	}
	if (Request{}).frozen().Body != nil || (Request{}).frozen().Query != nil {
		t.Error("an empty request gained a body or query")
	}
	if empty := (Request{Body: []byte{}}).frozen(); empty.Body == nil {
		t.Error("an empty non-nil body became nil, so it would be sent without Content-Type")
	}
}
