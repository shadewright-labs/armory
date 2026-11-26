package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/githost"
)

func dropConn(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// TestOutcomeUnknownAcrossAttempts: once an attempt of a write may have reached the host, every later
// failure of the call says the outcome is unknown and keeps its own cause, and none is retryable. An
// attempt the host answered with a rate limit did not write, so it leaves the outcome known.
func TestOutcomeUnknownAcrossAttempts(t *testing.T) {
	errDenied := errors.New(checkCanary)
	cases := []struct {
		name      string
		req       Request
		first     func(w http.ResponseWriter) // the answer to attempt 1; later attempts get later
		later     func(w http.ResponseWriter)
		refuse    int // the attempt from which the write check refuses; 0 means never
		credFail  int // the attempt whose credential fails with ErrUnauthorized; 0 means none
		cancelIn  bool
		attempts  int
		want      []error
		notWant   []error
		wantHits  int32
		wantState int // the status of the *githost.Error in the chain; 0 means none checked
	}{
		{name: "lost, then the check refuses", req: post(true), first: dropConn, refuse: 2,
			want: []error{githost.ErrOutcomeUnknown, githost.ErrStoppedBeforeWrite}, wantHits: 1},
		{name: "502, then the check refuses", req: post(true),
			first: func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }, refuse: 2,
			want: []error{githost.ErrOutcomeUnknown, githost.ErrStoppedBeforeWrite}, wantHits: 1},
		{name: "lost, then the credential fails", req: post(true), first: dropConn, credFail: 2,
			want: []error{githost.ErrOutcomeUnknown, githost.ErrUnauthorized}, wantHits: 1},
		{name: "lost, then cancelled in the backoff", req: post(true), first: dropConn, cancelIn: true,
			want: []error{githost.ErrOutcomeUnknown, context.Canceled}, wantHits: 1},
		{name: "lost every time", req: post(true), first: dropConn, later: dropConn,
			want: []error{githost.ErrOutcomeUnknown}, wantHits: DefaultMaxAttempts},
		{name: "lost, then 503 every time", req: post(true), first: dropConn,
			later: func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) },
			want:  []error{githost.ErrOutcomeUnknown}, wantHits: DefaultMaxAttempts, wantState: 503},
		{name: "lost, then a conflict", req: post(true), first: dropConn,
			later: func(w http.ResponseWriter) { w.WriteHeader(http.StatusConflict) },
			want:  []error{githost.ErrOutcomeUnknown, githost.ErrConflict}, wantHits: 2, wantState: 409},
		{name: "lost, then 422", req: post(true), first: dropConn,
			later: func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnprocessableEntity) },
			want:  []error{githost.ErrOutcomeUnknown, githost.ErrRejected}, wantHits: 2, wantState: 422},
		{name: "not idempotent, lost", req: post(false), first: dropConn,
			want: []error{githost.ErrOutcomeUnknown}, wantHits: 1},
		{name: "not idempotent, 502", req: post(false),
			first: func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
			want:  []error{githost.ErrOutcomeUnknown}, wantHits: 1, wantState: 502},
		{name: "rate limited, then the check refuses", req: post(true),
			first: func(w http.ResponseWriter) {
				w.Header().Set(hRetry, "1")
				w.WriteHeader(http.StatusTooManyRequests)
			}, refuse: 2,
			want: []error{githost.ErrStoppedBeforeWrite}, notWant: []error{githost.ErrOutcomeUnknown}, wantHits: 1},
		{name: "a read lost every time", req: get(), first: dropConn, later: dropConn,
			want: []error{githost.ErrRetryable}, notWant: []error{githost.ErrOutcomeUnknown}, wantHits: DefaultMaxAttempts},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				switch {
				case n == 1:
					c.first(w)
				case c.later != nil:
					c.later(w)
				default:
					okBody(w)
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var creds atomic.Int32
			h := newHarness(t, s, func(cfg *Config) {
				cfg.WriteCheck = func(_ context.Context, sd Send) error {
					if c.refuse > 0 && sd.Attempt >= c.refuse {
						return errDenied
					}
					return nil
				}
				cfg.Credential = githost.CredentialFunc(func(context.Context, githost.Scope) (githost.Token, error) {
					if int(creds.Add(1)) == c.credFail {
						return githost.Token{}, fmt.Errorf("%s: %w", tokenCanary, githost.ErrUnauthorized)
					}
					return githost.NewToken(githost.Bearer, tokenCanary, time.Time{}), nil
				})
				if c.cancelIn {
					cfg.Sleep = func(ctx context.Context, _ time.Duration) error {
						cancel()
						<-ctx.Done()
						return ctx.Err()
					}
				}
			})
			_, err := h.c.Do(ctx, c.req)
			for _, k := range c.want {
				if !errors.Is(err, k) {
					t.Errorf("Do = %v, want it to match %v", err, k)
				}
			}
			for _, k := range c.notWant {
				if errors.Is(err, k) {
					t.Errorf("Do = %v matches %v", err, k)
				}
			}
			if errors.Is(err, githost.ErrOutcomeUnknown) && errors.Is(err, githost.ErrRetryable) {
				t.Errorf("Do = %v: an unknown outcome is retryable", err)
			}
			if errors.Is(err, errDenied) {
				t.Errorf("Do = %v keeps the check's error", err)
			}
			if c.wantState != 0 {
				var he *githost.Error
				if !errors.As(err, &he) || he.Status != c.wantState {
					t.Errorf("Do = %v, want status %d in the chain", err, c.wantState)
				}
			}
			assertScrubbed(t, s, err)
			if n := s.hits.Load(); n != c.wantHits {
				t.Errorf("server got %d requests, want %d", n, c.wantHits)
			}
		})
	}
}

// TestUnusableReceipt: a 2xx answer to a write whose body is past the bound or cannot be read is
// ErrUnusableReceipt: the write happened, so it is never sent again and is not an unknown outcome.
func TestUnusableReceipt(t *testing.T) {
	const limit = 64
	bodies := map[string]func(w http.ResponseWriter){
		"declared over": func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", strconv.Itoa(limit+1))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, strings.Repeat("x", limit+1))
		},
		"chunked over": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, strings.Repeat("x", limit))
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "y")
		},
		"cut short": func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "50")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"sha":"`)
		},
	}
	for name, body := range bodies {
		for _, idem := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s idempotent=%v", name, idem), func(t *testing.T) {
				s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { body(w) })
				h := newHarness(t, s, nil)
				r := post(idem)
				r.Limit = limit
				resp, err := h.c.Do(t.Context(), r)
				var he *githost.Error
				if !errors.Is(err, githost.ErrUnusableReceipt) || !errors.As(err, &he) || he.Status != http.StatusCreated {
					t.Fatalf("Do = %v, want ErrUnusableReceipt with status 201", err)
				}
				if errors.Is(err, githost.ErrRetryable) || errors.Is(err, githost.ErrOutcomeUnknown) || resp.Body != nil {
					t.Errorf("Do = %v, %q: retryable, unknown or with a body", err, resp.Body)
				}
				assertScrubbed(t, s, err)
				if n := s.hits.Load(); n != 1 {
					t.Errorf("server got %d requests, want 1", n)
				}
			})
		}
	}
}

// TestRefusalClass: a write check that refuses with a valid Refusal gets its class back through
// errors.As, as a copy; its other text and its own error never reach the caller.
func TestRefusalClass(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantClass string // empty means no Refusal in the chain
	}{
		{"wrapped refusal", fmt.Errorf("%s: %w", checkCanary, &githost.Refusal{Class: "paused"}), "paused"},
		{"bare refusal", &githost.Refusal{Class: "blocked"}, "blocked"},
		{"joined refusal", errors.Join(errors.New(checkCanary), &githost.Refusal{Class: "rule-3"}), "rule-3"},
		{"canary class", &githost.Refusal{Class: checkCanary}, ""},
		{"spaced class", &githost.Refusal{Class: "paused " + strings.ToLower(checkCanary)}, ""},
		{"long class", &githost.Refusal{Class: strings.Repeat("a", 33)}, ""},
		{"empty class", &githost.Refusal{}, ""},
		{"plain error", errors.New(checkCanary), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
			h := newHarness(t, s, func(cfg *Config) {
				cfg.WriteCheck = func(context.Context, Send) error { return c.err }
			})
			_, err := h.c.Do(t.Context(), post(true))
			if !errors.Is(err, githost.ErrStoppedBeforeWrite) || errors.Is(err, githost.ErrRetryable) {
				t.Fatalf("Do = %v, want ErrStoppedBeforeWrite", err)
			}
			if errors.Is(err, c.err) {
				t.Errorf("Do = %v keeps the check's error", err)
			}
			var ref *githost.Refusal
			got := errors.As(err, &ref)
			if got != (c.wantClass != "") || (got && ref.Class != c.wantClass) {
				t.Errorf("Do = %v, refusal %v %+v; want class %q", err, got, ref, c.wantClass)
			}
			if got {
				var orig *githost.Refusal
				if errors.As(c.err, &orig) && orig == ref {
					t.Error("the caller's Refusal is returned as is, not copied")
				}
			}
			if strings.Contains(err.Error(), strings.ToLower(checkCanary)) {
				t.Errorf("error text %q holds the check's text", err)
			}
			assertScrubbed(t, s, err)
			if n := s.hits.Load(); n != 0 {
				t.Errorf("server got %d requests", n)
			}
		})
	}
}
