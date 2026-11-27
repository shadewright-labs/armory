package transport

import (
	"context"
	"time"
)

// wireContext is the context of a request on the wire: it ends when its parent context (the attempt's
// context) ends, with the same error and deadline, and returns no value.
//
// A client trace (net/http/httptrace) and a dial trace live in context values that net/http and net
// read from the request's context. httptrace.WithClientTrace composes a new trace with the one already
// in the context, so the caller's hooks would still fire; and a context derived with context.WithValue
// or context.WithCancel passes every value of its parent on. A context whose Value returns nil for
// every key is the only one under which no caller hook can see the request, its Authorization header
// included.
//
// It has an AfterFunc method, so contexts that net/http derives from it register with the parent
// context instead of starting a goroutine each.
type wireContext struct {
	call context.Context
}

func (w wireContext) Deadline() (time.Time, bool)           { return w.call.Deadline() }
func (w wireContext) Done() <-chan struct{}                 { return w.call.Done() }
func (w wireContext) Err() error                            { return w.call.Err() }
func (wireContext) Value(any) any                           { return nil }
func (w wireContext) AfterFunc(f func()) (stop func() bool) { return context.AfterFunc(w.call, f) }
