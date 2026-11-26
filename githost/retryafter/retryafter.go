// Package retryafter reads the delay an HTTP server asks a client to wait: the Retry-After header (RFC
// 9110, section 10.2.3) and the rate limit headers GitHub sends with an exhausted quota.
//
// Every delay is bounded by a maximum the caller passes, and a value that cannot be parsed, carries a
// sign or lies in the past gives zero. A number is ASCII digits only; one past the integer limit is the
// largest number, so it asks for the maximum. [Header] reads the reset headers when Retry-After is absent or not a
// valid value. The package keeps nothing, makes no request, and imports only the
// standard library. It is not internal, so callers outside githost can use it for their own clients.
package retryafter

import (
	"math"
	"net/http"
	"strings"
	"time"
)

// Parse reads a Retry-After value in seconds or as an HTTP date and returns the delay it asks for, capped
// at maxDelay. Seconds are ASCII digits only, without a sign; a run of digits past the integer limit asks
// for maxDelay. A date is measured from now. A past date, a signed number or an unparsable value gives
// zero, and so does a maxDelay of zero or less.
func Parse(v string, now time.Time, maxDelay time.Duration) time.Duration {
	d, _ := parse(v, now, maxDelay)
	return d
}

// parse is Parse that also reports whether v is a valid Retry-After value: digits only (delta-seconds)
// or an HTTP date, past dates included.
func parse(v string, now time.Time, maxDelay time.Duration) (time.Duration, bool) {
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
// of now. A reset at or before now, a signed number or an unparsable value gives zero, and so does a
// maxDelay of zero or less.
func ParseReset(v string, now time.Time, maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		return 0
	}
	reset, ok := digits(strings.TrimSpace(v))
	if !ok {
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
// (a past date or "0" included, which ask for none), otherwise the time until X-RateLimit-Reset when
// X-RateLimit-Remaining is "0". A Retry-After that is present but not a valid value does not hide the
// reset. The delay is capped at maxDelay; zero means the headers ask for no delay.
func Header(h http.Header, now time.Time, maxDelay time.Duration) time.Duration {
	if d, ok := parse(h.Get("Retry-After"), now, maxDelay); ok {
		return d
	}
	if strings.TrimSpace(h.Get("X-RateLimit-Remaining")) == "0" {
		return ParseReset(h.Get("X-RateLimit-Reset"), now, maxDelay)
	}
	return 0
}
