package transport

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/githost"
)

// stallingBody returns a partial answer, then blocks until the request's context ends and reports a
// clean end of the body: the order in which a server that flushes part of a body and stops on
// cancellation can be read.
type stallingBody struct {
	r    *http.Request
	sent bool
}

func (b *stallingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, `{"partial":`), nil
	}
	<-b.r.Context().Done()
	return 0, io.EOF
}

func (*stallingBody) Close() error { return nil }

// TestBodyEndsAfterAttemptDeadline: a body that ends cleanly only after the attempt deadline passed is
// not taken. A read is retried and ends as a timeout; a write's 2xx is an unusable receipt.
func TestBodyEndsAfterAttemptDeadline(t *testing.T) {
	for _, write := range []bool{false, true} {
		var hits int
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			hits++
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &stallingBody{r: r},
				ContentLength: -1, Request: r}, nil
		})
		h := newHarness(t, newServer(t, nil), func(c *Config) {
			c.RoundTripper = rt
			c.AttemptTimeout = 50 * time.Millisecond
			c.MaxAttempts = 2
		})
		r := get()
		if write {
			r = post(true)
		}
		resp, err := h.c.Do(t.Context(), r)
		switch {
		case write:
			if !errors.Is(err, githost.ErrUnusableReceipt) || hits != 1 || resp.Body != nil {
				t.Errorf("write: Do = %q, %v after %d sends; want ErrUnusableReceipt after 1", resp.Body, err, hits)
			}
		default:
			if !errors.Is(err, githost.ErrRetryable) || !strings.Contains(err.Error(), "timed out") || hits != 2 ||
				resp.Body != nil {
				t.Errorf("read: Do = %q, %v after %d sends; want a retryable timeout after 2", resp.Body, err, hits)
			}
		}
	}
}
