package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/githost"
)

// defaultTransport returns the HTTP transport New built when Config.RoundTripper is nil.
func defaultTransport(t *testing.T, c *Client) *http.Transport {
	t.Helper()
	p, ok := c.httpClient.Transport.(pinned)
	if !ok {
		t.Fatalf("client transport is %T, want pinned", c.httpClient.Transport)
	}
	tr, ok := p.next.(*http.Transport)
	if !ok {
		t.Fatalf("transport below the pin is %T, want *http.Transport", p.next)
	}
	return tr
}

// TestDefaultTransport: with no RoundTripper, the client requires TLS 1.2, bounds dial, handshake, the
// wait for headers and their size, bounds each attempt, follows no redirect, speaks HTTP/1.1 only, asks
// for no compression, has no TLS dial hook, and uses no proxy by default.
func TestDefaultTransport(t *testing.T) {
	c, err := New(Config{Host: apiHost, Credential: githost.CredentialFunc(
		func(context.Context, githost.Scope) (githost.Token, error) { return githost.Token{}, nil })})
	if err != nil {
		t.Fatal(err)
	}
	tr := defaultTransport(t, c)
	t.Run("client and pin", func(t *testing.T) { assertDefaultClient(t, c) })
	t.Run("TLS and timeouts", func(t *testing.T) { assertDefaultTLSAndTimeouts(t, tr) })
	t.Run("HTTP1 and dialer", func(t *testing.T) { assertDefaultHTTP1AndDialer(t, tr) })
	t.Run("hooks compression and proxy", func(t *testing.T) { assertDefaultHooksAndProxy(t, tr) })
}

func assertDefaultClient(t *testing.T, c *Client) {
	t.Helper()
	if c.attemptMax != DefaultAttemptTimeout || c.httpClient.CheckRedirect == nil {
		t.Errorf("attempt timeout %s, redirect policy set %v", c.attemptMax, c.httpClient.CheckRedirect != nil)
	}
	if p := c.httpClient.Transport.(pinned); p.host != apiHost {
		t.Errorf("pinned host %q", p.host)
	}
}

func assertDefaultTLSAndTimeouts(t *testing.T, tr *http.Transport) {
	t.Helper()
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("TLS config %+v, want MinVersion TLS 1.2", tr.TLSClientConfig)
	}
	if tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("certificate verification is off")
	}
	if tr.TLSHandshakeTimeout != 10*time.Second || tr.ResponseHeaderTimeout != 30*time.Second {
		t.Errorf("handshake timeout %s, header timeout %s", tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout)
	}
	if tr.MaxResponseHeaderBytes != 256<<10 {
		t.Errorf("header limit %d", tr.MaxResponseHeaderBytes)
	}
}

func assertDefaultHTTP1AndDialer(t *testing.T, tr *http.Transport) {
	t.Helper()
	if dialer.Timeout != 10*time.Second || dialer.KeepAlive != 30*time.Second || tr.DialContext == nil {
		t.Errorf("dialer %+v, dial func set %v", dialer, tr.DialContext != nil)
	}
	if tr.ForceAttemptHTTP2 || tr.TLSNextProto == nil || len(tr.TLSNextProto) != 0 ||
		!reflect.DeepEqual(tr.TLSClientConfig.NextProtos, []string{"http/1.1"}) ||
		tr.Protocols == nil || tr.Protocols.HTTP2() || tr.Protocols.UnencryptedHTTP2() || !tr.Protocols.HTTP1() {
		t.Errorf("HTTP/2 is not off: force %v, next protos %v %v, protocols %v", tr.ForceAttemptHTTP2,
			tr.TLSNextProto, tr.TLSClientConfig.NextProtos, tr.Protocols)
	}
}

func assertDefaultHooksAndProxy(t *testing.T, tr *http.Transport) {
	t.Helper()
	if !tr.DisableCompression || dialer.Resolver == nil || dialer.Resolver == net.DefaultResolver {
		t.Errorf("compression off %v, private resolver %v", tr.DisableCompression,
			dialer.Resolver != nil && dialer.Resolver != net.DefaultResolver)
	}
	if tr.DialTLSContext != nil {
		t.Error("DialTLSContext is set")
	}
	if tr.DialTLS != nil { //nolint:staticcheck // the test asserts the deprecated hook is unset
		t.Error("DialTLS is set")
	}
	if tr.Proxy != nil {
		t.Error("default transport has a proxy configured; want nil")
	}
}

// TestProxyNeverSeesTheToken: through an opt-in Config.Proxy, an https request goes in a CONNECT tunnel.
// The proxy sees the host and port, and Proxy-Authorization when authenticated over an https proxy, but
// never the Authorization header: that travels inside TLS to the host.
func TestProxyNeverSeesTheToken(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		name := "http proxy without auth"
		if authenticated {
			name = "https proxy with auth"
		}
		t.Run(name, func(t *testing.T) {
			var (
				gotAuth string
				mu      sync.Mutex
				methods []string
				headers []http.Header
				targets []string
			)
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotAuth = r.Header.Get("Authorization")
				mu.Unlock()
				okBody(w)
			}))
			t.Cleanup(srv.Close)
			host := srv.Listener.Addr().String()

			proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				methods = append(methods, r.Method)
				headers = append(headers, r.Header.Clone())
				targets = append(targets, r.Host)
				mu.Unlock()
				if r.Method != http.MethodConnect {
					http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
					return
				}
				// The tunnel goes only to the test server, whatever the request names; the target is checked below.
				up, err := (&net.Dialer{}).DialContext(r.Context(), "tcp", host)
				if err != nil {
					http.Error(w, "dial", http.StatusBadGateway)
					return
				}
				down, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					_ = up.Close()
					return
				}
				_, _ = io.WriteString(down, "HTTP/1.1 200 Connection established\r\n\r\n")
				go func() { _, _ = io.Copy(up, down); _ = up.Close() }()
				go func() { _, _ = io.Copy(down, up); _ = down.Close() }()
			})
			var proxy *httptest.Server
			if authenticated {
				proxy = httptest.NewTLSServer(proxyHandler)
			} else {
				proxy = httptest.NewServer(proxyHandler)
			}
			t.Cleanup(proxy.Close)

			pu, err := url.Parse(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			if authenticated {
				pu.User = url.UserPassword("proxyuser", strings.Join([]string{"proxy", "pass", "7e1a"}, "-"))
			}

			c, err := New(Config{
				Host:  host,
				Proxy: http.ProxyURL(pu),
				Credential: githost.CredentialFunc(
					func(context.Context, githost.Scope) (githost.Token, error) {
						return githost.NewToken(githost.Bearer, tokenCanary, time.Time{}), nil
					}),
			})
			if err != nil {
				t.Fatal(err)
			}
			tr := defaultTransport(t, c)
			t.Cleanup(tr.CloseIdleConnections)
			pool := x509.NewCertPool()
			pool.AddCert(srv.Certificate())
			if authenticated {
				pool.AddCert(proxy.Certificate())
			}
			tr.TLSClientConfig.RootCAs = pool

			if _, err := c.Do(t.Context(), get()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if gotAuth != "Bearer "+tokenCanary {
				t.Errorf("host got Authorization %q", gotAuth)
			}
			if !reflect.DeepEqual(methods, []string{http.MethodConnect}) || !reflect.DeepEqual(targets, []string{host}) {
				t.Fatalf("proxy saw %v to %v, want one CONNECT to the host", methods, targets)
			}
			h := headers[0]
			if authenticated && h.Get("Proxy-Authorization") == "" {
				t.Error("an authenticated https proxy got no Proxy-Authorization")
			}
			if _, ok := h["Authorization"]; ok {
				t.Error("the proxy got an Authorization header")
			}
			for k, vs := range h {
				for _, v := range vs {
					if strings.Contains(v, tokenCanary) {
						t.Errorf("the proxy got the token in %s", k)
					}
				}
			}
		})
	}
}

// TestProxyCleartextUserinfoRefused: a non-https proxy URL with userinfo is refused with ErrRejected
// before any connection is opened, and its credentials never appear in the error.
func TestProxyCleartextUserinfoRefused(t *testing.T) {
	proxyPass := strings.Join([]string{"proxy", "secret", "4d9c"}, "-")
	var (
		mu   sync.Mutex
		hits int
	)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		http.Error(w, "unexpected", http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	pu, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	pu.User = url.UserPassword("proxyuser", proxyPass)

	socksURL := &url.URL{
		Scheme: "socks5",
		User:   url.UserPassword("proxyuser", proxyPass),
		Host:   proxy.Listener.Addr().String(),
	}

	cases := []struct {
		name  string
		proxy func(*http.Request) (*url.URL, error)
		req   Request
	}{
		{"http proxy with userinfo on GET", http.ProxyURL(pu), get()},
		{"http proxy with userinfo on POST", http.ProxyURL(pu), post(false)},
		{"socks5 proxy with userinfo on GET", http.ProxyURL(socksURL), get()},
		{"env proxy with http userinfo", http.ProxyFromEnvironment, get()},
	}
	t.Setenv("HTTPS_PROXY", pu.String())
	t.Setenv("NO_PROXY", "")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(Config{
				Host:        apiHost,
				Proxy:       tc.proxy,
				Credential:  tokenCred(tokenCanary),
				WriteCheck:  func(context.Context, Send) error { return nil },
				MaxAttempts: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			tr := defaultTransport(t, c)
			t.Cleanup(tr.CloseIdleConnections)

			r := tc.req
			r.Path, r.Template, r.Query = "/x", "/x", nil
			_, err = c.Do(t.Context(), r)
			if !errors.Is(err, githost.ErrRejected) {
				t.Fatalf("Do = %v, want ErrRejected", err)
			}
			if errors.Is(err, githost.ErrRetryable) || errors.Is(err, githost.ErrOutcomeUnknown) {
				t.Errorf("Do = %v, must not be retryable or ErrOutcomeUnknown", err)
			}
			for _, bad := range []string{"proxyuser", proxyPass, proxy.Listener.Addr().String()} {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("error %q leaks %q", err, bad)
				}
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("proxy saw %d connection(s), want 0", hits)
	}
}

// TestDefaultTransportIgnoresEnvProxy: with no Proxy configured, the default transport ignores
// HTTPS_PROXY in the process environment and opens no connection to it.
func TestDefaultTransportIgnoresEnvProxy(t *testing.T) {
	var proxyHits sync.Mutex
	var hits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Lock()
		hits++
		proxyHits.Unlock()
		http.Error(w, "unexpected", http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	proxyAddr := proxy.Listener.Addr().String()

	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("https_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	c, err := New(Config{
		Host:        apiHost,
		Credential:  tokenCred(tokenCanary),
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := defaultTransport(t, c)
	t.Cleanup(tr.CloseIdleConnections)
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == proxyAddr {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		return nil, net.ErrClosed
	}

	r := get()
	r.Path, r.Template, r.Query = "/x", "/x", nil
	_, _ = c.Do(t.Context(), r)

	proxyHits.Lock()
	defer proxyHits.Unlock()
	if hits != 0 {
		t.Fatalf("proxy saw %d connection(s) from HTTPS_PROXY, want 0", hits)
	}
}
