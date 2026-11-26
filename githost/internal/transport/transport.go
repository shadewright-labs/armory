// Package transport is the HTTP client every githost host client sends through. It is internal: the
// host packages pin the origin, and no caller of the module can pass a URL.
//
// A Client is built for one https origin (a host, with a port only in tests) and sends every request
// through one private path, [Client.Do]. That path:
//
//   - builds the URL from the pinned origin and an escaped path, and refuses any URL whose scheme is not
//     https or whose host is not the pinned one, or whose path the request's [Template] does not name,
//     before a credential is asked for or attached;
//   - asks the Credential for a token before each attempt, with the caller's context, and installs it
//     in the Authorization header only there, after the origin check;
//   - sends the request with a context that ends with the caller's and has its deadline but returns no
//     value, so no client trace (net/http/httptrace) or dial trace of the caller sees the request;
//   - never follows a redirect: a 3xx other than 304 is an error, and 304 is returned to the caller for
//     a conditional request;
//   - reads every 2xx body with a bound the caller sets, refuses a declared length over it before
//     reading, and returns githost.ErrTooLarge past it; a 2xx answer to a write whose body cannot be
//     used is githost.ErrUnusableReceipt and is never sent again;
//   - retries reads, and writes marked idempotent, on network errors, timeouts, 408, 429, 5xx and rate
//     limits, with exponential backoff and jitter, Retry-After and rate limit reset delays, and the
//     context honoured during every wait; a write not marked idempotent is sent once;
//   - once an attempt of a write may have reached the host without an answer that says it did not
//     write, returns every later failure of the call as githost.ErrOutcomeUnknown next to its own kind;
//   - calls the write check before every attempt of a write, and sends nothing more when it refuses;
//   - builds error text from fixed words, the operation label, the status code and a valid refusal class
//     only, never from a body, URL, header, token or a wrapped error of another package or a callback;
//   - reports one payload-free Event per attempt to the Observer, after the attempt is over.
//
// An attempt is one call of http.Client.Do. Below it, Go's HTTP/1 transport may send a request again on
// a new connection when a reused one fails; for a request with a body it does so only when the body can
// be rewound, so Do clears GetBody on every request with a body, and Go then sends it again only when
// nothing of it was written. Each write check is therefore followed by at most one write that reaches
// the host.
//
// What sees the request after the token is set: the http.Client this package builds (its redirect
// policy only returns the last answer and never sees a token-bearing request of its own), and
// Config.RoundTripper when one is set, which is a trusted component and sees the Authorization header.
// The Credential, the write check, the Observer, Jitter, Now and Sleep never get the request. Errors of
// net/http (a *url.Error holds the method and URL, not the headers) are replaced, never wrapped. No code
// of this package formats a request, so a panic, in a callback or below, prints no header value.
package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shadewright-labs/armory/githost"
	"github.com/shadewright-labs/armory/githost/internal/secret"
	"github.com/shadewright-labs/armory/githost/retryafter"
)

const opNew = "transport.New"

// Defaults for [Config].
const (
	DefaultMaxAttempts    = 4
	DefaultBaseDelay      = 500 * time.Millisecond
	DefaultMaxWait        = 15 * time.Second
	DefaultTimeout        = 2 * time.Minute
	DefaultAttemptTimeout = 60 * time.Second
	// DefaultBodyLimit bounds a JSON answer when the request sets no lower bound.
	DefaultBodyLimit = 2 << 20
)

const (
	// maxDelay caps any delay a host asks for.
	maxDelay = time.Hour
	// secondaryWait is the delay of a rate limit that comes without a usable one.
	secondaryWait = time.Minute
	// maxProbeBody bounds the part of a 403 body searched for a rate limit marker.
	maxProbeBody = 16 << 10
	// maxHeaderBytes bounds the response headers.
	maxHeaderBytes = 256 << 10
)

// WriteCheck is called right before every attempt of a write, after the token is set and the body is
// built, with the caller's context. A non-nil error stops the call: nothing more is sent, and Do returns
// an error matching githost.ErrStoppedBeforeWrite. Of the check's error only the class of a valid
// *githost.Refusal in its chain is kept, as a new *githost.Refusal; its text and identity are not.
//
// Provisional: this hook stands in for the write gate of the design record, whose shape is being
// reworked. It describes the request, not the product operation, and will change.
type WriteCheck func(ctx context.Context, s Send) error

// Send describes one attempt of a write for a [WriteCheck].
type Send struct {
	Op       string   // the request's fixed operation label
	Method   string   // the HTTP method
	Template Template // the request's path template, never the raw path
	Attempt  int      // 1 for the first send, then 2, 3, ... for each retry
}

// Event is what the Observer gets for each attempt that was sent: no error, body, header, query, raw
// path or token.
type Event struct {
	Method   string   // the HTTP method
	Host     string   // the pinned host
	Template Template // the request's path template, never the raw path
	Status   int      // zero when no answer came
	Attempt  int      // 1 for the first send, then 2, 3, ... for each retry
	// Duration runs from the hand-off to the HTTP client until the answer's body was read to its end
	// (or to the bound) and closed, or until the attempt failed.
	Duration time.Duration
}

// Observer receives one Event per attempt. It is called synchronously once the attempt is over: the
// answer is read and closed, nothing is in flight, and the attempt timeout no longer runs, so a slow
// Observer cannot cut an answer short. Do neither returns nor starts another attempt until it returns,
// and checks the caller's context before the next attempt, so a slow Observer starts nothing past the
// caller's deadline; it must still not block, because Do returns only after it does.
type Observer func(Event)

// Config builds a [Client].
type Config struct {
	// Host is the one host the client reaches over https, such as "api.github.com". A port is allowed for
	// tests on loopback; a scheme, path, user or query is not.
	Host string
	// Repo is the repository tokens are asked for.
	Repo githost.RepoRef
	// Credential returns a token for each attempt. Required.
	Credential githost.Credential
	// WriteCheck is called before each attempt of a write. Nil allows no write.
	WriteCheck WriteCheck
	// Observer, when set, gets one Event per attempt.
	Observer Observer
	// Header holds fixed headers sent with every request, such as an API version. Authorization is
	// refused.
	Header http.Header
	// UserAgent is sent with every request; empty means "armory-githost".
	UserAgent string

	MaxAttempts    int           // attempts of one request; 0 means DefaultMaxAttempts
	BaseDelay      time.Duration // first backoff delay; 0 means DefaultBaseDelay
	MaxWait        time.Duration // longest delay waited in place; 0 means DefaultMaxWait
	Timeout        time.Duration // one call of Do, retries included; 0 means DefaultTimeout
	AttemptTimeout time.Duration // one attempt, body included; 0 means DefaultAttemptTimeout
	BodyLimit      int64         // default bound of a 2xx body; 0 means DefaultBodyLimit

	// RoundTripper replaces the HTTP transport; tests use it to trust a loopback server's certificate. It
	// is a trusted component: it gets every request with its Authorization header. Only code of this
	// module sets it; no exported API passes one through.
	RoundTripper http.RoundTripper
	// Jitter maps a backoff delay to the one waited; nil means a random value in [d/2, d].
	Jitter func(time.Duration) time.Duration
	// Now is the clock Retry-After dates and rate limit resets are measured from; nil means time.Now.
	Now func() time.Time
	// Sleep waits d or until ctx ends; nil means a timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Client sends requests to one https origin. It is safe for concurrent use.
type Client struct {
	host       string
	repo       githost.RepoRef
	cred       githost.Credential
	check      WriteCheck
	observe    Observer
	header     http.Header
	userAgent  string
	attempts   int
	base       time.Duration
	maxWait    time.Duration
	timeout    time.Duration
	bodyLimit  int64
	httpClient *http.Client
	jitter     func(time.Duration) time.Duration
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
}

// Request is one HTTP request to the pinned origin.
type Request struct {
	// Op is a fixed label for errors, such as "BranchHead". It never holds caller or host text.
	Op     string
	Method string // GET, HEAD, POST, PUT or PATCH
	// Path is the escaped path, starting with "/", built from escaped segments only. It holds no query.
	Path string
	// Template names Path, such as "/repos/{owner}/{repo}/pulls/{n}", for the Observer and the write
	// check. Host packages declare their templates as constants; see [Template].
	Template    Template
	Query       url.Values
	Body        []byte
	ContentType string // sent when Body is set; empty means application/json
	Accept      string
	IfNoneMatch string // sent as If-None-Match, so an unchanged answer is a 304
	Limit       int64  // bound of the 2xx body; 0 means the client's default
	// Idempotent marks a write that may be sent again in the call, such as a content-addressed object or a
	// ref move with an expected old value. Reads are always retried; a write without it is sent once.
	Idempotent bool
}

// Response is an answer whose status is not a transient failure. Body is read for 2xx only; any other
// status comes back without a body, for the host client to classify with [StatusError].
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// New validates cfg and returns a Client. It makes no network call.
func New(cfg Config) (*Client, error) {
	if !validHost(cfg.Host) {
		return nil, &githost.Error{Op: opNew, Kind: githost.ErrInvalidConfig}
	}
	if cfg.Credential == nil {
		return nil, &githost.Error{Op: opNew, Kind: githost.ErrInvalidConfig}
	}
	for k := range cfg.Header {
		if http.CanonicalHeaderKey(k) == "Authorization" {
			return nil, &githost.Error{Op: opNew, Kind: githost.ErrInvalidConfig}
		}
	}
	c := &Client{
		host:      cfg.Host,
		repo:      cfg.Repo,
		cred:      cfg.Credential,
		check:     cfg.WriteCheck,
		observe:   cfg.Observer,
		header:    cfg.Header.Clone(),
		userAgent: cfg.UserAgent,
		attempts:  orDefault(cfg.MaxAttempts, DefaultMaxAttempts),
		base:      orDefault(cfg.BaseDelay, DefaultBaseDelay),
		maxWait:   orDefault(cfg.MaxWait, DefaultMaxWait),
		timeout:   orDefault(cfg.Timeout, DefaultTimeout),
		bodyLimit: orDefault(cfg.BodyLimit, DefaultBodyLimit),
		jitter:    cfg.Jitter,
		now:       cfg.Now,
		sleep:     cfg.Sleep,
	}
	if c.userAgent == "" {
		c.userAgent = "armory-githost"
	}
	if c.jitter == nil {
		// #nosec G404 -- retry jitter, not a secret
		c.jitter = func(d time.Duration) time.Duration { return d/2 + rand.N(d/2+1) }
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = sleep
	}
	c.httpClient = newHTTPClient(cfg.RoundTripper, cfg.Host, orDefault(cfg.AttemptTimeout, DefaultAttemptTimeout))
	return c, nil
}

func orDefault[T int | int64 | time.Duration](v, d T) T {
	if v <= 0 {
		return d
	}
	return v
}

// validHost accepts a host name or IP with an optional port, and nothing else.
func validHost(h string) bool {
	if h == "" || strings.ContainsAny(h, "/?#@\\ \t\r\n") {
		return false
	}
	u, err := url.Parse("https://" + h)
	return err == nil && u.Host == h && u.Hostname() != "" && u.User == nil && u.Path == "" && u.RawQuery == ""
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newHTTPClient never follows a redirect (Go keeps the Authorization header on some), requires TLS 1.2
// or later, bounds connection setup, the wait for headers, the header size and each attempt, and checks
// the origin again below the client, in case anything below Do builds a request of its own.
func newHTTPClient(rt http.RoundTripper, host string, attempt time.Duration) *http.Client {
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		t.TLSHandshakeTimeout = 10 * time.Second
		t.ResponseHeaderTimeout = 30 * time.Second
		t.MaxResponseHeaderBytes = maxHeaderBytes
		t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		rt = t
	}
	return &http.Client{
		Transport: pinned{host: host, next: rt},
		Timeout:   attempt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// errOffOrigin is what pinned returns for a request to another origin. It never reaches a caller as is.
var errOffOrigin = errors.New("request off the pinned origin")

// pinned refuses any request whose URL is not https on the pinned host.
type pinned struct {
	host string
	next http.RoundTripper
}

func (p pinned) RoundTrip(r *http.Request) (*http.Response, error) {
	if !onOrigin(r.URL, p.host) {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, errOffOrigin
	}
	return p.next.RoundTrip(r)
}

// onOrigin reports whether u is an https URL on host with no user information.
func onOrigin(u *url.URL, host string) bool {
	return u != nil && u.Scheme == "https" && u.Host == host && u.User == nil && u.Opaque == ""
}

// Do sends r, retrying transient failures. See the package documentation for the rules.
func (c *Client) Do(ctx context.Context, r Request) (Response, error) {
	u, err := c.buildURL(r)
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	write := isWrite(r.Method)
	// mayHaveWritten is set once an attempt of the write may have reached the host without an answer that
	// says it did not write. From then on no failure of the call may read as "nothing was written".
	mayHaveWritten := false
	for attempt := 1; ; attempt++ {
		resp, sent, err := c.once(ctx, r, u, attempt)
		mayHaveWritten = mayHaveWritten || (write && sent)
		switch {
		case err == nil && (!mayHaveWritten || success(resp.Status)):
			return resp, nil
		case err == nil:
			// An answer such as 409 or 422 after an attempt whose answer was lost may be about that attempt.
			return Response{}, outcomeUnknown(r.Op, StatusError(r.Op, resp.Status))
		case errors.Is(err, githost.ErrUnusableReceipt):
			return Response{}, err
		case !errors.Is(err, githost.ErrRetryable) || attempt >= c.attempts || (write && !r.Idempotent):
			return Response{}, finalError(r.Op, err, mayHaveWritten)
		}
		if werr := c.backoff(ctx, r.Op, attempt, err); werr != nil {
			return Response{}, finalError(r.Op, werr, mayHaveWritten)
		}
	}
}

func isWrite(method string) bool { return method != http.MethodGet && method != http.MethodHead }

func success(status int) bool { return status >= 200 && status <= 299 }

// buildURL makes the URL of r on the pinned origin and refuses anything else: a method outside the
// set, a path that does not start with one slash, holds a query, a fragment or a backslash, would name
// another host, or that the template does not name.
func (c *Client) buildURL(r Request) (*url.URL, error) {
	rejected := &githost.Error{Op: r.Op, Kind: githost.ErrRejected}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return nil, rejected
	}
	if r.Op == "" || !strings.HasPrefix(r.Path, "/") || strings.HasPrefix(r.Path, "//") ||
		strings.ContainsAny(r.Path, "?#\\") || !r.Template.names(r.Path) {
		return nil, rejected
	}
	u, err := url.Parse("https://" + c.host + r.Path)
	if err != nil || !onOrigin(u, c.host) || u.RawQuery != "" || u.Fragment != "" {
		return nil, rejected
	}
	if len(r.Query) > 0 {
		u.RawQuery = r.Query.Encode()
	}
	return u, nil
}

// once is one attempt: token, request, origin check, Authorization, write check, send, classification.
// sent reports whether the attempt may have reached the host without an answer that says it was not
// carried out.
func (c *Client) once(ctx context.Context, r Request, u *url.URL, attempt int) (Response, bool, error) {
	if ctx.Err() != nil {
		return Response{}, false, ctxError(r.Op, ctx)
	}
	write := isWrite(r.Method)
	// The credential is the product's: it gets the caller's context, values included.
	tok, err := c.cred.Token(ctx, githost.Scope{Repo: c.repo, Write: write})
	if err != nil {
		return Response{}, false, credentialError(r.Op, err)
	}
	var body io.Reader
	if r.Body != nil {
		body = bytes.NewReader(r.Body)
	}
	// The wire request gets a context without the caller's values, so no client trace of the caller sees
	// it, its Authorization header included.
	req, err := http.NewRequestWithContext(wireContext{ctx}, r.Method, u.String(), body)
	if err != nil {
		return Response{}, false, fixed(r.Op, "request could not be built")
	}
	// Go's transport sends a request with a body again below Do only when it can rewind the body. Without
	// GetBody it does so only when nothing of the request was written, so a write check covers one send.
	req.GetBody = nil
	for k, vs := range c.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", c.userAgent)
	if r.Accept != "" {
		req.Header.Set("Accept", r.Accept)
	}
	if r.IfNoneMatch != "" {
		req.Header.Set("If-None-Match", r.IfNoneMatch)
	}
	if r.Body != nil {
		ct := r.ContentType
		if ct == "" {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}
	return c.send(ctx, r, req, tok, attempt)
}

// send is the one place a request leaves the process. The origin is checked before the token is set;
// the write check is the last step before the request is handed to the HTTP client. ctx is the call's
// context; the request carries a [wireContext] of it.
func (c *Client) send(
	ctx context.Context, r Request, req *http.Request, tok githost.Token, attempt int,
) (Response, bool, error) {
	if !onOrigin(req.URL, c.host) {
		return Response{}, false, &githost.Error{Op: r.Op, Kind: githost.ErrRejected}
	}
	scheme, value, ok := secret.Reveal(tok)
	if !ok {
		return Response{}, false, fixed(r.Op, "credential returned no token")
	}
	switch githost.Scheme(scheme) {
	case githost.Bearer:
		req.Header.Set("Authorization", "Bearer "+value)
	case githost.Basic:
		req.Header.Set("Authorization", "Basic "+basic(value))
	default:
		return Response{}, false, fixed(r.Op, "credential returned a token of an unknown scheme")
	}
	if isWrite(r.Method) {
		if c.check == nil {
			return Response{}, false, &githost.Error{Op: r.Op, Kind: githost.ErrStoppedBeforeWrite}
		}
		if ctx.Err() != nil {
			return Response{}, false, ctxError(r.Op, ctx)
		}
		if err := c.check(ctx, Send{Op: r.Op, Method: r.Method, Template: r.Template, Attempt: attempt}); err != nil {
			return Response{}, false, refusedError(r.Op, err)
		}
	}

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.emit(r, 0, attempt, time.Since(start))
		// The error of http.Client.Do holds the URL and possibly answer text: it is replaced, never wrapped.
		return Response{}, !errors.Is(err, errOffOrigin), c.transportError(ctx, r.Op, err)
	}
	out, sent, err := c.answer(ctx, r, resp)
	_ = resp.Body.Close()
	c.emit(r, resp.StatusCode, attempt, time.Since(start))
	return out, sent, err
}

// emit reports an attempt that is over to the Observer.
func (c *Client) emit(r Request, status, attempt int, d time.Duration) {
	if c.observe != nil {
		c.observe(Event{Method: r.Method, Host: c.host, Template: r.Template, Status: status, Attempt: attempt,
			Duration: d})
	}
}

// basic is the credentials of Basic authentication with an empty user name and value as the password.
func basic(value string) string { return base64.StdEncoding.EncodeToString([]byte(":" + value)) }

// answer turns an answer into a Response or an error. sent reports whether the attempt may have been
// carried out by the host although the answer does not say so.
func (c *Client) answer(ctx context.Context, r Request, resp *http.Response) (Response, bool, error) {
	code := resp.StatusCode
	write := isWrite(r.Method)
	switch {
	case success(code):
		limit := r.Limit
		if limit <= 0 {
			limit = c.bodyLimit
		}
		data, err := readBounded(resp, limit)
		if err == nil && ctx.Err() != nil {
			// An answer that raced the end of the call's context is not taken: the context ended first.
			err = ctxError(r.Op, ctx)
		}
		switch {
		case err == nil:
			return Response{Status: code, Header: resp.Header, Body: data}, false, nil
		case write:
			// The host carried out the write; only its answer is lost. Sending it again is never right.
			return Response{}, false, receiptError(ctx, r.Op, code, err)
		case errors.Is(err, githost.ErrTooLarge):
			return Response{}, false, &githost.Error{Op: r.Op, Status: code, Kind: githost.ErrTooLarge}
		default:
			return Response{}, false, c.transportError(ctx, r.Op, err)
		}
	case ctx.Err() != nil:
		return Response{}, write, ctxError(r.Op, ctx)
	case code == http.StatusNotModified:
		return Response{Status: code, Header: resp.Header}, false, nil
	case code >= 300 && code <= 399:
		// A redirect is never followed, so its answer is an error, whatever Location it names.
		return Response{}, false, &githost.Error{Op: r.Op, Status: code}
	case code == http.StatusForbidden || code == http.StatusTooManyRequests:
		if d, limited := rateLimited(resp, c.now()); limited {
			// A rate limit refused the request: nothing was carried out.
			return Response{}, false, &githost.RetryableError{RetryAfter: d, Err: &githost.Error{Op: r.Op, Status: code}}
		}
		return Response{Status: code, Header: resp.Header}, false, nil
	case code == http.StatusRequestTimeout || (code >= 500 && code <= 599):
		return Response{}, write, &githost.RetryableError{
			RetryAfter: retryafter.Header(resp.Header, c.now(), maxDelay),
			Err:        &githost.Error{Op: r.Op, Status: code},
		}
	}
	return Response{Status: code, Header: resp.Header}, false, nil
}

// readBounded reads a body of at most limit bytes, refusing a declared length over it before reading.
func readBounded(resp *http.Response, limit int64) ([]byte, error) {
	if resp.ContentLength > limit {
		return nil, githost.ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, githost.ErrTooLarge
	}
	return data, nil
}

// rateLimited reports whether a 403 or 429 is a rate limit, and the delay to wait: Retry-After, else
// the reset of an exhausted quota, else a minute. A 429 is always a rate limit; a 403 without those
// headers only when the start of its body names one, or cannot be read. The body is searched through a
// bounded reader and never kept.
func rateLimited(resp *http.Response, now time.Time) (time.Duration, bool) {
	h := resp.Header
	if h.Get("Retry-After") != "" || strings.TrimSpace(h.Get("X-RateLimit-Remaining")) == "0" {
		if d := retryafter.Header(h, now, maxDelay); d > 0 {
			return d, true
		}
		return secondaryWait, true
	}
	if resp.StatusCode == http.StatusTooManyRequests || mentionsRateLimit(resp.Body) {
		return secondaryWait, true
	}
	return 0, false
}

// mentionsRateLimit reports whether the start of a body names a rate limit. A body that cannot be read
// to the end of the probe may have hidden the message, so it counts as one: retrying is the safe side.
func mentionsRateLimit(r io.Reader) bool {
	if r == nil {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(r, maxProbeBody))
	if err != nil {
		return true
	}
	s := strings.ToLower(string(data))
	return strings.Contains(s, "rate limit") || strings.Contains(s, "abuse detection")
}

// backoff waits before the attempt after attempt: the delay the host asked for, or an exponential one
// with jitter. It returns err when the host asked for more than maxWait or the wait would pass the
// deadline of ctx, and the end of ctx when it has ended or ends during the wait.
func (c *Client) backoff(ctx context.Context, op string, attempt int, err error) error {
	if ctx.Err() != nil {
		// The context ended during the attempt or the Observer: the call is over.
		return ctxError(op, ctx)
	}
	d, asked := askedDelay(err)
	if asked && d > c.maxWait {
		return err
	}
	if !asked {
		d = c.jitter(min(c.base<<min(attempt-1, 30), c.maxWait))
	}
	// A context deadline is on the real clock, whatever clock reads Retry-After dates.
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d {
		return err
	}
	if serr := c.sleep(ctx, d); serr != nil {
		return ctxError(op, ctx)
	}
	return nil
}

func askedDelay(err error) (time.Duration, bool) {
	var r *githost.RetryableError
	if errors.As(err, &r) && r.RetryAfter > 0 {
		return r.RetryAfter, true
	}
	return 0, false
}
