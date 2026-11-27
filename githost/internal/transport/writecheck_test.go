package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/githost"
)

// TestWriteCheckRefusalWinsOverContextEnd: a write check that refuses while the call's context ends
// stops the call. The error matches ErrStoppedBeforeWrite, the end of the context and a valid refusal
// class, is never retryable, and the write is not checked or sent again.
func TestWriteCheckRefusalWinsOverContextEnd(t *testing.T) {
	waitDeadline := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	cases := []struct {
		name      string
		req       Request
		attempts  int
		first502  bool
		cancel    bool // the check cancels the call's context instead of waiting for its deadline
		refusal   error
		want      []error
		wantClass string
		wantHits  int32
	}{
		{name: "idempotent, deadline during the check", req: post(true), attempts: 4,
			want: []error{githost.ErrStoppedBeforeWrite, context.DeadlineExceeded}},
		{name: "not idempotent, deadline during the check", req: post(false), attempts: 4,
			want: []error{githost.ErrStoppedBeforeWrite, context.DeadlineExceeded}},
		{name: "after a lost attempt, deadline during the check", req: post(true), attempts: 2, first502: true,
			want:     []error{githost.ErrOutcomeUnknown, githost.ErrStoppedBeforeWrite, context.DeadlineExceeded},
			wantHits: 1},
		{name: "canceled with a refusal class", req: post(true), attempts: 4, cancel: true,
			refusal: &githost.Refusal{Class: classPaused},
			want:    []error{githost.ErrStoppedBeforeWrite, context.Canceled}, wantClass: classPaused},
		{name: "deadline with a refusal class", req: post(true), attempts: 4,
			refusal: &githost.Refusal{Class: classPaused},
			want:    []error{githost.ErrStoppedBeforeWrite, context.DeadlineExceeded}, wantClass: classPaused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				if c.first502 {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				okBody(w)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			var checks atomic.Int32
			h := newHarness(t, s, func(cfg *Config) {
				cfg.MaxAttempts = c.attempts
				cfg.WriteCheck = func(cctx context.Context, sd Send) error {
					checks.Add(1)
					if c.first502 && sd.Attempt == 1 {
						return nil
					}
					if c.cancel {
						cancel()
					}
					err := waitDeadline(cctx)
					if c.refusal != nil {
						return c.refusal
					}
					return err
				}
			})
			_, err := h.c.Do(ctx, c.req)
			for _, w := range c.want {
				if !errors.Is(err, w) {
					t.Errorf("Do = %v, want a match for %v", err, w)
				}
			}
			if errors.Is(err, githost.ErrRetryable) {
				t.Errorf("Do = %v matches ErrRetryable", err)
			}
			var ref *githost.Refusal
			if got := errors.As(err, &ref); got != (c.wantClass != "") || (got && ref.Class != c.wantClass) {
				t.Errorf("Do = %v, refusal %v %+v; want class %q", err, got, ref, c.wantClass)
			}
			if n := strings.Count(err.Error(), "githost: "); n > 2 {
				t.Errorf("error text %q repeats the prefix %d times", err, n)
			}
			wantChecks := int32(1)
			if c.first502 {
				wantChecks = 2
			}
			if checks.Load() != wantChecks || s.hits.Load() != c.wantHits {
				t.Errorf("checks = %d, hits = %d; want %d and %d", checks.Load(), s.hits.Load(), wantChecks, c.wantHits)
			}
			assertScrubbed(t, s, err)
		})
	}
}

// TestNilWriteCheckAsksNoToken: a nil write check refuses every write before the credential is asked,
// so no write token is minted for a write that will not be sent and no credential error hides the
// refusal. Reads still ask for a read token.
func TestNilWriteCheckAsksNoToken(t *testing.T) {
	for name, credErr := range map[string]error{
		"token":          nil,
		caseRetryable:    &githost.RetryableError{Err: errors.New(tokenCanary)},
		caseUnauthorized: githost.ErrUnauthorized,
		"plain failure":  errors.New(tokenCanary),
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
			var writes, reads atomic.Int32
			h := newHarness(t, s, func(cfg *Config) {
				cfg.WriteCheck = nil
				cfg.MaxAttempts = 3
				cfg.Credential = githost.CredentialFunc(func(_ context.Context, sc githost.Scope) (githost.Token, error) {
					if sc.Write {
						writes.Add(1)
					} else {
						reads.Add(1)
					}
					if credErr != nil {
						return githost.Token{}, credErr
					}
					return githost.NewToken(githost.Bearer, tokenCanary, time.Time{}), nil
				})
			})
			_, err := h.c.Do(t.Context(), post(true))
			if !errors.Is(err, githost.ErrStoppedBeforeWrite) || errors.Is(err, githost.ErrRetryable) {
				t.Fatalf("Do = %v, want ErrStoppedBeforeWrite and not retryable", err)
			}
			if writes.Load() != 0 || s.hits.Load() != 0 || len(h.waits) != 0 {
				t.Errorf("write tokens = %d, hits = %d, waits = %v; want none", writes.Load(), s.hits.Load(), h.waits)
			}
			assertScrubbed(t, s, err)
			if credErr == nil {
				if _, err := h.c.Do(t.Context(), get()); err != nil || reads.Load() != 1 {
					t.Errorf("read: Do = %v, read tokens = %d; want success and 1", err, reads.Load())
				}
			}
		})
	}
}
