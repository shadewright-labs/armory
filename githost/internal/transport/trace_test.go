package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/githost"
)

// hookRecorder is a ClientTrace whose every hook, whatever Go adds later, records that it fired and the
// arguments it saw.
type hookRecorder struct {
	trace *httptrace.ClientTrace
	mu    sync.Mutex
	calls []string
}

func newHookRecorder() *hookRecorder {
	h := &hookRecorder{trace: &httptrace.ClientTrace{}}
	v := reflect.ValueOf(h.trace).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		if f.Kind() != reflect.Func {
			continue
		}
		name := v.Type().Field(i).Name
		ft := f.Type()
		f.Set(reflect.MakeFunc(ft, func(args []reflect.Value) []reflect.Value {
			parts := make([]string, 0, len(args))
			for _, a := range args {
				parts = append(parts, fmt.Sprint(a.Interface()))
			}
			h.mu.Lock()
			h.calls = append(h.calls, name+"("+strings.Join(parts, ", ")+")")
			h.mu.Unlock()
			out := make([]reflect.Value, ft.NumOut())
			for j := range out {
				out[j] = reflect.Zero(ft.Out(j))
			}
			return out
		}))
	}
	return h
}

func (h *hookRecorder) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// TestHookRecorderSeesTheToken: the recorder is not vacuous. A plain request with the same trace fires
// hooks, and WroteHeaderField shows the Authorization value.
func TestHookRecorderSeesTheToken(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
	rec := newHookRecorder()
	ctx := httptrace.WithClientTrace(t.Context(), rec.trace)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.srv.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenCanary)
	resp, err := s.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(strings.Join(rec.seen(), "\n"), tokenCanary) {
		t.Fatalf("the recorder did not see the token: %q", rec.seen())
	}
}

// TestCallerTraceNeverSeesTheWire: a client trace in the caller's context gets no callback for any
// request the client sends: not on the first send, not on a retry, not on a reused connection, not on
// a write. The credential and the write check still get the caller's context, values included.
func TestCallerTraceNeverSeesTheWire(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		okBody(w)
	})
	type key struct{}
	rec := newHookRecorder()
	ctx := httptrace.WithClientTrace(context.WithValue(t.Context(), key{}, "v"), rec.trace)
	var credCtx, checkCtx []bool
	h := newHarness(t, s, func(cfg *Config) {
		cfg.Credential = githost.CredentialFunc(func(c context.Context, _ githost.Scope) (githost.Token, error) {
			credCtx = append(credCtx, httptrace.ContextClientTrace(c) != nil && c.Value(key{}) == "v")
			return githost.NewToken(githost.Bearer, tokenCanary, time.Time{}), nil
		})
		cfg.WriteCheck = func(c context.Context, _ Send) error {
			checkCtx = append(checkCtx, httptrace.ContextClientTrace(c) != nil && c.Value(key{}) == "v")
			return nil
		}
	})
	// A retry (502, then 200), a reused connection, and a write.
	for _, r := range []Request{get(), get(), post(true)} {
		if _, err := h.c.Do(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.hits.Load(); n != 4 {
		t.Fatalf("server got %d requests, want 4", n)
	}
	if calls := rec.seen(); len(calls) != 0 {
		t.Errorf("the caller's trace fired %d hooks: %q", len(calls), calls)
	}
	if !reflect.DeepEqual(credCtx, []bool{true, true, true, true}) || !reflect.DeepEqual(checkCtx, []bool{true}) {
		t.Errorf("credential saw the caller's context %v, write check %v", credCtx, checkCtx)
	}
}

// TestWireContextFollowsTheCaller: the context of the wire request ends when the caller's does, and
// carries its deadline, but no value.
func TestWireContextFollowsTheCaller(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithDeadline(context.WithValue(t.Context(), key{}, "v"), time.Now().Add(time.Hour))
	w := wireContext{parent}
	if w.Value(key{}) != nil || httptrace.ContextClientTrace(w) != nil {
		t.Error("the wire context carries a value")
	}
	if dl, ok := w.Deadline(); !ok || !dl.Equal(mustDeadline(t, parent)) {
		t.Errorf("deadline %v, %v", dl, ok)
	}
	child, stop := context.WithCancel(w)
	defer stop()
	cancel()
	select {
	case <-child.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a context derived from the wire context did not end with the caller's")
	}
	if !errors.Is(w.Err(), context.Canceled) || !errors.Is(child.Err(), context.Canceled) {
		t.Errorf("Err = %v, %v", w.Err(), child.Err())
	}
}

func mustDeadline(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("no deadline")
	}
	return dl
}

// TestCancelEndsRequestInFlight: a request waiting for its answer ends soon after the caller cancels,
// and soon after the caller's deadline.
func TestCancelEndsRequestInFlight(t *testing.T) {
	for _, byDeadline := range []bool{false, true} {
		t.Run(fmt.Sprint("deadline=", byDeadline), func(t *testing.T) {
			s := newServer(t, func(_ http.ResponseWriter, r *http.Request, _ int) {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			})
			h := newHarness(t, s, func(c *Config) { c.MaxAttempts = 1 })
			var ctx context.Context
			var cancel context.CancelFunc
			if byDeadline {
				ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(t.Context())
				time.AfterFunc(100*time.Millisecond, cancel)
			}
			defer cancel()
			start := time.Now()
			_, err := h.c.Do(ctx, get())
			want := context.Canceled
			if byDeadline {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatalf("Do = %v, want %v", err, want)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("Do took %s", d)
			}
		})
	}
}
