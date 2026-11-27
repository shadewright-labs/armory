package retryafter_test

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/shadewright-labs/armory/githost/retryafter"
)

var now = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

// Parse reads seconds or an HTTP date. "0" is a valid answer that asks to retry at once, and the error
// tells an unreadable value from it.
func ExampleParse() {
	for _, v := range []string{"120", "0", now.Add(90 * time.Second).Format(http.TimeFormat), "7200", "soon"} {
		d, err := retryafter.Parse(v, now, time.Hour)
		fmt.Println(d, err, errors.Is(err, retryafter.ErrInvalid))
	}
	// Output:
	// 2m0s <nil> false
	// 0s <nil> false
	// 1m30s <nil> false
	// 1h0m0s <nil> false
	// 0s retryafter: invalid value true
}

// ParseReset reads the Unix time a rate limit resets at, as GitHub sends in X-RateLimit-Reset.
func ExampleParseReset() {
	d, err := retryafter.ParseReset(strconv.FormatInt(now.Add(45*time.Second).Unix(), 10), now, time.Hour)
	fmt.Println(d, err)
	d, err = retryafter.ParseReset(strconv.FormatInt(now.Add(-time.Minute).Unix(), 10), now, time.Hour)
	fmt.Println(d, err)
	// Output:
	// 45s <nil>
	// 0s <nil>
}

// Header reads the delay a whole answer asks for: Retry-After, else the reset of an exhausted quota.
// The error says when the headers asked for nothing, so the caller can pick its own delay.
func ExampleHeader() {
	limited := http.Header{}
	limited.Set("Retry-After", "soon") // unusable, so the reset is used
	limited.Set("X-RateLimit-Remaining", "0")
	limited.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(10*time.Second).Unix(), 10))
	fmt.Println(retryafter.Header(limited, now, time.Hour))

	_, err := retryafter.Header(http.Header{}, now, time.Hour)
	fmt.Println(errors.Is(err, retryafter.ErrMissing))
	// Output:
	// 10s <nil>
	// true
}
