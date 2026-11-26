// Package retryafter reads the delay an HTTP server asks a client to wait: the Retry-After header (RFC
// 9110, section 10.2.3) and the rate limit headers GitHub sends with an exhausted quota.
//
// Every delay is bounded by a maximum the caller passes, and a value that cannot be parsed, is negative
// or lies in the past gives zero. [Header] reads the reset headers when Retry-After is absent or not a
// valid value. The package keeps nothing, makes no request, and imports only the
// standard library. It is not internal, so callers outside githost can use it for their own clients.
package retryafter

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Parse reads a Retry-After value in seconds or as an HTTP date and returns the delay it asks for, capped
// at maxDelay. A date is measured from now. A past date, a negative number or an unparsable value gives
// zero, and so does a maxDelay of zero or less.
func Parse(v string, now time.Time, maxDelay time.Duration) time.Duration {
	d, _ := parse(v, now, maxDelay)
	return d
}

// parse is Parse that also reports whether v is a valid Retry-After value: a non-negative number of
// seconds or an HTTP date, past dates included.
func parse(v string, now time.Time, maxDelay time.Duration) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	var d time.Duration
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		// Capped in seconds first: a huge value must not overflow the Duration.
		d = time.Duration(min(secs, int64(max(maxDelay, 0)/time.Second)+1)) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	} else {
		return 0, false
	}
	if maxDelay <= 0 {
		return 0, true
	}
	return min(max(d, 0), maxDelay), true
}

// ParseReset reads a rate limit reset time in Unix seconds, as GitHub sends in X-RateLimit-Reset, and
// returns the delay from now until then, capped at maxDelay. A past time or an unparsable value gives zero.
func ParseReset(v string, now time.Time, maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		return 0
	}
	reset, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0
	}
	// Compared in seconds first: time.Unix of a huge value must not overflow the subtraction.
	secs := reset - now.Unix()
	if secs <= 0 {
		return 0
	}
	if secs > int64(maxDelay/time.Second) {
		return maxDelay
	}
	return min(max(time.Unix(reset, 0).Sub(now), 0), maxDelay)
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
