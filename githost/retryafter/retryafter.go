// Package retryafter reads the delay an HTTP server asks a client to wait: the Retry-After header (RFC
// 9110, section 10.2.3) and the rate limit headers GitHub sends with an exhausted quota.
//
// Every delay is bounded by a maximum the caller passes, and inputs longer than 128 bytes are rejected
// without scanning. A value that cannot be parsed or carries a sign gives zero and [ErrInvalid], so a
// caller can tell it from a valid value that asks for no delay: "0", or a date or reset in the past,
// gives zero and a nil error, and means the request may be sent again at once. A number is ASCII digits
// only; one past the integer limit is the largest number, so it asks for the maximum. [Header] reads the
// reset headers when Retry-After is absent or not a valid value, and returns [ErrMissing] when the
// headers ask for nothing. The package keeps nothing, makes no request, and imports only the standard
// library. It is not internal, so callers outside githost can use it for their own clients.
package retryafter

import (
	"errors"
	"math"
	"net/http"
	"strings"
	"time"
)

// maxValueLen bounds the byte length of any header value the package parses. An HTTP-date is at most
// 35 bytes and an integer well past math.MaxInt64 is a few dozen digits, so longer inputs are rejected
// without scanning them.
const maxValueLen = 128

var (
	// ErrInvalid means a value is present but is not a valid delay: not ASCII digits or an HTTP date,
	// signed, or longer than 128 bytes. Its text never quotes the value.
	ErrInvalid = errors.New("retryafter: invalid value")
	// ErrMissing means the headers carry no Retry-After and no exhausted quota, so they ask for no delay
	// of their own.
	ErrMissing = errors.New("retryafter: no delay asked for")
)

// Parse reads a Retry-After value in seconds or as an HTTP date and returns the delay it asks for, capped
// at maxDelay. Seconds are ASCII digits only, without a sign; a run of digits past the integer limit asks
// for maxDelay. A date is measured from now. "0" and a past date are valid and give zero: retry at once.
// An empty input, an input longer than 128 bytes, a signed number or an unparsable value gives zero and
// [ErrInvalid]. A valid value with a maxDelay of zero or less gives zero and a nil error.
func Parse(v string, now time.Time, maxDelay time.Duration) (time.Duration, error) {
	d, ok := parse(v, now, maxDelay)
	if !ok {
		return 0, ErrInvalid
	}
	return d, nil
}

// parse is Parse that reports whether v is a valid Retry-After value: digits only (delta-seconds) or an
// HTTP date, past dates included.
func parse(v string, now time.Time, maxDelay time.Duration) (time.Duration, bool) {
	if len(v) > maxValueLen {
		return 0, false
	}
	v = strings.TrimSpace(v)
	if secs, ok := digits(v); ok {
		if maxDelay <= 0 {
			return 0, true
		}
		// Compare in seconds first so secs*time.Second cannot overflow Duration, even at math.MaxInt64.
		if secs > int64(maxDelay/time.Second) {
			return maxDelay, true
		}
		return time.Duration(secs) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	if maxDelay <= 0 {
		return 0, true
	}
	return min(max(t.Sub(now), 0), maxDelay), true
}

// digits reads a non-empty run of ASCII digits with no sign. A value past the integer limit is
// math.MaxInt64: it is still a valid number, and the largest.
func digits(v string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	var n int64
	for i := range len(v) {
		c := v[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if n > (math.MaxInt64-int64(c-'0'))/10 {
			n = math.MaxInt64
			continue
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

// ParseReset reads a rate limit reset time in Unix seconds, as GitHub sends in X-RateLimit-Reset, and
// returns the delay from now until then, capped at maxDelay. The reset is ASCII digits only, without a
// sign; a run of digits past the integer limit asks for maxDelay. The delay counts the sub-second part
// of now. A reset at or before now is valid and gives zero: retry at once. An empty input, an input
// longer than 128 bytes, a signed number or an unparsable value gives zero and [ErrInvalid]. A valid
// value with a maxDelay of zero or less gives zero and a nil error.
func ParseReset(v string, now time.Time, maxDelay time.Duration) (time.Duration, error) {
	if len(v) > maxValueLen {
		return 0, ErrInvalid
	}
	reset, ok := digits(strings.TrimSpace(v))
	if !ok {
		return 0, ErrInvalid
	}
	return resetDelay(reset, now, maxDelay), nil
}

// resetDelay is the delay from now until the Unix time reset, capped at maxDelay.
func resetDelay(reset int64, now time.Time, maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		return 0
	}
	nowSecs := now.Unix()
	if reset < nowSecs {
		return 0
	}
	// Both are known to be in order, and reset is not negative, so the difference only overflows when
	// now is far before 1970; it is then past any cap.
	secs := reset - nowSecs
	maxSecs := int64(maxDelay / time.Second)
	if secs < 0 || secs > maxSecs+1 {
		return maxDelay
	}
	if secs == maxSecs+1 {
		// secs*time.Second can overflow when maxDelay is near math.MaxInt64; compute from maxSecs instead.
		extra := time.Second - time.Duration(now.Nanosecond())
		base := time.Duration(maxSecs) * time.Second
		if extra >= maxDelay-base {
			return maxDelay
		}
		return base + extra
	}
	// secs <= maxSecs, so secs*time.Second cannot overflow.
	d := time.Duration(secs)*time.Second - time.Duration(now.Nanosecond())
	return min(max(d, 0), maxDelay)
}

// Header returns the delay the headers of a response ask for: Retry-After when it holds a valid value
// (a past date or "0" included, which ask to retry at once), otherwise the time until X-RateLimit-Reset
// when X-RateLimit-Remaining is "0". A Retry-After that is present but not a valid value does not hide
// the reset. The delay is capped at maxDelay.
//
// The error is nil when a header asked for the delay, zero included. It is [ErrInvalid] when a
// Retry-After or an exhausted quota is present but no value is usable, and [ErrMissing] when the
// headers carry neither; the delay is zero then, and the caller picks its own.
func Header(h http.Header, now time.Time, maxDelay time.Duration) (time.Duration, error) {
	hasRetry := len(h.Values("Retry-After")) > 0
	if hasRetry {
		if d, ok := parse(h.Get("Retry-After"), now, maxDelay); ok {
			return d, nil
		}
	}
	if rem := h.Get("X-RateLimit-Remaining"); len(rem) <= maxValueLen && strings.TrimSpace(rem) == "0" {
		return ParseReset(h.Get("X-RateLimit-Reset"), now, maxDelay)
	}
	if hasRetry {
		return 0, ErrInvalid
	}
	return 0, ErrMissing
}
