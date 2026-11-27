package transport

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/shadewright-labs/armory/githost"
)

// TestNoResendBelowTheCheck: when the host reads a write on a reused connection and closes it without an
// answer, Go's transport does not send the write again on a new connection by itself. Every write that
// reaches the host follows a write check of its own. A fixed Idempotency-Key header, which would let Go
// resend a bodiless write, is refused when the client is built.
func TestNoResendBelowTheCheck(t *testing.T) {
	bodiless := func(method string, idempotent bool) Request {
		r := post(idempotent)
		r.Method = method
		r.Body = nil
		return r
	}
	withBody := func(method string) Request {
		r := post(false)
		r.Method = method
		return r
	}
	cases := []struct {
		name   string
		req    Request
		header http.Header
	}{
		{"bodiless POST", bodiless(http.MethodPost, false), nil},
		{"bodiless PUT", bodiless(http.MethodPut, false), nil},
		{"bodiless PATCH", bodiless(http.MethodPatch, false), nil},
		{"POST with a body", withBody(http.MethodPost), nil},
		{"PUT with a body", withBody(http.MethodPut), nil},
		{"bodiless POST marked idempotent", bodiless(http.MethodPost, true), nil},
		{"bodiless POST with an Idempotency-Key header", bodiless(http.MethodPost, false),
			http.Header{"Idempotency-Key": {"k1"}}},
		{"bodiless POST with an X-Idempotency-Key header", bodiless(http.MethodPost, false),
			http.Header{"X-Idempotency-Key": {"k1"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				perConn = map[string]int{}
				writes  int
				dropped bool
			)
			s := newServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				mu.Lock()
				reused := perConn[r.RemoteAddr] > 0
				perConn[r.RemoteAddr]++
				write := r.Method != http.MethodGet
				if write {
					writes++
				}
				drop := write && reused && !dropped
				if drop {
					dropped = true
				}
				mu.Unlock()
				if drop {
					// The request is read; the connection closes with no answer.
					dropConn(w)
					return
				}
				okBody(w)
			})
			h, err := tryHarness(s, func(cfg *Config) { cfg.Header = c.header })
			switch {
			case c.header != nil && errors.Is(err, githost.ErrInvalidConfig):
				return
			case err != nil:
				t.Fatal(err)
			case c.header != nil:
				// Go on, to show what the header would let through.
				t.Errorf("New with %v succeeded, want ErrInvalidConfig", c.header)
			}
			// A read leaves an idle connection for the write to reuse.
			if _, err := h.c.Do(t.Context(), get()); err != nil {
				t.Fatal(err)
			}
			_, err = h.c.Do(t.Context(), c.req)
			mu.Lock()
			defer mu.Unlock()
			if !dropped {
				t.Fatal("the write did not go out on the reused connection")
			}
			h.mu.Lock()
			checks := len(h.checks)
			h.mu.Unlock()
			if writes != checks {
				t.Errorf("host got %d writes after %d write checks (Do = %v)", writes, checks, err)
			}
			if !c.req.Idempotent && !errors.Is(err, githost.ErrOutcomeUnknown) {
				t.Errorf("Do = %v, want ErrOutcomeUnknown", err)
			}
		})
	}
}
