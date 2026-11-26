package githost

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Error kinds. Clients wrap them, so callers match them with [errors.Is].
var (
	// ErrNotFound means the object asked for does not exist.
	ErrNotFound = errors.New("not found")
	// ErrPathNotInTree means the client read the tree at the commit asked for and the path is not a regular
	// file or directory there (absent, a symbolic link or a submodule). It wraps ErrNotFound and, unlike a
	// plain ErrNotFound, never changes for the same commit and path.
	ErrPathNotInTree = fmt.Errorf("path not in tree: %w", ErrNotFound)
	// ErrConflict means the request was based on stale state: the head moved, the branch exists at another
	// commit, the pull request is merged, or an Expect field did not match. The caller reads state again.
	ErrConflict = errors.New("conflict")
	// ErrForbidden means the identity lacks a permission, or a rule or policy of the host refused the
	// request. A rate limit is never ErrForbidden but a [RetryableError].
	ErrForbidden = errors.New("forbidden")
	// ErrUnauthorized means the host refused the credential (HTTP 401).
	ErrUnauthorized = errors.New("unauthorized")
	// ErrRejected means the request was refused as invalid, by the host or by the client before the send,
	// and state does not explain it. The same request fails the same way.
	ErrRejected = errors.New("rejected")
	// ErrTooLarge means an answer, file, list or commit is past a bound. Nothing past the bound was kept.
	ErrTooLarge = errors.New("too large")
	// ErrInvalidEntry means a tree holds an entry no tree can hold: a bad name or an unknown mode.
	ErrInvalidEntry = errors.New("invalid tree entry")
	// ErrUntrustedAnswer means the host answered about another object than the one asked for. It is not
	// retried.
	ErrUntrustedAnswer = errors.New("untrusted answer")
	// ErrUnsupported means the bound host lacks a capability the call needs.
	ErrUnsupported = errors.New("unsupported by the host")
	// ErrInvalidConfig means the input a client is built from cannot be used. It is permanent.
	ErrInvalidConfig = errors.New("invalid configuration")
	// ErrStoppedBeforeWrite means the product's check before a write refused it: nothing more was sent in
	// the call. Neither the check's text nor its error is kept.
	ErrStoppedBeforeWrite = errors.New("stopped before the write")
	// ErrOutcomeUnknown means a write that is not idempotent was sent and its answer was lost. It is not
	// repeated in the call: the caller reads state, then decides.
	ErrOutcomeUnknown = errors.New("outcome of the write unknown")
	// ErrRetryable means a transient failure: the same request may succeed later. [*RetryableError]
	// matches it.
	ErrRetryable = errors.New("retryable")
)

// Error is a failure of one operation, with the HTTP status when there was an answer. Its text is the
// operation label, the status and the kind, and nothing else.
type Error struct {
	// Op is a fixed label of the operation, such as "BranchHead". It never holds caller or host text.
	Op string
	// Status is the HTTP status of the answer; zero when there was none.
	Status int
	// Kind is one of the sentinels of this package, or nil when the status has no kind.
	Kind error
}

func (e *Error) Error() string {
	s := "githost: " + e.Op
	if e.Status != 0 {
		s += ": status " + strconv.Itoa(e.Status)
	}
	if e.Kind != nil {
		s += ": " + e.Kind.Error()
	}
	return s
}

// Unwrap returns the kind.
func (e *Error) Unwrap() error { return e.Kind }

// RetryableError is a transient failure, with the delay the host asked for when it asked for one. It
// matches [ErrRetryable] with [errors.Is].
type RetryableError struct {
	// RetryAfter is the delay the host asked for; zero when it gave none.
	RetryAfter time.Duration
	// Err is the underlying error.
	Err error
}

func (e *RetryableError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("retryable (after %s): %v", e.RetryAfter, e.Err)
	}
	return fmt.Sprintf("retryable: %v", e.Err)
}

// Unwrap returns the underlying error.
func (e *RetryableError) Unwrap() error { return e.Err }

// Is reports whether target is [ErrRetryable].
func (e *RetryableError) Is(target error) bool { return target == ErrRetryable }
