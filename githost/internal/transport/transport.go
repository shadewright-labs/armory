// Package transport is the HTTP client every githost host client sends through. It is internal: the
// host packages pin the origin, and no caller of the module can pass a URL.
//
// A Client is built for one https origin (a host, with a port only on loopback, for tests) and sends
// every request through one private path, [Client.Do]. That path:
//
//   - refuses, before a credential is asked for or attached, a request whose operation label is not a
//     fixed label, whose header values Go would refuse to send, or whose URL is not https on the pinned
//     host; and a path that is not sent byte for byte as given, holds an encoded slash or backslash or a
//     dot segment, or that the request's [Template] does not name;
//   - asks the Credential for a token before each attempt, with the caller's context, and installs it
//     in the Authorization header only there, after the origin check;
//   - checks every header of the request before the write check, so no request fails inside the HTTP
//     client for a header after the check allowed it;
//   - gives each attempt its own deadline (Config.AttemptTimeout), which also bounds the read of the
//     body: a body that ends after it, even cleanly, is not taken;
//   - sends the request with a context that ends with the attempt's and has its deadline but returns no
//     value, so no client trace (net/http/httptrace) or dial trace of the caller sees the request;
//   - never follows a redirect, and drops the Location header of a 3xx below the HTTP client, so a
//     malformed one cannot hide the status. A 3xx answer to a read is an error with its status; a 3xx
//     answer to a write also matches githost.ErrOutcomeUnknown, because a host may answer a write it
//     carried out with one (303 after a POST). New puts Config.Header keys in canonical form. Blank or
//     syntactically invalid If-None-Match values and unparsable If-Modified-Since dates are refused with
//     githost.ErrRejected before any send; a 304 is accepted only for a GET or HEAD that sent a valid
//     validator, and is an error otherwise;
//   - asks for the identity encoding and reads every 2xx body, except a HEAD answer's, with a bound the
//     caller sets, counted in bytes as they arrive; it refuses a declared length over the bound before
//     reading and returns githost.ErrTooLarge past it. A 2xx answer to a write whose body cannot be read
//     is githost.ErrUnusableReceipt and is never sent again, and also githost.ErrOutcomeUnknown when an
//     earlier attempt of the call may have written as well. A Response says so in
//     EarlierAttemptUnknown, and [Response.UnusableReceipt] builds the same error for a body the host
//     client cannot decode;
//   - retries reads, and writes marked idempotent, on network errors, timeouts, 408, 429, 5xx and rate
//     limits, with exponential backoff and jitter, Retry-After and rate limit reset delays, and the
//     context honoured during every wait; a write not marked idempotent is sent once. A delay header
//     that asks for none ("0", a past date, a reset that has passed) means the next attempt starts at
//     once, within MaxAttempts; a rate limit whose headers hold no usable value waits a minute;
//   - keeps the identity of the end of a context: an error caused by a cancellation matches
//     context.Canceled, and one caused by the call's or an attempt's deadline matches
//     context.DeadlineExceeded and is a githost.RetryableError (unless the write's outcome is unknown);
//   - works on a copy of the Request taken when Do is called, so the caller's later changes to its body
//     or query do not reach any attempt;
//   - once an attempt of a write may have reached the host without an answer that says it did not
//     write, returns every later failure of the call as githost.ErrOutcomeUnknown next to its own kind,
//     an unusable receipt included. An answer that says the write was refused (a 4xx, a rate limit)
//     keeps the outcome known (without githost.ErrOutcomeUnknown) when the call's context ends right
//     after it arrives;
//   - calls the write check before every attempt of a write, and sends nothing more when it refuses;
//   - builds error text from fixed words, the operation label, the status code and a valid refusal class
//     only, never from a body, URL, header, token or a wrapped error of another package or a callback;
//   - reports one payload-free Event per attempt to the Observer, after the attempt is over.
//
// An attempt is one call of http.Client.Do, and each write check is followed by at most one write that
// the host may have read. The default transport speaks HTTP/1.1 only (see newTransport), so only Go's
// HTTP/1 rules apply below Do. Go's transport sends a request again on its own only in these cases
// (net/http: persistConn.shouldRetryRequest and Request.isReplayable, the same in Go 1.26.0 and 1.27):
//
//   - On a reused connection, when not one byte of the request was written. A request with a body is
//     not sent again even then, because Do clears GetBody; a bodiless one is, and the host never saw the
//     first.
//   - On a reused connection that failed or closed before the first byte of the answer, when the request
//     is replayable: no body (or a GetBody) and the method GET, HEAD, OPTIONS or TRACE, or an
//     Idempotency-Key or X-Idempotency-Key header. PUT is not replayable by method. Of the writes Do
//     sends, only a bodiless one with such a header would qualify, and the host may have read the first:
//     New refuses both headers in Config.Header, and Request has no header field.
//
// So every resend below the check carries bytes the host never received. The same check decision
// covers them: the request is identical, token included. Keep-alive is not turned off for writes: it
// would not change these cases, and the first case needs no new connection.
//
// What sees the request after the token is set: the http.Client this package builds (its redirect
// policy only returns the last answer and never sees a token-bearing request of its own), the default
// transport or Config.RoundTripper, and nothing else. The Credential, the write check, the Observer,
// Jitter, Now and Sleep never get the request. Errors of net/http (a *url.Error holds the method and
// URL, not the headers) are replaced, never wrapped. No code of this package formats a request, so a
// panic, in a callback or below, prints no header value. This package never writes a token to any log,
// error or trace. What it cannot control is Go's HTTP/1.1 client logging unsolicited bytes a host
// sends on an idle kept-alive connection through the process's default logger: those are peer bytes,
// not bytes the client sent, unless the host echoed them back.
package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/shadewright-labs/armory/githost"
	"github.com/shadewright-labs/armory/githost/internal/secret"
	"github.com/shadewright-labs/armory/githost/retryafter"
)

const (
	opNew = "transport.New"
	// opDo labels the error of a request whose own label cannot be used.
	opDo = "transport.Do"
)

// Defaults for [Config].
const (
	DefaultMaxAttempts    = 4
	DefaultBaseDelay      = 500 * time.Millisecond
	DefaultMaxWait        = 15 * time.Second
	DefaultTimeout        = 2 * time.Minute
	DefaultAttemptTimeout = 60 * time.Second
	// DefaultBodyLimit is the default bound of a 2xx answer body when Config.BodyLimit and Request.Limit
	// are not set.
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
	// Host is the one host the client reaches over https, such as "api.github.com". A port from 1 to
	// 65535 is allowed only on localhost or a loopback IP, for tests; a scheme, path, user or query is not.
	Host string
	// Repo is the repository tokens are asked for.
	Repo githost.RepoRef
	// Credential returns a token for each attempt, retries included. Required: nil, or any typed nil
	// (function, pointer, map, slice, channel or interface) behind the interface, is refused.
	Credential githost.Credential
	// WriteCheck is called before each attempt of a write. Nil allows no write.
	WriteCheck WriteCheck
	// Observer, when set, gets one Event per attempt.
	Observer Observer
	// Header holds fixed headers sent with every request, such as an API version. Authorization,
	// Proxy-Authorization, Idempotency-Key, X-Idempotency-Key and Accept-Encoding are refused, and so
	// is a name or value Go would refuse to send.
	Header http.Header
	// UserAgent is sent with every request; empty means "armory-githost". A value Go would refuse to send
	// is refused.
	UserAgent string

	// Zero or less means the default for each of these.
	MaxAttempts    int           // attempts of one request; DefaultMaxAttempts
	BaseDelay      time.Duration // first backoff delay, doubled per attempt up to MaxWait; DefaultBaseDelay
	MaxWait        time.Duration // longest delay waited in place; DefaultMaxWait
	Timeout        time.Duration // one call of Do, retries included; DefaultTimeout
	AttemptTimeout time.Duration // one attempt, body read included; DefaultAttemptTimeout
	BodyLimit      int64         // default bound of a 2xx body, in bytes as they arrive; DefaultBodyLimit

	// Proxy selects the proxy for requests sent through the default HTTP transport. Nil (the default)
	// uses no proxy and ignores the process environment (HTTPS_PROXY, NO_PROXY). Proxy use is opt-in: a
	// caller that needs a proxy sets this explicitly (for example, http.ProxyFromEnvironment). Ignored
	// when RoundTripper is set.
	//
	// A proxy learns the pinned host and port, the timing and size of each exchange, and may resolve the
	// origin host name itself. For an HTTP proxy, Go opens a CONNECT tunnel so the Authorization header
	// travels inside TLS to the pinned host; Go also accepts SOCKS proxies (socks5, socks5h), where TLS
	// to the pinned host still protects the header. Confidentiality against the proxy depends on the
	// host certificate trust store, including any trusted TLS-inspection CA.
	//
	// If Proxy returns a URL whose scheme is not "https" and which carries userinfo, the request fails
	// before any connection is opened with githost.ErrRejected: proxy credentials must not travel in
	// clear text.
	Proxy func(*http.Request) (*url.URL, error)

	// RoundTripper replaces the default HTTP transport; tests use it to trust a loopback server's
	// certificate. Only code of this module sets it; no exported API passes one through.
	//
	// It is a trusted component and takes on what the default transport guarantees: it gets every request
	// with its Authorization header, so it sees the token; whether it speaks HTTP/2, logs headers (as Go's
	// HTTP/2 code does with GODEBUG=http2debug), undoes a compression, or sends a request again below
	// the write check is then its concern. Do still checks the origin of each request it is handed, asks
	// for the identity encoding, and clears GetBody.
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
	attemptMax time.Duration
	bodyLimit  int64
	httpClient *http.Client
	jitter     func(time.Duration) time.Duration
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
}

// Request is one HTTP request to the pinned origin.
type Request struct {
	// Op is a fixed label for errors, such as "BranchHead": 1 to 64 ASCII letters, digits, ".", "_" and
	// "-". Do refuses any other label and does not print it.
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
	// IfNoneMatch is sent as If-None-Match. When set it must be "*" or a comma-separated list of
	// optionally weak quoted entity-tags; otherwise Do refuses the request with githost.ErrRejected
	// before sending. For GET and HEAD, a 304 comes back as a Response.
	IfNoneMatch string
	Limit       int64 // bound of the 2xx body in bytes; zero or less means the client's default
	// Idempotent marks a write that may be sent again in the call, such as a content-addressed object or a
	// ref move with an expected old value. Reads are always retried; a write without it is sent once.
	Idempotent bool
}

// Response is a non-error answer from [Client.Do]: a 2xx, a 304 to a conditional GET or HEAD, or a
// non-rate-limit 4xx (or other non-3xx, non-5xx status). Body is read for non-HEAD 2xx answers only;
// any other status comes back without a body, for the host client to classify with [StatusError].
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	// EarlierAttemptUnknown reports that an earlier attempt of this call was a write that may have
	// reached the host without an answer that says it did not write. It is only ever set on a 2xx answer
	// to a write. A host client that cannot use such an answer returns [Response.UnusableReceipt].
	EarlierAttemptUnknown bool

	write bool // the answer is to a write
}

// UnusableReceipt is the error of a 2xx answer to a write whose body the host client cannot use: it does
// not decode, or does not describe the write. It matches githost.ErrUnusableReceipt with a
// *githost.Error that holds op and the status, and githost.ErrOutcomeUnknown as well when
// EarlierAttemptUnknown is set, so the caller learns that at least one write was accepted and whether an
// earlier one was applied too is unknown. It never matches githost.ErrRetryable.
//
// For any other Response (an answer to a read, a status that is not 2xx, or a Response that Do did not
// return) it is a *githost.Error with op and the status and no kind. An op that is not a valid label is
// replaced with a fixed one.
func (r Response) UnusableReceipt(op string) error {
	if !validOp(op) {
		op = opDo
	}
	if !r.write || !success(r.Status) {
		return &githost.Error{Op: op, Status: r.Status}
	}
	return finalError(op, &githost.Error{Op: op, Status: r.Status, Kind: githost.ErrUnusableReceipt},
		r.EarlierAttemptUnknown)
}

// New validates cfg and returns a Client. It makes no network call.
func New(cfg Config) (*Client, error) {
	invalid := &githost.Error{Op: opNew, Kind: githost.ErrInvalidConfig}
	if !validHost(cfg.Host) || isNil(cfg.Credential) || !validFieldValue(cfg.UserAgent) {
		return nil, invalid
	}
	for k, vs := range cfg.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Authorization", "Proxy-Authorization", "Idempotency-Key", "X-Idempotency-Key", "Accept-Encoding":
			// The token is set per attempt only; Proxy-Authorization must not be sent to the origin host;
			// an idempotency key would let Go's transport resend a bodiless write the host may have read,
			// below the write check; the encoding is always identity.
			return nil, invalid
		}
		if !validFieldName(k) {
			return nil, invalid
		}
		for _, v := range vs {
			if !validFieldValue(v) {
				return nil, invalid
			}
		}
	}
	c := &Client{
		host:       cfg.Host,
		repo:       cfg.Repo,
		cred:       cfg.Credential,
		check:      cfg.WriteCheck,
		observe:    cfg.Observer,
		header:     canonical(cfg.Header),
		userAgent:  cfg.UserAgent,
		attempts:   orDefault(cfg.MaxAttempts, DefaultMaxAttempts),
		base:       orDefault(cfg.BaseDelay, DefaultBaseDelay),
		maxWait:    orDefault(cfg.MaxWait, DefaultMaxWait),
		timeout:    orDefault(cfg.Timeout, DefaultTimeout),
		attemptMax: orDefault(cfg.AttemptTimeout, DefaultAttemptTimeout),
		bodyLimit:  orDefault(cfg.BodyLimit, DefaultBodyLimit),
		jitter:     cfg.Jitter,
		now:        cfg.Now,
		sleep:      cfg.Sleep,
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
	c.httpClient = newHTTPClient(cfg.RoundTripper, cfg.Host, cfg.Proxy)
	return c, nil
}

// canonical copies h with every key in canonical form, so a key in another case is checked and sent like
// the canonical one, and values of keys that differ only in case are kept together.
func canonical(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		ck := http.CanonicalHeaderKey(k)
		out[ck] = append(out[ck], vs...)
	}
	return out
}

func orDefault[T int | int64 | time.Duration](v, d T) T {
	if v <= 0 {
		return d
	}
	return v
}

// validHost accepts a host name or IP, with a port from 1 to 65535 only on a loopback host (localhost or
// a loopback IP), and nothing else.
func validHost(h string) bool {
	if h == "" || strings.ContainsAny(h, "/?#@\\ \t\r\n") || strings.HasSuffix(h, ":") {
		return false
	}
	u, err := url.Parse("https://" + h)
	if err != nil || u.Host != h || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" {
		return false
	}
	port := u.Port()
	if port == "" {
		return true
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && port[0] != '0' && allIn(port, "0123456789") && n >= 1 && n <= 65535
}

// isNil reports whether v is nil, or a nil function, pointer, map, slice or channel behind an interface.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Func, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	}
	return false
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

// dialer opens the connections of the default HTTP transport. It has a resolver of its own, so a
// change to net.DefaultResolver elsewhere in the process does not reach it.
var dialer = net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Resolver: &net.Resolver{}}

// newHTTPClient never follows a redirect (Go keeps the Authorization header on some), and checks the
// origin again below the client, in case anything below Do builds a request of its own. The attempt
// timeout is not the client's: Do sets it on each attempt's context, so it covers the body read too.
func newHTTPClient(rt http.RoundTripper, host string, proxy func(*http.Request) (*url.URL, error)) *http.Client {
	if rt == nil {
		rt = newTransport(proxy)
	}
	return &http.Client{
		Transport: pinned{host: host, next: rt},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// newTransport builds the default HTTP transport from explicit settings. It copies nothing from
// http.DefaultTransport, so no dial hook, TLS setting or proxy another part of the process installed
// there reaches a token-bearing request. It requires TLS 1.2 or later and bounds connection setup, the
// wait for headers and the header size.
//
// It speaks HTTP/1.1 only: it offers only "http/1.1" in TLS, has an empty TLSNextProto map and allows
// only HTTP/1 in Protocols. Go's HTTP/2 code is never used, so neither its resends below Do (Go 1.26.0
// sends a bodiless request again after a PROTOCOL_ERROR from the peer, which does not say the peer left
// the request alone) nor its debug log (GODEBUG=http2debug prints every header it encodes, Authorization
// included) applies.
//
// It does not ask for or undo a compression: Do asks for the identity encoding, and the body bound
// counts the bytes as they arrive.
//
// By default Proxy is nil, so the transport uses no proxy and ignores the process environment
// (HTTPS_PROXY, NO_PROXY). Proxy use is opt-in through Config.Proxy. When set, the proxy function is
// wrapped to refuse any non-https proxy URL that carries userinfo before a connection is opened. A
// proxy learns the pinned host and port, the timing and size of each exchange, and may resolve the
// origin host name itself. With an HTTP proxy, Go opens a CONNECT tunnel so the Authorization header
// travels inside TLS to the pinned host; Go also accepts SOCKS proxies (socks5, socks5h), where TLS to
// the pinned host still protects the header. Confidentiality against the proxy depends on the host
// certificate trust store, including any trusted TLS-inspection CA.
func newTransport(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	var p func(*http.Request) (*url.URL, error)
	if proxy != nil {
		p = checkedProxy(proxy)
	}
	return &http.Transport{
		Proxy:                  p,
		DialContext:            dialer.DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSHandshakeTimeout:    10 * time.Second,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
		Protocols:              &protocols,
		DisableCompression:     true,
		MaxIdleConns:           16,
		IdleConnTimeout:        90 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxHeaderBytes,
	}
}

// errCleartextProxyAuth is returned by checkedProxy when a non-https proxy URL carries userinfo. It
// never reaches a caller as is.
var errCleartextProxyAuth = errors.New("proxy credential requires https proxy")

// checkedProxy wraps fn and refuses any non-https proxy URL with userinfo before a connection is opened.
func checkedProxy(fn func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		u, err := fn(req)
		if err != nil || u == nil {
			return u, err
		}
		if !strings.EqualFold(u.Scheme, "https") && u.User != nil {
			return nil, errCleartextProxyAuth
		}
		return u, nil
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
	resp, err := p.next.RoundTrip(r)
	if err == nil && resp != nil && resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		// http.Client parses Location before its redirect policy runs and fails on a malformed one, which
		// would lose the status. Nothing here follows a redirect, so the header is dropped.
		resp.Header.Del("Location")
	}
	return resp, err
}

// onOrigin reports whether u is an https URL on host with no user information.
func onOrigin(u *url.URL, host string) bool {
	return u != nil && u.Scheme == "https" && u.Host == host && u.User == nil && u.Opaque == ""
}

// Do sends r, retrying transient failures. See the package documentation for the rules.
//
// Do works on a copy of r taken when it is called, so a caller that changes r.Body or r.Query while the
// call runs changes nothing that is checked or sent.
func (c *Client) Do(ctx context.Context, r Request) (Response, error) {
	r = r.frozen()
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
		earlier := mayHaveWritten
		resp, sent, err := c.once(ctx, r, u, attempt)
		mayHaveWritten = mayHaveWritten || (write && sent)
		switch {
		case err == nil && (!mayHaveWritten || success(resp.Status)):
			resp.EarlierAttemptUnknown = earlier
			return resp, nil
		case err == nil:
			// An answer such as 409 or 422 after an attempt whose answer was lost may be about that attempt.
			return Response{}, outcomeUnknown(r.Op, StatusError(r.Op, resp.Status))
		case errors.Is(err, githost.ErrStoppedBeforeWrite):
			// The write check refused, or there is none: the call stops whatever else the error matches,
			// a deadline included, and the refusal keeps its class.
			return Response{}, finalError(r.Op, err, mayHaveWritten)
		case errors.Is(err, githost.ErrUnusableReceipt):
			// This attempt was accepted. When an earlier one may have written too, the host may hold two
			// writes, so the caller learns both.
			return Response{}, finalError(r.Op, err, mayHaveWritten)
		case !errors.Is(err, githost.ErrRetryable) || attempt >= c.attempts || (write && !r.Idempotent):
			return Response{}, finalError(r.Op, err, mayHaveWritten)
		}
		if werr := c.backoff(ctx, r.Op, attempt, err); werr != nil {
			return Response{}, finalError(r.Op, werr, mayHaveWritten)
		}
	}
}

// frozen returns a copy of r that shares no slice or map with it. A nil body or query stays nil and an
// empty one stays empty, since a nil body is sent without a Content-Type.
func (r Request) frozen() Request {
	if r.Body != nil {
		r.Body = append(make([]byte, 0, len(r.Body)), r.Body...)
	}
	if r.Query != nil {
		q := make(url.Values, len(r.Query))
		for k, vs := range r.Query {
			q[k] = append([]string(nil), vs...)
		}
		r.Query = q
	}
	return r
}

func isWrite(method string) bool { return method != http.MethodGet && method != http.MethodHead }

func success(status int) bool { return status >= 200 && status <= 299 }

// buildURL makes the URL of r on the pinned origin and refuses anything else: an operation label that
// is not a fixed label, a method outside the set, a header value Go would refuse to send, a blank or
// malformed If-None-Match value, an If-Modified-Since value that http.ParseTime rejects, a path that
// does not start with one slash, holds a query, a fragment or a backslash, would name another host, is
// not sent as given, holds an encoded slash or backslash or a dot segment, or that the template does
// not name.
func (c *Client) buildURL(r Request) (*url.URL, error) {
	if !validOp(r.Op) {
		return nil, &githost.Error{Op: opDo, Kind: githost.ErrRejected}
	}
	rejected := &githost.Error{Op: r.Op, Kind: githost.ErrRejected}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return nil, rejected
	}
	if !validFieldValue(r.Accept) || !validFieldValue(r.IfNoneMatch) || !validFieldValue(r.ContentType) {
		return nil, rejected
	}
	if r.IfNoneMatch != "" && !validIfNoneMatch(r.IfNoneMatch) {
		return nil, rejected
	}
	if inm := c.header.Values("If-None-Match"); r.IfNoneMatch == "" && len(inm) > 0 && !validIfNoneMatchField(inm) {
		return nil, rejected
	}
	if ims := c.header.Values("If-Modified-Since"); len(ims) > 0 {
		if len(ims) != 1 || !validIfModifiedSince(ims[0]) {
			return nil, rejected
		}
	}
	if !strings.HasPrefix(r.Path, "/") || strings.HasPrefix(r.Path, "//") ||
		strings.ContainsAny(r.Path, "?#\\") || !cleanSegments(r.Path) || !r.Template.names(r.Path) {
		return nil, rejected
	}
	u, err := url.Parse("https://" + c.host + r.Path)
	// The path goes on the wire as EscapedPath; the template was checked against r.Path, so they must be
	// the same bytes.
	if err != nil || !onOrigin(u, c.host) || u.RawQuery != "" || u.Fragment != "" || u.EscapedPath() != r.Path {
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
	if write && c.check == nil {
		// A nil check refuses every write. It refuses before the credential is asked, so no write token is
		// minted for a write that will not be sent and no credential error hides the refusal.
		return Response{}, false, &githost.Error{Op: r.Op, Kind: githost.ErrStoppedBeforeWrite}
	}
	// The credential is the product's: it gets the caller's context, values included.
	tok, err := c.cred.Token(ctx, githost.Scope{Repo: c.repo, Write: write})
	if err != nil {
		return Response{}, false, credentialError(ctx, r.Op, err)
	}
	var body io.Reader
	if r.Body != nil {
		body = bytes.NewReader(r.Body)
	}
	// The attempt has a deadline of its own, which also bounds the read of the body.
	actx, stop := context.WithTimeout(ctx, c.attemptMax)
	defer stop()
	// The wire request gets a context without the caller's values, so no client trace of the caller sees
	// it, its Authorization header included.
	req, err := http.NewRequestWithContext(wireContext{actx}, r.Method, u.String(), body)
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
	// The body bound counts the bytes that arrive; no transport may undo a compression below it.
	req.Header.Set("Accept-Encoding", "identity")
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
	return c.send(ctx, actx, stop, r, req, tok, attempt)
}

// send is the one place a request leaves the process. The origin is checked before the token is set;
// every header is checked before the write check, and the write check is followed by a check of the
// call and attempt contexts before the request is handed to the HTTP client, so an attempt that timed
// out during the check fails before send. ctx is the call's context and actx the attempt's, which the
// request carries as a [wireContext]; stop ends the attempt's.
func (c *Client) send(
	ctx, actx context.Context, stop context.CancelFunc, r Request, req *http.Request, tok githost.Token, attempt int,
) (Response, bool, error) {
	if !onOrigin(req.URL, c.host) {
		return Response{}, false, &githost.Error{Op: r.Op, Kind: githost.ErrRejected}
	}
	scheme, value, ok := secret.Reveal(tok)
	if !ok {
		return Response{}, false, fixed(r.Op, "credential returned no token")
	}
	var auth string
	switch githost.Scheme(scheme) {
	case githost.Bearer:
		auth = "Bearer " + value
	case githost.Basic:
		auth = "Basic " + basic(value)
	default:
		return Response{}, false, fixed(r.Op, "credential returned a token of an unknown scheme")
	}
	if !validFieldValue(auth) {
		return Response{}, false, fixed(r.Op, "credential returned a token that cannot be sent")
	}
	req.Header.Set("Authorization", auth)
	// A header Go refuses to send fails inside the HTTP client, after the write check, where it could not
	// be told from a send. So every header is checked here, before the check.
	if !validHeader(req.Header) {
		return Response{}, false, &githost.Error{Op: r.Op, Kind: githost.ErrRejected}
	}
	if isWrite(r.Method) {
		if c.check == nil {
			return Response{}, false, &githost.Error{Op: r.Op, Kind: githost.ErrStoppedBeforeWrite}
		}
		if ctx.Err() != nil {
			return Response{}, false, ctxError(r.Op, ctx)
		}
		if err := c.check(ctx, Send{Op: r.Op, Method: r.Method, Template: r.Template, Attempt: attempt}); err != nil {
			return Response{}, false, refusedError(ctx, r.Op, err)
		}
	}
	if ctx.Err() != nil {
		return Response{}, false, ctxError(r.Op, ctx)
	}
	if actx.Err() != nil {
		return Response{}, false, timedOut(r.Op)
	}

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		stop()
		c.emit(r, 0, attempt, time.Since(start))
		// The error of http.Client.Do holds the URL and possibly answer text: it is replaced, never wrapped.
		preSend := errors.Is(err, errOffOrigin) || errors.Is(err, errCleartextProxyAuth)
		return Response{}, !preSend, c.transportError(ctx, r.Op, err)
	}
	out, sent, err := c.answer(ctx, actx, r, req, resp)
	_ = resp.Body.Close()
	stop()
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
// carried out by the host although the answer does not say so. ctx is the call's context, actx the
// attempt's, and req the request as sent.
func (c *Client) answer(
	ctx, actx context.Context, r Request, req *http.Request, resp *http.Response,
) (Response, bool, error) {
	if success(resp.StatusCode) {
		return c.receipt(ctx, actx, r, resp)
	}
	out, sent, err := c.classify(r, req, resp)
	if ctx.Err() != nil {
		// An answer or error that raced the end of the call's context is not returned; whether it says the
		// write was not carried out is kept.
		return Response{}, sent, ctxError(r.Op, ctx)
	}
	return out, sent, err
}

// receipt reads a 2xx answer. A body that is past the bound, cannot be read, or ends after the call or
// the attempt ended is not taken; for a write that is an unusable receipt, never sent again.
func (c *Client) receipt(ctx, actx context.Context, r Request, resp *http.Response) (Response, bool, error) {
	code := resp.StatusCode
	write := isWrite(r.Method)
	limit := r.Limit
	if limit <= 0 {
		limit = c.bodyLimit
	}
	var (
		data []byte
		err  error
	)
	if r.Method != http.MethodHead {
		// A HEAD answer has no body; its Content-Length describes the resource, not the answer.
		data, err = readBounded(resp, limit)
	}
	if err == nil {
		switch {
		case ctx.Err() != nil:
			// The context ended first, even if the body arrived.
			err = ctxError(r.Op, ctx)
		case actx.Err() != nil:
			// The attempt's deadline passed before the body ended, even if it then ended cleanly.
			err = context.DeadlineExceeded
		}
	}
	switch {
	case err == nil:
		return Response{Status: code, Header: resp.Header, Body: data, write: write}, false, nil
	case write:
		// The host carried out the write; only its answer is lost. Sending it again is never right.
		return Response{}, false, receiptError(actx, r.Op, code, err)
	case errors.Is(err, githost.ErrTooLarge):
		return Response{}, false, &githost.Error{Op: r.Op, Status: code, Kind: githost.ErrTooLarge}
	default:
		return Response{}, false, c.transportError(ctx, r.Op, err)
	}
}

// classify turns an answer that is not 2xx into a Response or an error.
func (c *Client) classify(r Request, req *http.Request, resp *http.Response) (Response, bool, error) {
	code := resp.StatusCode
	write := isWrite(r.Method)
	switch {
	case code == http.StatusNotModified && !write && hasValidCondition(req.Header):
		return Response{Status: code, Header: resp.Header}, false, nil
	case code >= 300 && code <= 399:
		// A redirect is never followed, so its answer is an error, whatever Location it named. A host may
		// answer a write it carried out with one (303 after a POST), so for a write the outcome is unknown.
		return Response{}, write, &githost.Error{Op: r.Op, Status: code}
	case code == http.StatusForbidden || code == http.StatusTooManyRequests:
		if d, asked, limited := rateLimited(resp, c.now()); limited {
			// A rate limit refused the request: nothing was carried out.
			return Response{}, false, retryAfter(d, asked, &githost.Error{Op: r.Op, Status: code})
		}
		return Response{Status: code, Header: resp.Header}, false, nil
	case code == http.StatusRequestTimeout || (code >= 500 && code <= 599):
		d, herr := retryafter.Header(resp.Header, c.now(), maxDelay)
		return Response{}, write, retryAfter(d, herr == nil, &githost.Error{Op: r.Op, Status: code})
	}
	// A 4xx says the request was refused. Any other status says nothing about a write.
	return Response{Status: code, Header: resp.Header}, write && (code < 400 || code > 499), nil
}

// readBounded reads a body of at most limit bytes as they arrive, refusing a declared length over it
// before reading.
func readBounded(resp *http.Response, limit int64) ([]byte, error) {
	if resp.ContentLength > limit {
		return nil, githost.ErrTooLarge
	}
	n := limit
	if n < math.MaxInt64 {
		// One byte past the bound tells a body at the bound from a longer one.
		n++
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, n))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, githost.ErrTooLarge
	}
	return data, nil
}

// rateLimited reports whether a 403 or 429 is a rate limit, and the delay to wait: Retry-After, else
// the reset of an exhausted quota, else a minute. asked reports that the headers asked for the delay,
// which may be zero ("0", a past date or a reset that has passed: retry at once); an unreadable value
// gets the minute. A 429 is always a rate limit; a 403 without those headers only when the start of its
// body names one, or cannot be read. The body is searched through a bounded reader and never kept.
func rateLimited(resp *http.Response, now time.Time) (d time.Duration, asked, limited bool) {
	h := resp.Header
	if h.Get("Retry-After") != "" || strings.TrimSpace(h.Get("X-RateLimit-Remaining")) == "0" {
		if d, err := retryafter.Header(h, now, maxDelay); err == nil {
			return d, true, true
		}
		return secondaryWait, false, true
	}
	if resp.StatusCode == http.StatusTooManyRequests || mentionsRateLimit(resp.Body) {
		return secondaryWait, false, true
	}
	return 0, false, false
}

// retryNow is a RetryableError whose answer asked for a delay of zero: the next attempt starts at once,
// with no backoff. Do never returns it; finalError unwraps it.
type retryNow struct{ err *githost.RetryableError }

func (e *retryNow) Error() string { return e.err.Error() }
func (e *retryNow) Unwrap() error { return e.err }

// retryAfter is the retryable error of an answer that may be tried again after d. When the answer's
// headers asked for a delay of zero, the next attempt is not delayed.
func retryAfter(d time.Duration, asked bool, err error) error {
	re := &githost.RetryableError{RetryAfter: d, Err: err}
	if asked && d == 0 {
		return &retryNow{err: re}
	}
	return re
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

// backoff waits before the attempt after attempt: the delay asked for in err, or an exponential one
// with jitter. It returns err when the asked delay exceeds maxWait or the wait would pass the deadline
// of ctx, and the end of ctx when it has ended or ends during the wait.
func (c *Client) backoff(ctx context.Context, op string, attempt int, err error) error {
	if ctx.Err() != nil {
		// The context ended during the attempt or the Observer: the call is over.
		return ctxError(op, ctx)
	}
	if _, now := errors.AsType[*retryNow](err); now {
		// The host asked for no delay. MaxAttempts and the deadline still bound the call.
		return nil
	}
	d, asked := askedDelay(err)
	if asked && d > c.maxWait {
		return err
	}
	if !asked {
		d = c.jitter(c.expDelay(attempt))
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

// expDelay is the backoff delay after attempt: BaseDelay doubled for each earlier attempt, capped at
// MaxWait. It doubles step by step, so no shift or product overflows.
func (c *Client) expDelay(attempt int) time.Duration {
	d := min(c.base, c.maxWait)
	for i := 1; i < attempt && d < c.maxWait; i++ {
		if d > c.maxWait/2 {
			return c.maxWait
		}
		d *= 2
	}
	return d
}

func askedDelay(err error) (time.Duration, bool) {
	var r *githost.RetryableError
	if errors.As(err, &r) && r != nil && r.RetryAfter > 0 {
		return r.RetryAfter, true
	}
	return 0, false
}
