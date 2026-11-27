package transport

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/shadewright-labs/armory/githost"
)

// TestEarlierAttemptUnknown: an idempotent write answered 502 and then 201 with a body the host client
// cannot decode. The Response says an earlier attempt may have written, and UnusableReceipt builds the
// error that says both: this write was accepted, and whether the earlier one was applied is unknown.
func TestEarlierAttemptUnknown(t *testing.T) {
	cases := []struct {
		name    string
		first   int // the status of the first answer; zero means only one attempt
		write   bool
		earlier bool
	}{
		{"502 then 201", http.StatusBadGateway, true, true},
		{"lost then 201", -1, true, true},
		{"429 then 201", http.StatusTooManyRequests, true, false},
		{"201 only", 0, true, false},
		{"read after 502", http.StatusBadGateway, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				switch {
				case n == 1 && c.first == -1:
					dropConn(w)
				case n == 1 && c.first > 0:
					w.Header().Set(hRetry, "1")
					w.WriteHeader(c.first)
				default:
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, "{")
				}
			})
			h := newHarness(t, s, nil)
			r := get()
			if c.write {
				r = post(true)
			}
			resp, err := h.c.Do(t.Context(), r)
			if err != nil || resp.Status != http.StatusCreated || string(resp.Body) != "{" {
				t.Fatalf("Do = %+v, %v; want the 201", resp, err)
			}
			if resp.EarlierAttemptUnknown != c.earlier {
				t.Fatalf("EarlierAttemptUnknown = %v, want %v", resp.EarlierAttemptUnknown, c.earlier)
			}
			uerr := resp.UnusableReceipt(opTest)
			var he *githost.Error
			if !errors.As(uerr, &he) || he.Status != http.StatusCreated || he.Op != opTest {
				t.Errorf("UnusableReceipt = %v, want status 201 and the label", uerr)
			}
			if errors.Is(uerr, githost.ErrRetryable) {
				t.Errorf("UnusableReceipt = %v is retryable", uerr)
			}
			if !c.write {
				if errors.Is(uerr, githost.ErrUnusableReceipt) || errors.Is(uerr, githost.ErrOutcomeUnknown) {
					t.Errorf("UnusableReceipt of a read = %v, want neither write kind", uerr)
				}
				return
			}
			if !errors.Is(uerr, githost.ErrUnusableReceipt) || errors.Is(uerr, githost.ErrOutcomeUnknown) != c.earlier {
				t.Errorf("UnusableReceipt = %v; want ErrUnusableReceipt, ErrOutcomeUnknown %v", uerr, c.earlier)
			}
		})
	}
}

// TestUnusableReceiptOfZeroResponse: a Response that was never returned by Do gives a fixed error, not a
// receipt.
func TestUnusableReceiptOfZeroResponse(t *testing.T) {
	err := Response{}.UnusableReceipt(opTest)
	if err == nil || errors.Is(err, githost.ErrUnusableReceipt) || errors.Is(err, githost.ErrOutcomeUnknown) {
		t.Errorf("UnusableReceipt of a zero Response = %v", err)
	}
}
