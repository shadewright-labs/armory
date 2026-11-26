package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
// for no compression, has no TLS dial hook, and takes its proxy from the environment.
func TestDefaultTransport(t *testing.T) {
	c, err := New(Config{Host: apiHost, Credential: githost.CredentialFunc(
		func(context.Context, githost.Scope) (githost.Token, error) { return githost.Token{}, nil })})
	if err != nil {
		t.Fatal(err)
	}
	if c.attemptMax != DefaultAttemptTimeout || c.httpClient.CheckRedirect == nil {
		t.Errorf("attempt timeout %s, redirect policy set %v", c.attemptMax, c.httpClient.CheckRedirect != nil)
	}
	if p := c.httpClient.Transport.(pinned); p.host != apiHost {
		t.Errorf("pinned host %q", p.host)
	}
	tr := defaultTransport(t, c)
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
	if dialer.Timeout != 10*time.Second || dialer.KeepAlive != 30*time.Second || tr.DialContext == nil {
		t.Errorf("dialer %+v, dial func set %v", dialer, tr.DialContext != nil)
	}
	if tr.ForceAttemptHTTP2 || tr.TLSNextProto == nil || len(tr.TLSNextProto) != 0 ||
		!reflect.DeepEqual(tr.TLSClientConfig.NextProtos, []string{"http/1.1"}) ||
		tr.Protocols == nil || tr.Protocols.HTTP2() || tr.Protocols.UnencryptedHTTP2() || !tr.Protocols.HTTP1() {
		t.Errorf("HTTP/2 is not off: force %v, next protos %v %v, protocols %v", tr.ForceAttemptHTTP2,
			tr.TLSNextProto, tr.TLSClientConfig.NextProtos, tr.Protocols)
	}
	if !tr.DisableCompression || tr.DialTLSContext != nil || tr.DialTLS != nil || dialer.Resolver == nil ||
		dialer.Resolver == net.DefaultResolver {
		t.Errorf("compression off %v, TLS dial hook set %v, private resolver %v", tr.DisableCompression,
			tr.DialTLSContext != nil || tr.DialTLS != nil, dialer.Resolver != nil && dialer.Resolver != net.DefaultResolver)
	}
	if tr.Proxy == nil || reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Error("proxy is not taken from the environment")
	}
}

// TestProxyNeverSeesTheToken: through a proxy, an https request goes in a CONNECT tunnel. The proxy sees
// the host and port, and Proxy-Authorization when its URL has a user, but never the Authorization header:
// that travels inside TLS to the host.
func TestProxyNeverSeesTheToken(t *testing.T) {
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

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	t.Cleanup(proxy.Close)

	c, err := New(Config{Host: host, Credential: githost.CredentialFunc(
		func(context.Context, githost.Scope) (githost.Token, error) {
			return githost.NewToken(githost.Bearer, tokenCanary, time.Time{}), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	tr := defaultTransport(t, c)
	t.Cleanup(tr.CloseIdleConnections)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr.TLSClientConfig.RootCAs = pool
	// The environment's proxy settings skip loopback, so the test points the transport at its proxy.
	pu, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	pu.User = url.UserPassword("proxyuser", strings.ToLower(pathCanary))
	tr.Proxy = http.ProxyURL(pu)

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
	if h.Get("Proxy-Authorization") == "" {
		t.Error("an authenticated proxy got no Proxy-Authorization")
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
}
