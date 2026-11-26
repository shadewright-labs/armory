package githost

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	opBranchHead = "BranchHead"
	opGet        = "Get"
)

func TestErrorKinds(t *testing.T) {
	all := []error{ErrNotFound, ErrPathNotInTree, ErrConflict, ErrForbidden, ErrUnauthorized, ErrRejected,
		ErrTooLarge, ErrInvalidEntry, ErrUntrustedAnswer, ErrUnsupported, ErrInvalidConfig,
		ErrStoppedBeforeWrite, ErrOutcomeUnknown, ErrUnusableReceipt, ErrRetryable}
	cases := []struct {
		name string
		err  error
		is   []error
	}{
		{"path not in tree is not found", ErrPathNotInTree, []error{ErrPathNotInTree, ErrNotFound}},
		{"status error", &Error{Op: opBranchHead, Status: 409, Kind: ErrConflict}, []error{ErrConflict}},
		{"wrapped status error", fmt.Errorf("x: %w", &Error{Op: "Merge", Kind: ErrForbidden}), []error{ErrForbidden}},
		{"status without kind", &Error{Op: opGet, Status: 302}, nil},
		{"retryable", &RetryableError{RetryAfter: time.Second, Err: &Error{Op: opGet, Status: 503}},
			[]error{ErrRetryable}},
		{"retryable with kind", &RetryableError{Err: &Error{Op: opGet, Kind: ErrTooLarge}},
			[]error{ErrRetryable, ErrTooLarge}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range all {
				want := false
				for _, w := range c.is {
					want = want || errors.Is(w, k) && errors.Is(k, w)
				}
				if got := errors.Is(c.err, k); got != want {
					t.Errorf("errors.Is(%v, %v) = %v, want %v", c.err, k, got, want)
				}
			}
		})
	}
}

func TestErrorAs(t *testing.T) {
	err := fmt.Errorf("outer: %w", &RetryableError{RetryAfter: 3 * time.Second,
		Err: &Error{Op: "ListPullRequests", Status: 429}})
	var re *RetryableError
	if !errors.As(err, &re) || re.RetryAfter != 3*time.Second {
		t.Fatalf("errors.As RetryableError: %v", err)
	}
	var he *Error
	if !errors.As(err, &he) || he.Status != 429 || he.Op != "ListPullRequests" {
		t.Fatalf("errors.As Error: %v", err)
	}
}

func TestErrorText(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&Error{Op: opBranchHead, Status: 404, Kind: ErrNotFound}, "githost: BranchHead: status 404: not found"},
		{&Error{Op: opBranchHead, Kind: ErrTooLarge}, "githost: BranchHead: too large"},
		{&Error{Op: opGet, Status: 301}, "githost: Get: status 301"},
		{&RetryableError{Err: &Error{Op: opGet, Status: 502}}, "retryable: githost: Get: status 502"},
		{&RetryableError{RetryAfter: 2 * time.Second, Err: &Error{Op: opGet, Status: 429}},
			"retryable (after 2s): githost: Get: status 429"},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}

func TestRefusalValid(t *testing.T) {
	cases := map[string]bool{
		"paused":                true,
		"blocked":               true,
		"policy-7":              true,
		"0":                     true,
		strings.Repeat("a", 32): true,
		strings.Repeat("a", 33): false,
		"":                      false,
		"Paused":                false,
		"has space":             false,
		"under_score":           false,
		"line\nbreak":           false,
		"dot.ted":               false,
		"https://example.test":  false,
		"caf\u00e9":             false,
	}
	for class, want := range cases {
		if got := (&Refusal{Class: class}).Valid(); got != want {
			t.Errorf("Refusal{%q}.Valid() = %v, want %v", class, got, want)
		}
	}
	var nilRefusal *Refusal
	if nilRefusal.Valid() {
		t.Error("a nil Refusal is valid")
	}
	if got := (&Refusal{Class: "paused"}).Error(); got != "refused: paused" {
		t.Errorf("Error() = %q", got)
	}
	if got := (&Refusal{Class: "Not A Class"}).Error(); got != "refused" {
		t.Errorf("Error() of an invalid class = %q, want the fixed text only", got)
	}
}

// TestNilErrorsDoNotPanic: a typed nil *Error or *RetryableError, which a callback may return by mistake,
// prints and unwraps without a panic.
func TestNilErrorsDoNotPanic(t *testing.T) {
	var e *Error
	var r *RetryableError
	for _, err := range []error{e, r} {
		_ = err.Error()
		_ = errors.Unwrap(err)
		_ = errors.Is(err, ErrNotFound)
	}
	if !errors.Is(r, ErrRetryable) {
		t.Error("a nil *RetryableError no longer matches ErrRetryable")
	}
}
