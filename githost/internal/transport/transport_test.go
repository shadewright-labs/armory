package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/githost"
)

// Canaries: synthetic values that no error text may contain.
const (
	tokenCanary = "CANARY-TOKEN-5b1f0c" //nolint:gosec // a synthetic canary, not a credential
	bodyCanary  = "CANARY-BODY-9d2e7a"
	pathCanary  = "CANARY-PATH-3c8f4d"
	checkCanary = "CANARY-CHECK-71ab0e"
)

const (
	opTest  = "TestOp"
	apiHost = "api.example.test"
	evilURL = "https://evil.example.test/x"
	hRetry  = "Retry-After"
	// at joins user information to a host in fixtures, so no fixture reads as an email address.
	at       = "@"
	template = "/repos/{owner}/{repo}/things/{id}"
	base     = 10 * time.Millisecond
)

var fixedNow = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

// server is a loopback TLS server that counts requests and records their Authorization headers.
type server struct {
	srv  *httptest.Server
	hits atomic.Int32
	mu   sync.Mutex
	auth []string
}

func newServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int)) *server {
	t.Helper()
	s := &server{}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(s.hits.Add(1))
		s.mu.Lock()
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.mu.Unlock()
		h(w, r, n)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *server) host() string { return s.srv.Listener.Addr().String() }

// harness is a client of one server with recorded waits, write checks and events.
type harness struct {
	c      *Client
	s      *server
	mu     sync.Mutex
	waits  []time.Duration
	checks []Send
	events []Event
	creds  atomic.Int32
}

func newHarness(t *testing.T, s *server, mod func(*Config)) *harness {
	t.Helper()
	h := &harness{s: s}
	cfg := Config{
		Host: s.host(),
		Repo: githost.RepoRef{Host: githost.GitHub, Owner: "o", Name: "n"},
		Credential: githost.CredentialFunc(func(context.Context, githost.Scope) (githost.Token, error) {
			h.creds.Add(1)
			return githost.NewToken(githost.Bearer, tokenCanary, time.Time{}), nil
		}),
		WriteCheck: func(_ context.Context, sd Send) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.checks = append(h.checks, sd)
			return nil
		},
		Observer: func(e Event) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.events = append(h.events, e)
		},
		BaseDelay:    base,
		RoundTripper: s.srv.Client().Transport,
		Jitter:       func(d time.Duration) time.Duration { return d },
		Now:          func() time.Time { return fixedNow },
		Sleep: func(ctx context.Context, d time.Duration) error {
			h.mu.Lock()
			h.waits = append(h.waits, d)
			h.mu.Unlock()
			return ctx.Err()
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.c = c
	return h
}

func get() Request {
	return Request{Op: opTest, Method: http.MethodGet, Path: "/repos/o/n/things/" + pathCanary, Template: template,
		Query: url.Values{"q": {pathCanary}}}
}

func post(idempotent bool) Request {
	r := get()
	r.Method = http.MethodPost
	r.Body = []byte(`{"x":"` + bodyCanary + `"}`)
	r.Idempotent = idempotent
	return r
}

// assertScrubbed fails when any error in the chain of err holds a canary, the server's address or a URL.
func assertScrubbed(t *testing.T, s *server, err error) {
	t.Helper()
	stack := []error{err}
	for len(stack) > 0 {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if e == nil {
			continue
		}
		text := e.Error()
		for _, bad := range []string{tokenCanary, bodyCanary, pathCanary, checkCanary, "https://", "127.0.0.1"} {
			if strings.Contains(text, bad) {
				t.Fatalf("error text %q holds %q", text, bad)
			}
		}
		if s != nil && strings.Contains(text, s.host()) {
			t.Fatalf("error text %q holds the host", text)
		}
		switch u := e.(type) { //nolint:errorlint // walks the chain one link at a time
		case interface{ Unwrap() error }:
			stack = append(stack, u.Unwrap())
		case interface{ Unwrap() []error }:
			stack = append(stack, u.Unwrap()...)
		}
	}
}

func okBody(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"ok":true}`) }

func TestNewRefusesConfig(t *testing.T) {
	cred := githost.CredentialFunc(func(context.Context, githost.Scope) (githost.Token, error) {
		return githost.Token{}, nil
	})
	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty host", Config{Credential: cred}},
		{"scheme", Config{Host: "https://api.example.test", Credential: cred}},
		{"path", Config{Host: "api.example.test/x", Credential: cred}},
		{"user", Config{Host: "u" + at + apiHost, Credential: cred}},
		{"query", Config{Host: "api.example.test?x=1", Credential: cred}},
		{"backslash", Config{Host: `api.example.test\x`, Credential: cred}},
		{"space", Config{Host: "api example.test", Credential: cred}},
		{"port only", Config{Host: ":443", Credential: cred}},
		{"no credential", Config{Host: apiHost}},
		{"authorization header", Config{Host: apiHost, Credential: cred,
			Header: http.Header{"authorization": {"x"}}}},
	}
	for _, c := range cases {
		if _, err := New(c.cfg); !errors.Is(err, githost.ErrInvalidConfig) {
			t.Errorf("%s: New = %v, want ErrInvalidConfig", c.name, err)
		}
	}
	for _, h := range []string{"api.example.test", "127.0.0.1:8443", "[::1]:8443"} {
		if _, err := New(Config{Host: h, Credential: cred}); err != nil {
			t.Errorf("New(%q) = %v", h, err)
		}
	}
}

// TestRequestOffOriginRefused: a request that would leave the pinned origin, or is not one the client
// sends, is refused before a token is asked for and before anything is sent.
func TestRequestOffOriginRefused(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
	h := newHarness(t, s, nil)
	cases := map[string]func(*Request){
		"userinfo host":  func(r *Request) { r.Path = "@evil.example.test/x" },
		"scheme-less":    func(r *Request) { r.Path = "//evil.example.test/x" },
		"relative":       func(r *Request) { r.Path = "x" },
		"query in path":  func(r *Request) { r.Path = "/x?y=1" },
		"fragment":       func(r *Request) { r.Path = "/x#y" },
		"backslash":      func(r *Request) { r.Path = `/\evil.example.test/x` },
		"method DELETE":  func(r *Request) { r.Method = http.MethodDelete },
		"method TRACE":   func(r *Request) { r.Method = http.MethodTrace },
		"no op label":    func(r *Request) { r.Op = "" },
		"no template":    func(r *Request) { r.Template = "" },
		"absolute https": func(r *Request) { r.Path = evilURL },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			r := get()
			mod(&r)
			_, err := h.c.Do(t.Context(), r)
			if !errors.Is(err, githost.ErrRejected) {
				t.Fatalf("Do = %v, want ErrRejected", err)
			}
		})
	}
	if n := s.hits.Load(); n != 0 {
		t.Errorf("server got %d requests", n)
	}
	if n := h.creds.Load(); n != 0 {
		t.Errorf("credential asked %d times", n)
	}
}

func TestOnOrigin(t *testing.T) {
	const host = apiHost
	cases := map[string]bool{
		"https://api.example.test/x":               true,
		"http://api.example.test/x":                false,
		evilURL:                                    false,
		"https://api.example.test:8443/x":          false,
		"https://u:p" + at + apiHost + "/x":        false,
		"https://api.example.test.evil.test/x":     false,
		"HTTPS://api.example.test/x":               true, // url.Parse lowers the scheme
		"ftp://api.example.test/x":                 false,
		"https:opaque":                             false,
		"https://API.EXAMPLE.TEST/x":               false,
		"https://" + apiHost + at + "evil.test/x/": false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := onOrigin(u, host); got != want {
			t.Errorf("onOrigin(%q) = %v, want %v", raw, got, want)
		}
	}
	if onOrigin(nil, host) {
		t.Error("onOrigin(nil) = true")
	}
}

// TestPinnedRoundTripper: below the client, a request off the origin, or not https, never reaches the
// network transport.
func TestPinnedRoundTripper(t *testing.T) {
	called := false
	p := pinned{host: apiHost, next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("sent")
	})}
	for _, raw := range []string{"http://api.example.test/x", evilURL} {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, raw, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.RoundTrip(r); !errors.Is(err, errOffOrigin) {
			t.Errorf("%s: RoundTrip = %v, want errOffOrigin", raw, err)
		}
	}
	if called {
		t.Error("the network transport was called")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestRedirectNotFollowed: a 3xx naming another host is an error; the other host gets nothing, and the
// token never leaves for it.
func TestRedirectNotFollowed(t *testing.T) {
	other := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				http.Redirect(w, r, other.srv.URL+"/stolen", code)
			})
			// httptest servers share one certificate, so the client trusts both and only the redirect rule
			// stops the hop.
			h := newHarness(t, s, nil)
			_, err := h.c.Do(t.Context(), get())
			var he *githost.Error
			if !errors.As(err, &he) || he.Status != code || errors.Is(err, githost.ErrRetryable) {
				t.Fatalf("Do = %v, want a status %d error", err, code)
			}
			assertScrubbed(t, s, err)
			if n := s.hits.Load(); n != 1 {
				t.Errorf("origin got %d requests, want 1", n)
			}
		})
	}
	if n := other.hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests", n)
	}
}

// TestNotModified: a conditional request gets its 304 back, not an error.
func TestNotModified(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		okBody(w)
	})
	h := newHarness(t, s, nil)
	r := get()
	r.IfNoneMatch = `"v1"`
	resp, err := h.c.Do(t.Context(), r)
	if err != nil || resp.Status != http.StatusNotModified || resp.Body != nil {
		t.Fatalf("Do = %+v, %v; want a 304", resp, err)
	}
}

func TestAuthorizationSet(t *testing.T) {
	for _, c := range []struct {
		scheme githost.Scheme
		want   string
	}{
		{githost.Bearer, "Bearer " + tokenCanary},
		{githost.Basic, "Basic " + basic(tokenCanary)},
	} {
		s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
		h := newHarness(t, s, func(cfg *Config) {
			cfg.Credential = githost.CredentialFunc(func(context.Context, githost.Scope) (githost.Token, error) {
				return githost.NewToken(c.scheme, tokenCanary, time.Time{}), nil
			})
		})
		if _, err := h.c.Do(t.Context(), get()); err != nil {
			t.Fatal(err)
		}
		if len(s.auth) != 1 || s.auth[0] != c.want {
			t.Errorf("scheme %d: Authorization %q, want %q", c.scheme, s.auth, c.want)
		}
	}
}

// TestBodyBounds: a 2xx body past the bound is ErrTooLarge, whether its length is declared or not, and
// is not retried.
func TestBodyBounds(t *testing.T) {
	const limit = 64
	cases := []struct {
		name    string
		write   func(w http.ResponseWriter)
		wantErr bool
	}{
		{"declared over", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", strconv.Itoa(limit+1))
			_, _ = io.WriteString(w, strings.Repeat("x", limit+1))
		}, true},
		{"chunked over", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, strings.Repeat("x", limit))
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "y")
		}, true},
		{"at the bound", func(w http.ResponseWriter) { _, _ = io.WriteString(w, strings.Repeat("x", limit)) }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { c.write(w) })
			h := newHarness(t, s, nil)
			r := get()
			r.Limit = limit
			resp, err := h.c.Do(t.Context(), r)
			if c.wantErr {
				if !errors.Is(err, githost.ErrTooLarge) || errors.Is(err, githost.ErrRetryable) || resp.Body != nil {
					t.Fatalf("Do = %v, want ErrTooLarge", err)
				}
				if n := s.hits.Load(); n != 1 {
					t.Errorf("server got %d requests, want 1", n)
				}
				return
			}
			if err != nil || len(resp.Body) != limit {
				t.Fatalf("Do = %d bytes, %v", len(resp.Body), err)
			}
		})
	}
}

// TestDefaultBodyLimit: without a bound on the request, the client's bound applies.
func TestDefaultBodyLimit(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = io.WriteString(w, strings.Repeat("x", 100))
	})
	h := newHarness(t, s, func(c *Config) { c.BodyLimit = 99 })
	if _, err := h.c.Do(t.Context(), get()); !errors.Is(err, githost.ErrTooLarge) {
		t.Fatalf("Do = %v, want ErrTooLarge", err)
	}
}

// TestSlowBody: a body that stalls is cut at the attempt timeout, retried, and reported as a timeout.
func TestSlowBody(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		_, _ = io.WriteString(w, `{"partial":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	h := newHarness(t, s, func(c *Config) {
		c.AttemptTimeout = 100 * time.Millisecond
		c.MaxAttempts = 2
	})
	start := time.Now()
	_, err := h.c.Do(t.Context(), get())
	if !errors.Is(err, githost.ErrRetryable) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Do = %v, want a retryable timeout", err)
	}
	assertScrubbed(t, s, err)
	if n := s.hits.Load(); n != 2 {
		t.Errorf("server got %d requests, want 2", n)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Do took %s", d)
	}
}

// TestRetryAfterAndRateLimits: a delay the host asks for is waited in place up to MaxWait, and a longer
// one is returned to the caller without a wait.
func TestRetryAfterAndRateLimits(t *testing.T) {
	reset := strconv.FormatInt(fixedNow.Add(7*time.Second).Unix(), 10)
	cases := []struct {
		name      string
		status    int
		header    map[string]string
		body      string
		wantWaits []time.Duration
		wantAfter time.Duration // the RetryAfter of the returned error; zero means success
		wantHits  int32
	}{
		{"429 seconds", 429, map[string]string{hRetry: "3"}, "", []time.Duration{3 * time.Second}, 0, 2},
		{"429 date", 429, map[string]string{hRetry: fixedNow.Add(5 * time.Second).Format(http.TimeFormat)}, "",
			[]time.Duration{5 * time.Second}, 0, 2},
		{"429 past date", 429, map[string]string{hRetry: fixedNow.Add(-time.Hour).Format(http.TimeFormat)}, "",
			nil, time.Minute, 1},
		{"429 too long", 429, map[string]string{hRetry: "60"}, "", nil, time.Minute, 1},
		{"429 capped at an hour", 429, map[string]string{hRetry: "999999"}, "", nil, time.Hour, 1},
		{"429 without headers", 429, nil, "", nil, time.Minute, 1},
		{"403 quota reset", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, "",
			[]time.Duration{7 * time.Second}, 0, 2},
		{"403 secondary", 403, nil, `{"message":"You have exceeded a secondary rate limit ` + bodyCanary + `"}`,
			nil, time.Minute, 1},
		{"503 retry-after", 503, map[string]string{hRetry: "2"}, bodyCanary, []time.Duration{2 * time.Second}, 0, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n == 1 {
					for k, v := range c.header {
						w.Header().Set(k, v)
					}
					w.WriteHeader(c.status)
					_, _ = io.WriteString(w, c.body)
					return
				}
				okBody(w)
			})
			h := newHarness(t, s, nil)
			_, err := h.c.Do(t.Context(), get())
			if c.wantAfter == 0 && err != nil {
				t.Fatalf("Do = %v, want success", err)
			}
			if c.wantAfter != 0 {
				var re *githost.RetryableError
				if !errors.As(err, &re) || re.RetryAfter != c.wantAfter {
					t.Fatalf("Do = %v, want retryable after %s", err, c.wantAfter)
				}
				assertScrubbed(t, s, err)
			}
			if !reflect.DeepEqual(h.waits, c.wantWaits) {
				t.Errorf("waits %v, want %v", h.waits, c.wantWaits)
			}
			if n := s.hits.Load(); n != c.wantHits {
				t.Errorf("server got %d requests, want %d", n, c.wantHits)
			}
		})
	}
}

// TestPlain403IsAnswer: a 403 that is not a rate limit comes back as an answer, for the host client to
// classify as ErrForbidden; its body is not kept.
func TestPlain403IsAnswer(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "Resource not accessible "+bodyCanary)
	})
	h := newHarness(t, s, nil)
	resp, err := h.c.Do(t.Context(), get())
	if err != nil || resp.Status != http.StatusForbidden || resp.Body != nil {
		t.Fatalf("Do = %+v, %v", resp, err)
	}
	if serr := StatusError(opTest, resp.Status); !errors.Is(serr, githost.ErrForbidden) {
		t.Errorf("StatusError = %v", serr)
	}
}

// TestTransientThenSuccess: 5xx and 408 are retried with exponential backoff until success or the
// attempt limit.
func TestTransientThenSuccess(t *testing.T) {
	cases := []struct {
		name      string
		fail      int // number of failed answers before success
		status    int
		wantErr   bool
		wantWaits []time.Duration
	}{
		{"502 then success", 1, http.StatusBadGateway, false, []time.Duration{base}},
		{"408 then success", 1, http.StatusRequestTimeout, false, []time.Duration{base}},
		{"500 twice then success", 2, http.StatusInternalServerError, false, []time.Duration{base, 2 * base}},
		{"503 every time", 99, http.StatusServiceUnavailable, true, []time.Duration{base, 2 * base, 4 * base}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n <= c.fail {
					w.WriteHeader(c.status)
					_, _ = io.WriteString(w, bodyCanary)
					return
				}
				okBody(w)
			})
			h := newHarness(t, s, nil)
			resp, err := h.c.Do(t.Context(), get())
			if c.wantErr {
				var he *githost.Error
				if !errors.Is(err, githost.ErrRetryable) || !errors.As(err, &he) || he.Status != c.status {
					t.Fatalf("Do = %v, want a retryable status %d", err, c.status)
				}
				assertScrubbed(t, s, err)
			} else if err != nil || string(resp.Body) != `{"ok":true}` {
				t.Fatalf("Do = %q, %v", resp.Body, err)
			}
			if !reflect.DeepEqual(h.waits, c.wantWaits) {
				t.Errorf("waits %v, want %v", h.waits, c.wantWaits)
			}
		})
	}
}

// TestJitterApplied: the backoff delay goes through the jitter source.
func TestJitterApplied(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		okBody(w)
	})
	h := newHarness(t, s, func(c *Config) { c.Jitter = func(d time.Duration) time.Duration { return d / 2 } })
	if _, err := h.c.Do(t.Context(), get()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.waits, []time.Duration{base / 2}) {
		t.Errorf("waits %v, want [%s]", h.waits, base/2)
	}
}

// TestCancelDuringBackoff: a context cancelled while the client waits ends the wait at once.
func TestCancelDuringBackoff(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h := newHarness(t, s, func(c *Config) {
		c.BaseDelay = 10 * time.Second
		c.Sleep = nil // the real timer
		c.Now = nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := h.c.Do(ctx, get())
	if !errors.Is(err, context.Canceled) || errors.Is(err, githost.ErrRetryable) {
		t.Fatalf("Do = %v, want the cancellation", err)
	}
	assertScrubbed(t, s, err)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Do took %s after the cancel", d)
	}
	if n := s.hits.Load(); n != 1 {
		t.Errorf("server got %d requests, want 1", n)
	}
}

// TestNoWaitPastDeadline: a backoff that would end after the context's deadline is not started.
func TestNoWaitPastDeadline(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h := newHarness(t, s, func(c *Config) { c.Now = nil; c.BaseDelay = time.Minute; c.MaxWait = time.Hour })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := h.c.Do(ctx, get())
	if !errors.Is(err, githost.ErrRetryable) || len(h.waits) != 0 {
		t.Fatalf("Do = %v with waits %v; want the retryable error at once", err, h.waits)
	}
}

// errCheck stands for a product's refusal; neither its text nor its identity may reach the caller.
var errCheck = errors.New(checkCanary + " writes are paused")

// TestWriteCheckBeforeEachAttempt: an idempotent write asks the check before every attempt, and a
// refusal of a retry stops the call with nothing more sent.
func TestWriteCheckBeforeEachAttempt(t *testing.T) {
	cases := []struct {
		name       string
		refuseFrom int // first attempt the check refuses; 0 means never
		fail       int // failed answers before success
		wantHits   int32
		wantChecks []int
		wantErr    error
	}{
		{"allowed retries", 0, 2, 3, []int{1, 2, 3}, nil},
		{"refuses the retry", 2, 1, 1, []int{1, 2}, githost.ErrStoppedBeforeWrite},
		{"refuses the first send", 1, 0, 0, []int{1}, githost.ErrStoppedBeforeWrite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n <= c.fail {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				okBody(w)
			})
			var attempts []int
			h := newHarness(t, s, func(cfg *Config) {
				cfg.WriteCheck = func(_ context.Context, sd Send) error {
					attempts = append(attempts, sd.Attempt)
					if sd.Op != opTest || sd.Method != http.MethodPost || sd.Template != template {
						t.Errorf("check got %+v", sd)
					}
					if c.refuseFrom > 0 && sd.Attempt >= c.refuseFrom {
						return errCheck
					}
					return nil
				}
			})
			_, err := h.c.Do(t.Context(), post(true))
			if !errors.Is(err, c.wantErr) && (c.wantErr != nil || err != nil) {
				t.Fatalf("Do = %v, want %v", err, c.wantErr)
			}
			if c.wantErr != nil {
				if errors.Is(err, errCheck) || errors.Is(err, githost.ErrRetryable) {
					t.Errorf("Do = %v keeps the check's error or is retryable", err)
				}
				assertScrubbed(t, s, err)
			}
			if n := s.hits.Load(); n != c.wantHits {
				t.Errorf("server got %d requests, want %d", n, c.wantHits)
			}
			if !reflect.DeepEqual(attempts, c.wantChecks) {
				t.Errorf("check asked for attempts %v, want %v", attempts, c.wantChecks)
			}
		})
	}
}

// TestReadsNotChecked: reads go out while the check refuses, and do not ask it.
func TestReadsNotChecked(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
	asked := 0
	h := newHarness(t, s, func(c *Config) {
		c.WriteCheck = func(context.Context, Send) error { asked++; return errCheck }
	})
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		r := get()
		r.Method = m
		if _, err := h.c.Do(t.Context(), r); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	if asked != 0 {
		t.Errorf("reads asked the check %d times", asked)
	}
}

func TestNilWriteCheckAllowsNoWrite(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
	h := newHarness(t, s, func(c *Config) { c.WriteCheck = nil })
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		r := post(true)
		r.Method = m
		if _, err := h.c.Do(t.Context(), r); !errors.Is(err, githost.ErrStoppedBeforeWrite) {
			t.Errorf("%s: Do = %v, want ErrStoppedBeforeWrite", m, err)
		}
	}
	if n := s.hits.Load(); n != 0 {
		t.Errorf("server got %d requests", n)
	}
}

// TestWriteSentOnce: a write not marked idempotent is never sent twice in a call; a lost answer is
// ErrOutcomeUnknown and a rate limit is returned to the caller.
func TestWriteSentOnce(t *testing.T) {
	cases := []struct {
		name    string
		handle  func(w http.ResponseWriter)
		wantErr error
	}{
		{"502", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }, githost.ErrOutcomeUnknown},
		{"408", func(w http.ResponseWriter) { w.WriteHeader(http.StatusRequestTimeout) }, githost.ErrOutcomeUnknown},
		{"connection dropped", func(w http.ResponseWriter) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, githost.ErrOutcomeUnknown},
		{"429", func(w http.ResponseWriter) {
			w.Header().Set(hRetry, "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}, githost.ErrRetryable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { c.handle(w) })
			h := newHarness(t, s, nil)
			_, err := h.c.Do(t.Context(), post(false))
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("Do = %v, want %v", err, c.wantErr)
			}
			assertScrubbed(t, s, err)
			if n := s.hits.Load(); n != 1 {
				t.Errorf("server got %d requests, want 1", n)
			}
			if len(h.waits) != 0 {
				t.Errorf("waited %v", h.waits)
			}
		})
	}
}

// TestNetworkErrorRetried: a read whose connection drops is retried and ends as a retryable failure
// with fixed text.
func TestNetworkErrorRetried(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	h := newHarness(t, s, nil)
	_, err := h.c.Do(t.Context(), get())
	if !errors.Is(err, githost.ErrRetryable) {
		t.Fatalf("Do = %v, want retryable", err)
	}
	assertScrubbed(t, s, err)
	if n := s.hits.Load(); n != DefaultMaxAttempts {
		t.Errorf("server got %d requests, want %d", n, DefaultMaxAttempts)
	}
}

// TestUnreachableHostScrubbed: the dial error of net/http names the URL; the client's error does not.
func TestUnreachableHostScrubbed(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
	h := newHarness(t, s, func(c *Config) { c.MaxAttempts = 1 })
	s.srv.Close()
	_, err := h.c.Do(t.Context(), get())
	if !errors.Is(err, githost.ErrRetryable) {
		t.Fatalf("Do = %v, want retryable", err)
	}
	assertScrubbed(t, s, err)
}

// endAfter passes requests on and ends a context before the answer is returned, or on the first read of
// its body: an answer that raced the end of the call.
type endAfter struct {
	next   http.RoundTripper
	end    context.CancelFunc
	inBody bool
}

func (e endAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := e.next.RoundTrip(r)
	if err != nil {
		return resp, err
	}
	if !e.inBody {
		e.end()
		return resp, nil
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(endingReader{e.end}, resp.Body), resp.Body}
	return resp, nil
}

type endingReader struct{ end context.CancelFunc }

func (e endingReader) Read([]byte) (int, error) {
	e.end()
	return 0, io.EOF
}

// TestAnswerAfterContextEnd: an answer, or a body, taken after the call's context ended is dropped and
// the end of the context is reported.
func TestAnswerAfterContextEnd(t *testing.T) {
	for _, inBody := range []bool{false, true} {
		for _, status := range []int{http.StatusOK, http.StatusNotFound} {
			t.Run(strconv.Itoa(status)+map[bool]string{false: " answer", true: " body"}[inBody], func(t *testing.T) {
				s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
					w.WriteHeader(status)
					okBody(w)
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				h := newHarness(t, s, func(c *Config) {
					c.RoundTripper = endAfter{next: s.srv.Client().Transport, end: cancel, inBody: inBody}
				})
				resp, err := h.c.Do(ctx, get())
				if inBody && status != http.StatusOK {
					return // a non-2xx body is never read
				}
				if !errors.Is(err, context.Canceled) || resp.Body != nil || resp.Status != 0 {
					t.Fatalf("Do = %+v, %v; want the cancellation", resp, err)
				}
			})
		}
	}
}

// TestCredentialErrorsKeepClassOnly: a credential's error text never reaches the caller; its class does.
func TestCredentialErrorsKeepClassOnly(t *testing.T) {
	leak := errors.New(tokenCanary + " https://login.example.test/" + pathCanary)
	cases := []struct {
		name      string
		err       error
		tok       githost.Token
		want      []error
		retryable bool
	}{
		{"plain", leak, githost.Token{}, nil, false},
		{"canceled", errors.Join(leak, context.Canceled), githost.Token{}, []error{context.Canceled}, false},
		{"deadline", errors.Join(leak, context.DeadlineExceeded), githost.Token{},
			[]error{context.DeadlineExceeded}, true},
		{"retryable", &githost.RetryableError{RetryAfter: time.Hour, Err: leak}, githost.Token{}, nil, true},
		{"not found", errors.Join(leak, githost.ErrNotFound), githost.Token{}, []error{githost.ErrNotFound}, false},
		{"forbidden", errors.Join(leak, githost.ErrForbidden), githost.Token{}, []error{githost.ErrForbidden}, false},
		{"unauthorized", errors.Join(leak, githost.ErrUnauthorized), githost.Token{},
			[]error{githost.ErrUnauthorized}, false},
		{"zero token", nil, githost.Token{}, nil, false},
		{"unknown scheme", nil, githost.NewToken(githost.Scheme(7), tokenCanary, time.Time{}), nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
			h := newHarness(t, s, func(cfg *Config) {
				cfg.MaxAttempts = 1
				cfg.Credential = githost.CredentialFunc(func(context.Context, githost.Scope) (githost.Token, error) {
					return c.tok, c.err
				})
			})
			_, err := h.c.Do(t.Context(), get())
			if err == nil {
				t.Fatal("Do succeeded")
			}
			for _, k := range c.want {
				if !errors.Is(err, k) {
					t.Errorf("Do = %v, want %v", err, k)
				}
			}
			if got := errors.Is(err, githost.ErrRetryable); got != c.retryable {
				t.Errorf("retryable = %v, want %v", got, c.retryable)
			}
			if errors.Is(err, leak) {
				t.Error("the credential's error is in the chain")
			}
			assertScrubbed(t, s, err)
			if n := s.hits.Load(); n != 0 {
				t.Errorf("server got %d requests", n)
			}
		})
	}
}

// TestScopeAsked: reads ask for a read token, writes for a write token, for the bound repository.
func TestScopeAsked(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
	var scopes []githost.Scope
	h := newHarness(t, s, func(c *Config) {
		c.Credential = githost.CredentialFunc(func(_ context.Context, sc githost.Scope) (githost.Token, error) {
			scopes = append(scopes, sc)
			return githost.NewToken(githost.Bearer, tokenCanary, time.Time{}), nil
		})
	})
	for _, r := range []Request{get(), post(true)} {
		if _, err := h.c.Do(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	repo := githost.RepoRef{Host: githost.GitHub, Owner: "o", Name: "n"}
	want := []githost.Scope{{Repo: repo}, {Repo: repo, Write: true}}
	if !reflect.DeepEqual(scopes, want) {
		t.Errorf("scopes %+v, want %+v", scopes, want)
	}
}

// TestObserver: one event per attempt, with the path template and no other request data.
func TestObserver(t *testing.T) {
	if n := reflect.TypeFor[Event]().NumField(); n != 6 {
		t.Fatalf("Event has %d fields, want 6: method, host, template, status, attempt, duration", n)
	}
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		okBody(w)
	})
	h := newHarness(t, s, nil)
	if _, err := h.c.Do(t.Context(), get()); err != nil {
		t.Fatal(err)
	}
	if len(h.events) != 2 {
		t.Fatalf("events %+v, want 2", h.events)
	}
	for i, e := range h.events {
		want := Event{Method: http.MethodGet, Host: s.host(), Template: template,
			Status: []int{http.StatusBadGateway, http.StatusOK}[i], Attempt: i + 1, Duration: e.Duration}
		if e != want || e.Duration < 0 {
			t.Errorf("event %d = %+v, want %+v", i, e, want)
		}
	}
}

// TestRequestsCannotBeRewound: no request with a body carries GetBody, so Go's transport does not send
// it again below Do once any of it was written.
func TestRequestsCannotBeRewound(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
	var seen []bool
	h := newHarness(t, s, func(c *Config) {
		next := s.srv.Client().Transport
		c.RoundTripper = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			seen = append(seen, r.Body != nil && r.GetBody == nil)
			return next.RoundTrip(r)
		})
	})
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		r := post(true)
		r.Method = m
		if _, err := h.c.Do(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(seen, []bool{true, true, true}) {
		t.Errorf("body without GetBody: %v", seen)
	}
}

func TestRequestHeaders(t *testing.T) {
	var got http.Header
	s := newServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		got = r.Header.Clone()
		okBody(w)
	})
	h := newHarness(t, s, func(c *Config) { c.Header = http.Header{"X-Api-Version": {"2022-11-28"}} })
	r := post(true)
	r.Accept = "application/vnd.github+json"
	if _, err := h.c.Do(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"X-Api-Version": "2022-11-28", "Accept": "application/vnd.github+json",
		"Content-Type": "application/json", "User-Agent": "armory-githost"} {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
}

func TestStatusError(t *testing.T) {
	cases := map[int]error{
		404: githost.ErrNotFound, 410: githost.ErrNotFound, 409: githost.ErrConflict, 403: githost.ErrForbidden,
		401: githost.ErrUnauthorized, 400: githost.ErrRejected, 422: githost.ErrRejected, 418: nil,
	}
	for code, kind := range cases {
		err := StatusError(opTest, code)
		var he *githost.Error
		if !errors.As(err, &he) || he.Status != code || !errors.Is(he.Kind, kind) || (kind == nil && he.Kind != nil) {
			t.Errorf("StatusError(%d) = %v, want kind %v", code, err, kind)
		}
	}
}

// failingReader returns some bytes, then an error, like a connection reset.
type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func TestRateLimited(t *testing.T) {
	resp := func(status int, body io.Reader, kv ...string) *http.Response {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(body)}
	}
	reset := func(d time.Duration) string { return strconv.FormatInt(fixedNow.Add(d).Unix(), 10) }
	cases := []struct {
		name    string
		r       *http.Response
		limited bool
		want    time.Duration
	}{
		{"retry-after", resp(403, strings.NewReader(""), hRetry, "12"), true, 12 * time.Second},
		{"quota reset", resp(403, strings.NewReader(""), "X-RateLimit-Remaining", "0",
			"X-RateLimit-Reset", reset(45*time.Second)), true, 45 * time.Second},
		{"quota reset in the past", resp(403, strings.NewReader(""), "X-RateLimit-Remaining", "0",
			"X-RateLimit-Reset", reset(-time.Minute)), true, time.Minute},
		{"headerless 429", resp(429, strings.NewReader("")), true, time.Minute},
		{"secondary 403", resp(403, strings.NewReader("You have exceeded a secondary rate limit"),
			"X-RateLimit-Remaining", "10"), true, time.Minute},
		{"abuse 403", resp(403, strings.NewReader("Abuse Detection mechanism")), true, time.Minute},
		{"marker past the probe", resp(403, strings.NewReader(strings.Repeat(" ", maxProbeBody)+"rate limit")),
			false, 0},
		{"plain 403", resp(403, strings.NewReader("Resource not accessible")), false, 0},
		{"unreadable 403", resp(403, &failingReader{data: []byte(`{"message":"You have exc`),
			err: io.ErrUnexpectedEOF}), true, time.Minute},
	}
	for _, c := range cases {
		d, ok := rateLimited(c.r, fixedNow)
		if ok != c.limited || d != c.want {
			t.Errorf("%s: rateLimited = %v, %v; want %v, %v", c.name, d, ok, c.want, c.limited)
		}
	}
}

func TestNextPage(t *testing.T) {
	c, err := New(Config{Host: apiHost, Credential: githost.CredentialFunc(
		func(context.Context, githost.Scope) (githost.Token, error) { return githost.Token{}, nil })})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		link   []string
		want   string
		ok     bool
		bad    bool
		param  string
		header string
	}{
		{name: "none"},
		{name: "last page", link: []string{`<https://api.example.test/x?page=1>; rel="prev"`}},
		{name: "next", link: []string{`<https://api.example.test/x?page=3&per_page=100>; rel="next", ` +
			`<https://api.example.test/x?page=9>; rel="last"`}, want: "3", ok: true},
		{name: "cursor", link: []string{`<https://api.example.test/x?after=Y3Vyc29yOjE%3D>; rel="next"`},
			param: "after", want: "Y3Vyc29yOjE=", ok: true},
		{name: "two rels", link: []string{`<https://api.example.test/x?page=2>; rel="first next"`}, want: "2", ok: true},
		{name: "second header", link: []string{`<https://api.example.test/x?page=1>; rel="prev"`,
			`<https://api.example.test/x?page=4>; rel=next`}, want: "4", ok: true},
		{name: "other host", link: []string{`<https://evil.example.test/x?page=3>; rel="next"`}, bad: true},
		{name: "plain http", link: []string{`<http://api.example.test/x?page=3>; rel="next"`}, bad: true},
		{name: "userinfo", link: []string{"<https://" + apiHost + at + `evil.test/x?page=3>; rel="next"`}, bad: true},
		{name: "no brackets", link: []string{`https://api.example.test/x?page=3; rel="next"`}, bad: true},
		{name: "missing cursor", link: []string{`<https://api.example.test/x?per_page=3>; rel="next"`}, bad: true},
		{name: "cursor with slash", link: []string{`<https://api.example.test/x?page=..%2Fx>; rel="next"`}, bad: true},
		{name: "cursor too long", link: []string{`<https://api.example.test/x?page=` + strings.Repeat("1", maxCursor+1) +
			`>; rel="next"`}, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for _, l := range tc.link {
				h.Add("Link", l)
			}
			param := tc.param
			if param == "" {
				param = "page"
			}
			got, ok, err := c.NextPage(opTest, h, param)
			if tc.bad {
				if !errors.Is(err, githost.ErrUntrustedAnswer) || ok || got != "" {
					t.Fatalf("NextPage = %q, %v, %v; want ErrUntrustedAnswer", got, ok, err)
				}
				return
			}
			if err != nil || ok != tc.ok || got != tc.want {
				t.Fatalf("NextPage = %q, %v, %v; want %q, %v", got, ok, err, tc.want, tc.ok)
			}
		})
	}
}
