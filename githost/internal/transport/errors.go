package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/shadewright-labs/armory/githost"
)

// kindsError is an error of fixed text that matches several kinds. Each kind is a sentinel of package
// githost, a context error, or an error this package built from those; never an error of a callback.
type kindsError struct {
	msg   string
	kinds []error
}

func (e *kindsError) Error() string   { return e.msg }
func (e *kindsError) Unwrap() []error { return e.kinds }

// fixed is an error of fixed text with no kind and no wrapped error.
func fixed(op, msg string) error { return errors.New("githost: " + op + ": " + msg) }

// finalError is the error a call ends with. Once an attempt of the write may have reached the host, it
// says the outcome is unknown, next to its own cause.
func finalError(op string, err error, mayHaveWritten bool) error {
	if !mayHaveWritten || errors.Is(err, githost.ErrOutcomeUnknown) {
		return err
	}
	return outcomeUnknown(op, err)
}

// outcomeUnknown matches githost.ErrOutcomeUnknown and cause, but never githost.ErrRetryable: the
// caller reads state before it sends the write again. cause is an error of this package.
func outcomeUnknown(op string, cause error) error {
	if re, ok := errors.AsType[*githost.RetryableError](cause); ok {
		cause = re.Err
	}
	return &kindsError{
		msg:   "githost: " + op + ": " + githost.ErrOutcomeUnknown.Error() + "; last failure: " + cause.Error(),
		kinds: []error{githost.ErrOutcomeUnknown, cause},
	}
}

// receiptError is a 2xx answer to a write whose body could not be used. It matches
// githost.ErrUnusableReceipt and keeps the class of cause when it is the bound or the end of the
// context; it is never retryable.
func receiptError(ctx context.Context, op string, status int, cause error) error {
	head := &githost.Error{Op: op, Status: status, Kind: githost.ErrUnusableReceipt}
	kinds := []error{head}
	switch {
	case errors.Is(cause, githost.ErrTooLarge):
		kinds = append(kinds, githost.ErrTooLarge)
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		kinds = append(kinds, context.DeadlineExceeded)
	case ctx.Err() != nil:
		kinds = append(kinds, context.Canceled)
	}
	msg := head.Error()
	if len(kinds) > 1 {
		msg += ": " + kinds[1].Error()
	}
	return &kindsError{msg: msg, kinds: kinds}
}

// refusedError is a refusal of the write check. Only the class of a valid *githost.Refusal in its
// chain is kept, copied into a new Refusal; the check's text and identity are dropped.
func refusedError(op string, err error) error {
	stopped := &githost.Error{Op: op, Kind: githost.ErrStoppedBeforeWrite}
	var ref *githost.Refusal
	if !errors.As(err, &ref) || ref == nil {
		return stopped
	}
	kept := &githost.Refusal{Class: string([]byte(ref.Class))}
	if !kept.Valid() {
		return stopped
	}
	return &kindsError{msg: stopped.Error() + ": " + kept.Error(), kinds: []error{stopped, kept}}
}

// transportError replaces an error of the HTTP client, which may hold the URL or answer text, with fixed
// text and its class.
func (c *Client) transportError(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return ctxError(op, ctx)
	}
	if errors.Is(err, errOffOrigin) {
		return &githost.Error{Op: op, Kind: githost.ErrRejected}
	}
	if errors.Is(err, errCleartextProxyAuth) {
		head := &githost.Error{Op: op, Kind: githost.ErrRejected}
		return &kindsError{
			msg:   head.Error() + ": proxy credentials require an https proxy",
			kinds: []error{head},
		}
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return &githost.RetryableError{Err: fixed(op, "request timed out")}
	}
	return &githost.RetryableError{Err: fixed(op, "request failed (network)")}
}

// ctxError reports the end of the call's context. A deadline is retryable by the caller; a cancellation
// is not.
func ctxError(op string, ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &githost.RetryableError{Err: fmt.Errorf("githost: %s: %w", op, context.DeadlineExceeded)}
	}
	return fmt.Errorf("githost: %s: %w", op, context.Canceled)
}

// credentialError replaces an error of the Credential, which may hold any text, with fixed text that
// keeps only its class.
func credentialError(op string, err error) error {
	const msg = "credential unavailable"
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("githost: %s: %s: %w", op, msg, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return &githost.RetryableError{Err: fmt.Errorf("githost: %s: %s: %w", op, msg, context.DeadlineExceeded)}
	case errors.Is(err, githost.ErrRetryable):
		// The same cap as a delay the host asks for; backoff applies the caller's deadline to both.
		after, _ := askedDelay(err)
		return &githost.RetryableError{RetryAfter: min(after, maxDelay), Err: fixed(op, msg)}
	}
	for _, kind := range []error{githost.ErrNotFound, githost.ErrForbidden, githost.ErrUnauthorized} {
		if errors.Is(err, kind) {
			return fmt.Errorf("githost: %s: %s: %w", op, msg, kind)
		}
	}
	return fixed(op, msg)
}

// StatusError classifies a status the host client did not handle itself. An op that is not a valid
// label is replaced with a fixed one.
func StatusError(op string, status int) error {
	if !validOp(op) {
		op = opDo
	}
	var kind error
	switch status {
	case http.StatusNotFound, http.StatusGone:
		kind = githost.ErrNotFound
	case http.StatusConflict:
		kind = githost.ErrConflict
	case http.StatusForbidden:
		kind = githost.ErrForbidden
	case http.StatusUnauthorized:
		kind = githost.ErrUnauthorized
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		kind = githost.ErrRejected
	}
	return &githost.Error{Op: op, Status: status, Kind: kind}
}

// maxOp bounds the length of an operation label.
const maxOp = 64

// validOp reports whether op is a label errors may print: 1 to 64 ASCII letters, digits, ".", "_" and
// "-".
func validOp(op string) bool {
	return op != "" && len(op) <= maxOp &&
		allIn(op, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-")
}

// validFieldName reports whether k is an HTTP field name (RFC 9110, section 5.1): one or more token
// characters.
func validFieldName(k string) bool {
	return k != "" && allIn(k, "!#$%&'*+-.^_`|~0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

// validFieldValue reports whether v is a field value Go sends: no control character other than a tab,
// and no DEL. It is the rule of net/http's own check, so a value that passes is not refused below Do.
func validFieldValue(v string) bool {
	for i := range len(v) {
		if b := v[i]; (b < ' ' && b != '\t') || b == 0x7f {
			return false
		}
	}
	return true
}

// validHeader reports whether every name and value of h is one Go sends.
func validHeader(h http.Header) bool {
	for k, vs := range h {
		if !validFieldName(k) {
			return false
		}
		for _, v := range vs {
			if !validFieldValue(v) {
				return false
			}
		}
	}
	return true
}

// validIfNoneMatch reports whether s is a syntactically valid RFC 9110 If-None-Match value: "*" or a
// comma-separated list of one or more optionally weak quoted entity-tags.
func validIfNoneMatch(s string) bool {
	s = trimOWS(s)
	if s == "" {
		return false
	}
	if s == "*" {
		return true
	}
	i := 0
	for {
		i = skipOWS(s, i)
		var ok bool
		i, ok = parseEntityTag(s, i)
		if !ok {
			return false
		}
		i = skipOWS(s, i)
		if i == len(s) {
			return true
		}
		if s[i] != ',' {
			return false
		}
		i++
	}
}

func parseEntityTag(s string, i int) (int, bool) {
	if strings.HasPrefix(s[i:], "W/") {
		i += 2
	}
	if i >= len(s) || s[i] != '"' {
		return 0, false
	}
	i++
	for i < len(s) && s[i] != '"' {
		b := s[i]
		// RFC 9110 section 8.8.3: etagc = %x21 / %x23-7E / obs-text (%x80-FF).
		if b < 0x21 || b == 0x7f {
			return 0, false
		}
		i++
	}
	if i >= len(s) || s[i] != '"' {
		return 0, false
	}
	return i + 1, true
}

func skipOWS(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

func trimOWS(s string) string {
	start := skipOWS(s, 0)
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// validIfModifiedSince reports whether s is an HTTP-date accepted by http.ParseTime.
func validIfModifiedSince(s string) bool {
	if s == "" {
		return false
	}
	_, err := http.ParseTime(s)
	return err == nil
}

// hasValidCondition reports whether h carries a usable If-None-Match or If-Modified-Since validator.
func hasValidCondition(h http.Header) bool {
	if inm := h.Values("If-None-Match"); len(inm) > 0 {
		valid := true
		for _, v := range inm {
			if !validIfNoneMatch(v) {
				valid = false
				break
			}
		}
		if valid {
			return true
		}
	}
	if ims := h.Values("If-Modified-Since"); len(ims) == 1 && validIfModifiedSince(ims[0]) {
		return true
	}
	return false
}
