package retryafter

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	hRetry     = "Retry-After"
	hRemaining = "X-RateLimit-Remaining"
	hReset     = "X-RateLimit-Reset"
	// unusable is a header value that is neither a number nor a date.
	unusable     = "soon"
	pastInt64Max = "9223372036854775808"
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

// FuzzParse: no input panics, and for any string, clock and cap, Parse and ParseReset never return
// a negative delay or one above the cap, and integer seconds past the cap always return the cap.
func FuzzParse(f *testing.F) {
	const maxDelay = 10 * time.Minute
	values := []string{
		"", "7", " 7 ", "0", "-5", "-1", "1.5", "600", "601", "99999999999999",
		"9223372036854775807", pastInt64Max, "99999999999999999999999999999999",
		"3600", "3599", "+7", "-0", "+0", "-9223372036854775808", "0x10", "7 7", "٧", "0007",
		unusable,
		now.Format(http.TimeFormat),
		now.Add(20 * time.Second).Format(http.TimeFormat),
		now.Add(time.Hour).Format(http.TimeFormat),
		now.Add(-time.Minute).Format(http.TimeFormat),
		now.Add(20 * time.Second).Format(time.RFC850),
		"Sun, 06 Nov 1994",
	}
	for _, s := range values {
		f.Add(s, now.Unix(), int64(0), int64(maxDelay))
	}
	f.Add("7", now.Unix(), int64(0), int64(0))
	f.Add("7", now.Unix(), int64(0), int64(-1))
	f.Add(pastInt64Max, now.Unix(), int64(900_000_000), int64(math.MaxInt64))
	f.Add("2", int64(1), int64(900_000_000), int64(500*time.Millisecond))
	f.Add("1", int64(math.MinInt64), int64(0), int64(time.Hour))
	f.Fuzz(func(t *testing.T, v string, nowUnix, nowNsec, capNs int64) {
		at := time.Unix(nowUnix, nowNsec)
		capDur := time.Duration(capNs)
		d := Parse(v, at, capDur)
		if d < 0 || (capDur <= 0 && d != 0) || (capDur > 0 && d > capDur) {
			t.Fatalf("Parse(%q, %v, %d) = %d", v, at, capDur, d)
		}
		if capDur > 0 {
			if secs, ok := digits(strings.TrimSpace(v)); ok {
				if secs > int64(capDur/time.Second) && d != capDur {
					t.Fatalf("Parse(%q, %v, %d) = %d, want cap %d", v, at, capDur, d, capDur)
				}
			}
		}
		r := ParseReset(v, at, capDur)
		if r < 0 || (capDur <= 0 && r != 0) || (capDur > 0 && r > capDur) {
			t.Fatalf("ParseReset(%q, %v, %d) = %d", v, at, capDur, r)
		}
	})
}

// FuzzHeader: for any header strings, clock and cap, Header never panics and never returns a negative
// delay or one above the cap.
func FuzzHeader(f *testing.F) {
	reset := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	seeds := []struct {
		retry, remaining, reset string
		nowUnix, nowNsec        int64
		capDur                  time.Duration
	}{
		{"", "", "", now.Unix(), 0, time.Hour},
		{"12", "", "", now.Unix(), 0, time.Hour},
		{"999999", "", "", now.Unix(), 0, time.Hour},
		{now.Add(20 * time.Second).Format(http.TimeFormat), "", "", now.Unix(), 0, time.Hour},
		{"3", "0", reset(time.Minute), now.Unix(), 0, time.Hour},
		{"", "0", reset(45 * time.Second), now.Unix(), 0, time.Hour},
		{"", "0", reset(48 * time.Hour), now.Unix(), 0, time.Hour},
		{"", "0", "9223372036854775807", now.Unix(), 0, time.Hour},
		{"", "0", pastInt64Max, now.Unix(), 0, time.Hour},
		{"", "0", reset(-time.Minute), now.Unix(), 0, time.Hour},
		{"", "0", unusable, now.Unix(), 0, time.Hour},
		{"", "10", reset(time.Minute), now.Unix(), 0, time.Hour},
		{"0", "0", reset(time.Minute), now.Unix(), 0, time.Hour},
		{unusable, "0", reset(45 * time.Second), now.Unix(), 0, time.Hour},
		{"-5", "0", reset(45 * time.Second), now.Unix(), 0, time.Hour},
		{"+7", "0", reset(45 * time.Second), now.Unix(), 0, time.Hour},
		{"-0", "0", reset(45 * time.Second), now.Unix(), 0, time.Hour},
		{"3", "0", unusable, now.Unix(), 0, time.Hour},
		{unusable, "0", "later", now.Unix(), 0, time.Hour},
		{unusable, "10", reset(time.Minute), now.Unix(), 0, time.Hour},
		{"", "0", "-9223372036854775808", 1, 0, time.Hour},
		{"", "0", "1", math.MinInt64, 0, time.Hour},
		{"", "0", "2", 1, 900_000_000, 500 * time.Millisecond},
		{"", "0", "3", 1, 900_000_000, 2 * time.Second},
		{"", "0", "1", 1, 900_000_000, time.Hour},
		{"", "0", "+7", 1, 0, time.Hour},
		{"", "0", "2", 1, 0, 300 * time.Millisecond},
		{"", "0", pastInt64Max, 0, 0, math.MaxInt64},
		{"12", "0", "1", now.Unix(), 0, 0},
		{"12", "0", "1", now.Unix(), 0, -time.Second},
	}
	for _, s := range seeds {
		f.Add(s.retry, s.remaining, s.reset, s.nowUnix, s.nowNsec, int64(s.capDur))
	}
	f.Fuzz(func(t *testing.T, retry, remaining, reset string, nowUnix, nowNsec, capNs int64) {
		h := http.Header{}
		if retry != "" {
			h.Set(hRetry, retry)
		}
		h.Set(hRemaining, remaining)
		h.Set(hReset, reset)
		at := time.Unix(nowUnix, nowNsec)
		capDur := time.Duration(capNs)
		d := Header(h, at, capDur)
		if d < 0 || (capDur <= 0 && d != 0) || (capDur > 0 && d > capDur) {
			t.Fatalf("Header(%q, %q, %q, %v, %d) = %d", retry, remaining, reset, at, capDur, d)
		}
	})
}

// TestParseAtTheLimits: a delta-seconds value is digits only, so a sign is not a value; and a run of
// digits past the integer limit still asks for the longest delay, never for none.
func TestParseAtTheLimits(t *testing.T) {
	cases := []struct {
		v    string
		want time.Duration
		ok   bool
	}{
		{pastInt64Max, time.Hour, true},
		{"99999999999999999999999999999999", time.Hour, true},
		{"9223372036854775807", time.Hour, true},
		{"3600", time.Hour, true},
		{"3599", 3599 * time.Second, true},
		{"+7", 0, false},
		{"-0", 0, false},
		{"+0", 0, false},
		{"-9223372036854775808", 0, false},
		{"0x10", 0, false},
		{"7 7", 0, false},
		{"٧", 0, false}, // a digit of another script
		{"0007", 7 * time.Second, true},
	}
	for _, c := range cases {
		d, ok := parse(c.v, now, time.Hour)
		if d != c.want || ok != c.ok {
			t.Errorf("parse(%q) = %s, %v; want %s, %v", c.v, d, ok, c.want, c.ok)
		}
	}
	// A cap of math.MaxInt64 must not overflow when multiplying seconds to Duration.
	for _, v := range []string{"9223372037", "9223372036854775807", pastInt64Max} {
		if d, ok := parse(v, now, math.MaxInt64); d != math.MaxInt64 || !ok {
			t.Errorf("parse(%q, MaxInt64) = %d, %v; want MaxInt64, true", v, d, ok)
		}
	}
	if d, ok := parse("9223372036", now, math.MaxInt64); d != 9223372036*time.Second || !ok {
		t.Errorf("parse(9223372036, MaxInt64) = %d, %v", d, ok)
	}
}

// TestParseResetAtTheLimits: a reset is measured from now with its sub-second part, never wraps at the
// integer limits, and a sign or a reset before now asks for no delay.
func TestParseResetAtTheLimits(t *testing.T) {
	cases := []struct {
		name     string
		v        string
		now      time.Time
		maxDelay time.Duration
		want     time.Duration
	}{
		{"most negative reset", "-9223372036854775808", time.Unix(1, 0), time.Hour, 0},
		{"negative reset", "-1", time.Unix(1, 0), time.Hour, 0},
		{"most negative now", "1", time.Unix(-9223372036854775808, 0), time.Hour, time.Hour},
		{"sub-second now", "2", time.Unix(1, 900_000_000), 500 * time.Millisecond, 100 * time.Millisecond},
		{"sub-second now, cap above", "3", time.Unix(1, 900_000_000), 2 * time.Second, 1100 * time.Millisecond},
		{"sub-second now, past", "1", time.Unix(1, 900_000_000), time.Hour, 0},
		{"plus sign", "+7", time.Unix(1, 0), time.Hour, 0},
		{"past the integer limit", pastInt64Max, time.Unix(1, 0), time.Hour, time.Hour},
		{"largest", "9223372036854775807", time.Unix(1, 0), time.Hour, time.Hour},
		{"cap below a second", "2", time.Unix(1, 0), 300 * time.Millisecond, 300 * time.Millisecond},
		{"max cap overflow exact", "9223372037", time.Unix(0, 900_000_000), math.MaxInt64,
			9223372036*time.Second + 100*time.Millisecond},
		{"max cap overflow capped", "9223372037", time.Unix(0, 100_000_000), math.MaxInt64, math.MaxInt64},
		{"max cap huge reset", pastInt64Max, time.Unix(0, 0), math.MaxInt64, math.MaxInt64},
	}
	for _, c := range cases {
		if got := ParseReset(c.v, c.now, c.maxDelay); got != c.want {
			t.Errorf("%s: ParseReset(%q) = %s, want %s", c.name, c.v, got, c.want)
		}
	}
}
