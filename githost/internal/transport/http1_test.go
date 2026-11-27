package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/githost"
)

// tokenCred is a credential that always returns a Bearer token with value v.
func tokenCred(v string) githost.Credential {
	return githost.CredentialFunc(func(context.Context, githost.Scope) (githost.Token, error) {
		return githost.NewToken(githost.Bearer, v, time.Time{}), nil
	})
}

// defaultClientFor builds a client with the default transport for srv, trusting only srv's certificate.
func defaultClientFor(t *testing.T, srv *httptest.Server, mod func(*Config)) *Client {
	t.Helper()
	cfg := Config{Host: srv.Listener.Addr().String(), Credential: tokenCred(tokenCanary),
		WriteCheck: func(context.Context, Send) error { return nil }}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tr := defaultTransport(t, c)
	t.Cleanup(tr.CloseIdleConnections)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr.TLSClientConfig.RootCAs = pool
	return c
}

// h2Server is a TLS server that offers HTTP/2 and records the protocol of each request.
func h2Server(t *testing.T) (*httptest.Server, *[]string, *sync.Mutex) {
	t.Helper()
	var (
		mu     sync.Mutex
		protos []string
	)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protos = append(protos, r.Proto)
		mu.Unlock()
		okBody(w)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &protos, &mu
}

// TestDefaultClientSpeaksHTTP1: against a server that offers HTTP/2, the default client negotiates
// HTTP/1.1, so no HTTP/2 resend or HTTP/2 debug log applies to a request that carries a token.
func TestDefaultClientSpeaksHTTP1(t *testing.T) {
	srv, protos, mu := h2Server(t)
	// The server does speak HTTP/2 with a client that offers it.
	resp, err := srv.Client().Get(srv.URL) //nolint:noctx // a test probe of the server
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("the server answered an HTTP/2 client with %s", resp.Proto)
	}
	c := defaultClientFor(t, srv, nil)
	for _, r := range []Request{get(), post(false)} {
		r.Path, r.Template, r.Query = "/x", "/x", nil
		if _, err := c.Do(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"HTTP/2.0", "HTTP/1.1", "HTTP/1.1"}; !reflect.DeepEqual(*protos, want) {
		t.Errorf("protocols %v, want %v", *protos, want)
	}
}

const h2HelperEnv = "ARMORY_GITHOST_H2DEBUG_HELPER"

// TestHTTP2DebugLogsNoToken: with GODEBUG=http2debug=2, which makes Go's HTTP/2 code log every header
// it encodes or decodes, no byte of the token reaches the log. GODEBUG is read when a process starts, so
// the requests run in a child process. A control request through an HTTP/2 client proves the log is on.
func TestHTTP2DebugLogsNoToken(t *testing.T) {
	if os.Getenv(h2HelperEnv) != "" {
		t.Skip("in the child process")
	}
	secret := strings.Join([]string{"h2", "dbg", "secret", "7f3a"}, "-")
	control := strings.Join([]string{"h2", "dbg", "control", "c41e"}, "-")
	// #nosec G204 G702 -- the test re-executes its own binary with fixed arguments
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHTTP2DebugHelper$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), "GODEBUG=http2debug=2", h2HelperEnv+"=1",
		"ARMORY_H2_SECRET="+secret, "ARMORY_H2_CONTROL="+control)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "helper done proto=HTTP/1.1") {
		t.Fatalf("the child process did not finish its requests over HTTP/1.1:\n%s", s)
	}
	if !strings.Contains(s, control) {
		t.Fatalf("the HTTP/2 debug log is off: the control value is missing:\n%s", s)
	}
	for _, part := range []string{secret, secret[len(secret)/2:], strings.Split(secret, "-")[2]} {
		if strings.Contains(s, part) {
			t.Errorf("the output holds %q of the token", part)
		}
	}
}

// TestHTTP2DebugHelper runs in the child process of TestHTTP2DebugLogsNoToken only.
func TestHTTP2DebugHelper(t *testing.T) {
	if os.Getenv(h2HelperEnv) == "" {
		t.Skip("runs only as a child process")
	}
	srv, _, _ := h2Server(t)
	// Control: an HTTP/2 request with a non-secret Authorization value, which the debug log prints.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("ARMORY_H2_CONTROL"))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	c := defaultClientFor(t, srv, func(cfg *Config) { cfg.Credential = tokenCred(os.Getenv("ARMORY_H2_SECRET")) })
	tr := defaultTransport(t, c)
	for _, r := range []Request{get(), post(false)} {
		r.Path, r.Template, r.Query = "/x", "/x", nil
		if _, err := c.Do(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	// Ask the server what it saw, over the same default transport, without a token.
	probe, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	presp, err := (&http.Client{Transport: tr}).Do(probe)
	if err != nil {
		t.Fatal(err)
	}
	_ = presp.Body.Close()
	fmt.Printf("helper done proto=%s\n", presp.Proto)
}

// TestReusedConnClosedBeforeBodilessWrite: the host closes a reused connection before it reads a
// bodiless write, either while the connection is idle or once the request starts to arrive. Go's
// transport may send the write again on a new connection only when not one byte of it went out, so the
// host never handles more writes than the write check allowed.
func TestReusedConnClosedBeforeBodilessWrite(t *testing.T) {
	for _, when := range []http.ConnState{http.StateIdle, http.StateActive} {
		for _, idem := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s idempotent=%v", when, idem), func(t *testing.T) {
				for range 10 {
					reusedCloseOnce(t, when, idem)
				}
			})
		}
	}
}

func reusedCloseOnce(t *testing.T, when http.ConnState, idem bool) {
	t.Helper()
	var (
		writes atomic.Int32
		armed  atomic.Bool
		mu     sync.Mutex
		active = map[net.Conn]int{}
	)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		okBody(w)
	}))
	srv.Config.ConnState = func(conn net.Conn, s http.ConnState) {
		mu.Lock()
		if s == http.StateActive {
			active[conn]++
		}
		n := active[conn]
		mu.Unlock()
		switch {
		case when == http.StateIdle && s == http.StateIdle && armed.CompareAndSwap(true, false):
			_ = conn.Close()
		case when == http.StateActive && s == http.StateActive && n == 2 && armed.CompareAndSwap(true, false):
			_ = conn.Close()
		}
	}
	srv.StartTLS()
	defer srv.Close()
	var checks atomic.Int32
	c := defaultClientFor(t, srv, func(cfg *Config) {
		cfg.WriteCheck = func(context.Context, Send) error { checks.Add(1); return nil }
		cfg.BaseDelay = time.Millisecond
	})
	r := get()
	r.Path, r.Template, r.Query = "/x", "/x", nil
	if when == http.StateIdle {
		armed.Store(true)
	}
	if _, err := c.Do(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	w := post(idem)
	w.Path, w.Template, w.Query, w.Body = "/x", "/x", nil, nil
	_, err := c.Do(t.Context(), w)
	if n, k := writes.Load(), checks.Load(); n > k || (!idem && n > 1) {
		t.Fatalf("host handled %d writes after %d write checks (Do = %v)", n, k, err)
	}
	if err != nil && !idem && !errors.Is(err, githost.ErrOutcomeUnknown) {
		t.Fatalf("Do = %v, want success or ErrOutcomeUnknown", err)
	}
}

// TestNewIgnoresProcessTransport: New takes nothing from http.DefaultTransport. A replaced one that is
// not an *http.Transport does not make New panic, and one with its own TLS dial hook, proxy or TLS
// settings does not reach the client.
func TestNewIgnoresProcessTransport(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	cfg := Config{Host: apiHost, Credential: tokenCred(tokenCanary)}

	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("process transport used")
	})
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("New panicked with a replaced DefaultTransport: %v", p)
			}
		}()
		if _, err := New(cfg); err != nil {
			t.Fatal(err)
		}
	}()

	hooked := false
	http.DefaultTransport = &http.Transport{
		DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			hooked = true
			return nil, errors.New("hook")
		},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			hooked = true
			return nil, errors.New("hook")
		},
		Proxy:              func(*http.Request) (*url.URL, error) { hooked = true; return nil, nil },
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the hook under test
		ForceAttemptHTTP2:  true,
		DisableCompression: false,
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tr := defaultTransport(t, c)
	if tr == http.DefaultTransport || tr.DialTLSContext != nil ||
		tr.DialTLS != nil || //nolint:staticcheck // the test asserts the deprecated hook is unset
		tr.ForceAttemptHTTP2 || tr.TLSClientConfig.InsecureSkipVerify {
		t.Errorf("the default transport took settings of http.DefaultTransport: %+v", tr)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { okBody(w) }))
	t.Cleanup(srv.Close)
	c2 := defaultClientFor(t, srv, nil)
	r := get()
	r.Path, r.Template, r.Query = "/x", "/x", nil
	if _, err := c2.Do(t.Context(), r); err != nil || hooked {
		t.Fatalf("Do = %v, process hook called %v", err, hooked)
	}
}

// TestNoProcessWideHTTPState: no file of githost names the process-wide HTTP client or transport, or a
// helper that sends through them.
func TestNoProcessWideHTTPState(t *testing.T) {
	banned := map[string]bool{"DefaultTransport": true, "DefaultClient": true, "Get": true, "Head": true,
		"Post": true, "PostForm": true}
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" && banned[sel.Sel.Name] {
					t.Errorf("%s uses http.%s", path, sel.Sel.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestIdleChannelUnsolicitedPeerBytesLogged establishes what Go's HTTP/1.1 client logs through the
// standard logger when a server writes extra bytes after a response on a kept-alive connection: it logs
// a prefix of the peer bytes, and nothing the client sent.
func TestIdleChannelUnsolicitedPeerBytesLogged(t *testing.T) {
	var logs syncBuffer
	origOut := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(origOut) })

	const peerNoise = "PEER-UNSOLICITED-DATA-7c21"
	sentToken := strings.Join([]string{"idle", "chan", "tok", "9f4b"}, "-")
	sendExtra := make(chan struct{})
	extraDone := make(chan struct{})

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() {
			_ = conn.Close()
			close(extraDone)
		}()
		const payload = `{"idle":true}`
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(payload), payload)
		_ = rw.Flush()
		<-sendExtra
		_, _ = io.WriteString(rw, peerNoise+"\r\n")
		_ = rw.Flush()
	}))
	t.Cleanup(srv.Close)

	c := defaultClientFor(t, srv, func(cfg *Config) { cfg.Credential = tokenCred(sentToken) })
	tr := defaultTransport(t, c)
	r := get()
	r.Path, r.Template, r.Query = "/x", "/x", nil
	resp, err := c.Do(t.Context(), r)
	if err != nil || string(resp.Body) != `{"idle":true}` {
		close(sendExtra)
		t.Fatalf("Do = %q, %v", resp.Body, err)
	}
	close(sendExtra)
	<-extraDone

	deadline := time.Now().Add(2 * time.Second)
	var logged string
	for time.Now().Before(deadline) {
		logged = logs.String()
		if strings.Contains(logged, "Unsolicited response received on idle HTTP channel") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tr.CloseIdleConnections()
	if !strings.Contains(logged, "Unsolicited response received on idle HTTP channel") {
		t.Fatalf("standard logger did not record the idle-channel message; got %q", logged)
	}
	if !strings.Contains(logged, peerNoise) {
		t.Errorf("standard logger did not include the peer bytes; got %q", logged)
	}
	if strings.Contains(logged, sentToken) {
		t.Errorf("standard logger contains the client token: %q", logged)
	}
	t.Logf("captured idle-channel log: %q", strings.TrimSpace(logged))
}
