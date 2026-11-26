package retryafter

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

const (
	hRetry     = "Retry-After"
	hRemaining = "X-RateLimit-Remaining"
	hReset     = "X-RateLimit-Reset"
	// unusable is a header value that is neither a number nor a date.
	unusable = "soon"
)

var now = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func TestParse(t *testing.T) {
	const maxDelay = 10 * time.Minute
	cases := map[string]time.Duration{
		"":                    0,
		"7":                   7 * time.Second,
		" 7 ":                 7 * time.Second, //nolint:gocritic // spaces around the value are what this case checks
		"0":                   0,
		"-5":                  0,
		"1.5":                 0,
		"600":                 maxDelay,
		"601":                 maxDelay,
		"99999999999999":      maxDelay,
		"9223372036854775807": maxDelay,
		unusable:              0,
		now.Add(20 * time.Second).Format(http.TimeFormat): 20 * time.Second,
		now.Add(time.Hour).Format(http.TimeFormat):        maxDelay,
		now.Add(-time.Minute).Format(http.TimeFormat):     0,
		now.Add(20 * time.Second).Format(time.RFC850):     20 * time.Second,
	}
	for v, want := range cases {
		if got := Parse(v, now, maxDelay); got != want {
			t.Errorf("Parse(%q) = %s, want %s", v, got, want)
		}
	}
	if got := Parse("7", now, 0); got != 0 {
		t.Errorf("Parse with no cap = %s, want 0", got)
	}
}

func TestHeader(t *testing.T) {
	reset := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	cases := []struct {
		name string
		kv   []string
		want time.Duration
	}{
		{"none", nil, 0},
		{"retry-after seconds", []string{hRetry, "12"}, 12 * time.Second},
		{"retry-after capped", []string{hRetry, "999999"}, time.Hour},
		{"retry-after date", []string{hRetry, now.Add(20 * time.Second).Format(http.TimeFormat)}, 20 * time.Second},
		{"retry-after wins over reset", []string{hRetry, "3", hRemaining, "0",
			hReset, reset(time.Minute)}, 3 * time.Second},
		{"quota reset", []string{hRemaining, "0", hReset, reset(45 * time.Second)},
			45 * time.Second},
		{"quota reset capped", []string{hRemaining, "0", hReset, reset(48 * time.Hour)},
			time.Hour},
		{"quota reset huge", []string{hRemaining, "0", hReset, "9223372036854775807"},
			time.Hour},
		{"quota reset in the past", []string{hRemaining, "0", hReset, reset(-time.Minute)}, 0},
		{"quota reset not a number", []string{hRemaining, "0", hReset, unusable}, 0},
		{"quota left", []string{hRemaining, "10", hReset, reset(time.Minute)}, 0},
		{"retry-after zero wins over reset", []string{hRetry, "0", hRemaining, "0",
			hReset, reset(time.Minute)}, 0},
		{"retry-after unusable, reset used", []string{hRetry, unusable, hRemaining, "0",
			hReset, reset(45 * time.Second)}, 45 * time.Second},
		{"retry-after negative, reset used", []string{hRetry, "-5", hRemaining, "0",
			hReset, reset(45 * time.Second)}, 45 * time.Second},
		{"retry-after usable, reset unusable", []string{hRetry, "3", hRemaining, "0", hReset, unusable},
			3 * time.Second},
		{"both unusable", []string{hRetry, unusable, hRemaining, "0", hReset, "later"}, 0},
		{"retry-after unusable, quota left", []string{hRetry, unusable, hRemaining, "10",
			hReset, reset(time.Minute)}, 0},
	}
	for _, c := range cases {
		h := http.Header{}
		for i := 0; i+1 < len(c.kv); i += 2 {
			h.Set(c.kv[i], c.kv[i+1])
		}
		if got := Header(h, now, time.Hour); got != c.want {
			t.Errorf("%s: Header = %s, want %s", c.name, got, c.want)
		}
	}
}

// FuzzParse: no input panics, and every delay lies between zero and the cap.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"", "7", "-1", "9223372036854775807", now.Format(http.TimeFormat), "Sun, 06 Nov 1994"} {
		f.Add(s, now.Unix(), int64(time.Hour))
	}
	f.Fuzz(func(t *testing.T, v string, nowUnix, maxDelay int64) {
		at := time.Unix(nowUnix, 0)
		d := Parse(v, at, time.Duration(maxDelay))
		if d < 0 || (maxDelay <= 0 && d != 0) || (maxDelay > 0 && d > time.Duration(maxDelay)) {
			t.Fatalf("Parse(%q, %d, %d) = %d", v, nowUnix, maxDelay, d)
		}
	})
}

// FuzzHeader: the rate limit headers give a delay between zero and the cap, whatever they hold.
func FuzzHeader(f *testing.F) {
	f.Add("", "0", strconv.FormatInt(now.Unix()+30, 10), now.Unix(), int64(time.Hour))
	f.Add("12", "0", "1", now.Unix(), int64(time.Minute))
	f.Add("", "0", "-9223372036854775808", int64(-1), int64(time.Hour))
	f.Fuzz(func(t *testing.T, retry, remaining, reset string, nowUnix, maxDelay int64) {
		h := http.Header{}
		if retry != "" {
			h.Set(hRetry, retry)
		}
		h.Set(hRemaining, remaining)
		h.Set(hReset, reset)
		d := Header(h, time.Unix(nowUnix, 0), time.Duration(maxDelay))
		if d < 0 || (maxDelay <= 0 && d != 0) || (maxDelay > 0 && d > time.Duration(maxDelay)) {
			t.Fatalf("Header(%q, %q, %q) = %d", retry, remaining, reset, d)
		}
	})
}
