package netguard

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeDNS answers DNS queries over an in-memory stream connection, with messages built from the standard
// library only. answer returns the addresses for the nth query of a type (n counts from 1).
type fakeDNS struct {
	queriesA atomic.Int32
	answer   func(qtype uint16, n int32) []netip.Addr
}

const (
	typeA    = 1
	typeAAAA = 28
)

func (f *fakeDNS) resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go f.serve(server)
			return client, nil
		},
	}
}

// serve reads length-prefixed DNS queries (the stream form, since a pipe is not a PacketConn) and writes
// one answer for each.
func (f *fakeDNS) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	for {
		var l [2]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		resp := f.respond(q)
		if resp == nil {
			return
		}
		out := binary.BigEndian.AppendUint16(nil, uint16(len(resp))) //nolint:gosec // a test answer is short
		if _, err := c.Write(append(out, resp...)); err != nil {
			return
		}
	}
}

func (f *fakeDNS) respond(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	// Skip the question name: labels up to the zero byte.
	i := 12
	for i < len(q) && q[i] != 0 {
		i += int(q[i]) + 1
	}
	if i+5 > len(q) {
		return nil
	}
	question := q[12 : i+5]
	qtype := binary.BigEndian.Uint16(q[i+1:])
	var n int32
	if qtype == typeA {
		n = f.queriesA.Add(1)
	}
	var addrs []netip.Addr
	for _, a := range f.answer(qtype, n) {
		if (qtype == typeA && a.Is4()) || (qtype == typeAAAA && a.Is6()) {
			addrs = append(addrs, a)
		}
	}
	resp := make([]byte, 12, 12+len(question)+len(addrs)*28)
	copy(resp, q[:2])                                        // ID
	binary.BigEndian.PutUint16(resp[2:], 0x8180)             // response, recursion desired and available
	binary.BigEndian.PutUint16(resp[4:], 1)                  // one question
	binary.BigEndian.PutUint16(resp[6:], uint16(len(addrs))) //nolint:gosec // at most two answers
	resp = append(resp, question...)
	for _, a := range addrs {
		resp = append(resp, 0xc0, 12) // name: pointer to the question
		resp = binary.BigEndian.AppendUint16(resp, qtype)
		resp = binary.BigEndian.AppendUint16(resp, 1) // class IN
		resp = binary.BigEndian.AppendUint32(resp, 0) // TTL
		b := a.AsSlice()
		resp = binary.BigEndian.AppendUint16(resp, uint16(len(b))) //nolint:gosec // 4 or 16
		resp = append(resp, b...)
	}
	return resp
}

// loopbackListener counts the connections a listener on 127.0.0.1 accepts.
type loopbackListener struct {
	ln       net.Listener
	accepted atomic.Int32
	wg       sync.WaitGroup
}

func newLoopbackListener(t *testing.T) *loopbackListener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), netTCP4, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &loopbackListener{ln: ln}
	l.wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.accepted.Add(1)
			_ = c.Close()
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		l.wg.Wait()
	})
	return l
}

func (l *loopbackListener) port() string {
	return strconv.Itoa(l.ln.Addr().(*net.TCPAddr).Port)
}

// controlCall records one call of the Control hook.
type controlCall struct {
	address string
	err     error
}

var errStop = errors.New("test: stop before connect")

// recordingDialer returns a dialer whose Control runs p.Control, records the result, and stops an allowed
// connection with errStop, so no packet leaves for a public address.
func recordingDialer(p Policy, r *net.Resolver, calls *[]controlCall, mu *sync.Mutex) *net.Dialer {
	return &net.Dialer{
		Resolver:      r,
		Timeout:       5 * time.Second,
		FallbackDelay: -1, // try every address in turn
		Control: func(network, address string, c syscall.RawConn) error {
			err := p.Control(network, address, c)
			mu.Lock()
			*calls = append(*calls, controlCall{address, err})
			mu.Unlock()
			if err == nil {
				return errStop
			}
			return err
		},
	}
}

func TestControlRefusesName(t *testing.T) {
	l := newLoopbackListener(t)
	dns := &fakeDNS{answer: func(uint16, int32) []netip.Addr {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	}}
	d := &net.Dialer{Resolver: dns.resolver(), Control: Policy{}.Control, Timeout: 5 * time.Second}
	c, err := d.DialContext(t.Context(), netTCP, "loop.example.:"+l.port())
	if err == nil {
		_ = c.Close()
		t.Fatal("dial to a name that resolves to 127.0.0.1 succeeded")
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("dial error %v does not match ErrRefused", err)
	}
	// The literal too, and with a ControlContext dialer.
	d2 := &net.Dialer{ControlContext: Policy{}.ControlContext, Timeout: 5 * time.Second}
	if c, err := d2.DialContext(t.Context(), netTCP, "127.0.0.1:"+l.port()); !errors.Is(err, ErrRefused) {
		if c != nil {
			_ = c.Close()
		}
		t.Fatalf("dial to 127.0.0.1 = %v, want ErrRefused", err)
	}
	if n := l.accepted.Load(); n != 0 {
		t.Fatalf("loopback listener accepted %d connections", n)
	}
}

func TestControlRebinding(t *testing.T) {
	l := newLoopbackListener(t)
	dns := &fakeDNS{answer: func(qtype uint16, n int32) []netip.Addr {
		if qtype != typeA {
			return nil
		}
		if n == 1 {
			return []netip.Addr{netip.MustParseAddr(pub4)}
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	}}
	var (
		calls []controlCall
		mu    sync.Mutex
	)
	d := recordingDialer(Policy{}, dns.resolver(), &calls, &mu)
	target := "rebind.example.:" + l.port()

	_, err := d.DialContext(t.Context(), netTCP, target)
	if !errors.Is(err, errStop) {
		t.Fatalf("first dial = %v, want the test stop", err)
	}
	_, err = d.DialContext(t.Context(), netTCP, target)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("second dial = %v, want ErrRefused", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("Control ran %d times, want 2: %+v", len(calls), calls)
	}
	if calls[0].address != "8.8.8.8:"+l.port() || calls[0].err != nil {
		t.Errorf("first call = %+v, want 8.8.8.8 allowed", calls[0])
	}
	if calls[1].address != "127.0.0.1:"+l.port() || !errors.Is(calls[1].err, ErrRefused) {
		t.Errorf("second call = %+v, want 127.0.0.1 refused", calls[1])
	}
	if n := l.accepted.Load(); n != 0 {
		t.Fatalf("loopback listener accepted %d connections", n)
	}
}

func TestControlMixedAnswer(t *testing.T) {
	ln6, err := (&net.ListenConfig{}).Listen(t.Context(), netTCP6, "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback on this host")
	}
	port := strconv.Itoa(ln6.Addr().(*net.TCPAddr).Port)
	var accepted atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln6.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	dns := &fakeDNS{answer: func(uint16, int32) []netip.Addr {
		return []netip.Addr{netip.MustParseAddr(pub4), netip.MustParseAddr(loop6)}
	}}
	var (
		calls []controlCall
		mu    sync.Mutex
	)
	d := recordingDialer(Policy{}, dns.resolver(), &calls, &mu)
	if c, err := d.DialContext(t.Context(), netTCP, "mixed.example.:"+port); err == nil {
		_ = c.Close()
		t.Fatal("dial succeeded")
	}
	_ = ln6.Close()
	<-done
	mu.Lock()
	defer mu.Unlock()
	var saw4, saw6 bool
	for _, c := range calls {
		switch c.address {
		case "8.8.8.8:" + port:
			saw4 = c.err == nil
		case "[::1]:" + port:
			saw6 = errors.Is(c.err, ErrRefused)
		default:
			t.Errorf("unexpected Control call %+v", c)
		}
	}
	if !saw4 || !saw6 {
		t.Fatalf("Control calls %+v: want 8.8.8.8 allowed and ::1 refused", calls)
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("IPv6 loopback listener accepted %d connections", n)
	}
}

func TestControlInputs(t *testing.T) {
	p := Policy{}
	allowed := []struct{ network, address string }{
		{netTCP, pub4HTTPS},
		{netTCP4, "8.8.8.8:1"},
		{netTCP4, "8.8.8.8:65535"},
		{netTCP6, "[2001:4860:4860::8888]:443"},
		{netTCP6, "[64:ff9b::808:808]:443"},
		{netTCP6, "[::ffff:8.8.8.8]:443"},
	}
	for _, tc := range allowed {
		if err := p.Control(tc.network, tc.address, nil); err != nil {
			t.Errorf("Control(%q, %q) = %v, want nil", tc.network, tc.address, err)
		}
	}
	malformed := []struct{ network, address string }{
		{netTCP, nameHTTPS},
		{netTCP, "localhost:443"},
		{"unix", pub4HTTPS},
		{"ip4:icmp", pub4},
		{netUDP, pub4DNS},
		{"udp4", pub4DNS},
		{"TCP", pub4HTTPS},
		{"", pub4HTTPS},
		{netTCP, pub4},
		{netTCP, "8.8.8.8:"},
		{netTCP, "8.8.8.8:0"},
		{netTCP, "8.8.8.8:+443"},
		{netTCP, "8.8.8.8:-1"},
		{netTCP, "8.8.8.8:https"},
		{netTCP, "8.8.8.8:0443"},
		{netTCP, "8.8.8.8:65536"},
		{netTCP, "8.8.8.8:99999"},
		{netTCP, "8.8.8.8:443 "},
		{netTCP4, "[2001:4860:4860::8888]:443"},
		{netTCP4, "[::ffff:8.8.8.8]:443"},
		{netTCP6, pub4HTTPS},
		{netTCP, "2001:4860:4860::8888:443"},
		{netTCP, "[8.8.8.8]:443"},
		{netTCP, "[2001:4860::1:443"},
		{netTCP, "[2001:4860::1%1]:443"},
		{netTCP, "[fe80::1%25eth0]:443"},
		{netTCP, "008.8.8.8:443"},
		{netTCP, "0x08080808:443"},
		{netTCP, "[" + strings.Repeat("0", 70) + "::1]:443"},
	}
	for _, tc := range malformed {
		err := p.Control(tc.network, tc.address, nil)
		var r *Refusal
		if !errors.As(err, &r) || !errors.Is(err, ErrRefused) || !errors.Is(err, ErrInvalidAddr) || r.Class != Invalid {
			t.Errorf("Control(%q, %q) = %v, want a malformed-address refusal", tc.network, tc.address, err)
		}
	}
	over := "[" + strings.Repeat("0", MaxControlAddrLen) + ":2001:4860::1]:443"
	if err := p.Control(netTCP6, over, nil); !errors.Is(err, ErrInvalidAddr) {
		t.Errorf("Control over 80 bytes = %v, want ErrInvalidAddr", err)
	}
	refused := []struct{ network, address string }{
		{netTCP4, "127.0.0.1:443"},
		{netTCP4, "10.0.0.1:443"},
		{netTCP6, loop6HTTPS},
		{netTCP6, "[fe80::1]:443"},
		{netTCP6, "[3fff::1]:443"},
	}
	for _, tc := range refused {
		err := p.Control(tc.network, tc.address, nil)
		if !errors.Is(err, ErrRefused) || errors.Is(err, ErrInvalidAddr) {
			t.Errorf("Control(%q, %q) = %v, want a refusal of a well-formed address", tc.network, tc.address, err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.ControlContext(ctx, netTCP4, pub4HTTPS, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("ControlContext with a done context = %v, want context.Canceled", err)
	}
	if err := p.ControlContext(t.Context(), netTCP4, pub4HTTPS, nil); err != nil {
		t.Errorf("ControlContext = %v", err)
	}
	if err := p.ControlContext(t.Context(), netTCP4, "127.0.0.1:443", nil); !errors.Is(err, ErrRefused) {
		t.Errorf("ControlContext(127.0.0.1) = %v", err)
	}
}
