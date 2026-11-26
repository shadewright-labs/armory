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
	// commit, the pull request is merged, or a precondition the request carried, such as an expected old
	// value, did not hold. The caller reads state again.
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
	// the call. Neither the check's text nor its error is kept; a valid [Refusal] class is.
	ErrStoppedBeforeWrite = errors.New("stopped before the write")
	// ErrOutcomeUnknown means an attempt of a write may have reached the host and its answer was lost or
	// was a failure that does not say the write was refused (a network error, a timeout, 3xx, 408 or 5xx).
	// Every later failure in the same call (a refused check, a credential error, the end of the context,
	// a status, the last retry, an unusable receipt) keeps this kind next to its own, so a caller never
	// reads it as "nothing was written". It never matches ErrRetryable: the caller reads state, then
	// decides.
	ErrOutcomeUnknown = errors.New("outcome of the write unknown")
	// ErrUnusableReceipt means the host accepted a write (a 2xx answer) and the answer could not be used:
	// it was past the bound, could not be read to the end before the attempt deadline, or could not be
	// decoded. The host accepted that attempt's write (for 202, accepted it for processing, which may not
	// have completed yet), and what it created or changed is not known. For a 202 or another 2xx where no
	// body is expected (such as 204), a clean empty answer succeeds when the operation expects none; a
	// body read failure, timeout or oversized body still returns ErrUnusableReceipt. It is never retried,
	// and the caller reads state to learn the result and must not send the write again.
	//
	// When an earlier attempt of the same call may have reached the host, the error matches
	// ErrOutcomeUnknown too: at least one write was accepted, and whether the earlier one was also
	// applied is unknown, so a write that is not idempotent may have happened twice. Without such an
	// attempt it does not match ErrOutcomeUnknown.
	ErrUnusableReceipt = errors.New("write accepted, answer unusable")
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

// Error returns "githost: <Op>" followed by the status (when non-zero) and kind (when non-nil), or
// "githost: nil error" for a nil *Error.
func (e *Error) Error() string {
	if e == nil {
		return "githost: nil error"
	}
	s := "githost: " + e.Op
	if e.Status != 0 {
		s += ": status " + strconv.Itoa(e.Status)
	}
	if e.Kind != nil {
		s += ": " + e.Kind.Error()
	}
	return s
}

// Unwrap returns the kind; nil for a nil *Error.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Kind
}

// RetryableError is a transient failure, with the delay to wait before retrying when one applies. It
// matches [ErrRetryable] with [errors.Is], including a nil *RetryableError.
type RetryableError struct {
	// RetryAfter is the delay to wait before retrying, or zero when none applies. On errors from a
	// client it is the delay the host asked for (Retry-After or rate limit reset), a delay a
	// [Credential] asked for, or the client's one-minute fallback for a rate limit without a usable
	// delay. The client caps every delay at one hour.
	RetryAfter time.Duration
	// Err is the underlying error.
	Err error
}

// Error returns "retryable (after <RetryAfter>): <Err>" when RetryAfter is positive, "retryable: <Err>"
// otherwise, and "retryable" for a nil *RetryableError.
func (e *RetryableError) Error() string {
	if e == nil {
		return "retryable"
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("retryable (after %s): %v", e.RetryAfter, e.Err)
	}
	return fmt.Sprintf("retryable: %v", e.Err)
}

// Unwrap returns the underlying error; nil for a nil *RetryableError.
func (e *RetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is reports whether target is [ErrRetryable]. It returns true for ErrRetryable even on a nil
// *RetryableError.
func (e *RetryableError) Is(target error) bool { return target == ErrRetryable }

// maxRefusalClass bounds the length of a [Refusal] class.
const maxRefusalClass = 32

// Refusal lets a product's check before a write say which of its own refusals stopped the write, such
// as "paused" or "blocked", without free text. The check returns a *Refusal, alone or wrapped; when its
// Class is valid, the client's error (which matches [ErrStoppedBeforeWrite]) carries a new *Refusal with
// that class, found with [errors.As]. Nothing else of the check's error is kept, and an invalid class
// is dropped.
//
// The write check interface itself (its name and signature) is provisional and still being designed.
// What is fixed: the check runs on every attempt of a write (retries included), a refusal stops the
// send, and a refusal carries only a pattern-checked class.
type Refusal struct {
	// Class is a short constant the product chose: 1 to 32 lowercase ASCII letters, digits and dashes.
	Class string
}

// Error returns "refused" and the class when it is valid.
func (r *Refusal) Error() string {
	if r.Valid() {
		return "refused: " + r.Class
	}
	return "refused"
}

// Valid reports whether the class is 1 to 32 lowercase ASCII letters, digits and dashes.
func (r *Refusal) Valid() bool {
	if r == nil || r.Class == "" || len(r.Class) > maxRefusalClass {
		return false
	}
	for i := range len(r.Class) {
		c := r.Class[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}
