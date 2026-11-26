package modelusage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// Family is a provider reply format.
type Family int

const (
	// Anthropic is the Anthropic Messages API (POST /v1/messages).
	Anthropic Family = iota + 1
	// OpenAIChat is the OpenAI chat completions API (POST /v1/chat/completions) and servers that copy it.
	OpenAIChat
	// OpenAIResponses is the OpenAI Responses API (POST /v1/responses).
	OpenAIResponses
)

// String returns the family's name.
func (f Family) String() string {
	switch f {
	case Anthropic:
		return "anthropic"
	case OpenAIChat:
		return "openai-chat"
	case OpenAIResponses:
		return "openai-responses"
	}
	return "unknown"
}

// Usage is the token usage of one reply, with each count as the provider reported it. The meaning of
// InputTokens differs by family, so totals come from the methods, which apply the rule of the family:
//
//   - Anthropic: input_tokens counts only the input after the last cache breakpoint. The cache writes
//     (cache_creation_input_tokens) and cache reads (cache_read_input_tokens) are reported apart and are
//     not part of it, so the whole prompt is InputTokens + CacheCreationInputTokens + CacheReadInputTokens.
//     The cache writes by lifetime (cache_creation.ephemeral_5m_input_tokens and ephemeral_1h_input_tokens)
//     are parts of CacheCreationInputTokens. OutputTokens counts every output token, thinking included, and
//     ReasoningTokens (output_tokens_details.thinking_tokens) is a part of it. CachedPromptTokens,
//     CacheWritePromptTokens and the OpenAI lifetimes stay zero.
//   - OpenAIChat and OpenAIResponses: prompt_tokens (chat) or input_tokens (responses) is the whole prompt.
//     Its details, cached_tokens (cache reads) and cache_write_tokens (cache writes), are parts of it, kept
//     in CachedPromptTokens and CacheWritePromptTokens and never added again. The Responses API also splits
//     the cache writes by lifetime (cache_write_tokens_details.ttl_30m_tokens and ttl_12h_tokens), parts of
//     CacheWritePromptTokens. completion_tokens or output_tokens is all output, and ReasoningTokens
//     (reasoning_tokens) is a part of it. CacheCreationInputTokens, CacheReadInputTokens and the Anthropic
//     lifetimes stay zero.
//
// A breakdown is a part of its parent count and never adds to a total. A provider may leave part of a
// parent without a breakdown (an older model, or a field it does not send): the lifetimes of the cache
// writes then sum to less than the writes, and the rest has no known lifetime. A breakdown that sums to
// more than its parent is refused as ErrOutOfRange rather than accepted as reported, because both
// references define it as a part of the parent, and a price per lifetime taken from it would charge more
// than the parent the provider bills.
//
// A parser never returns a Usage that breaks these rules: the parts are never larger than their whole, and
// every count and total is within the caller's [Limits.MaxTokens].
type Usage struct {
	// Family is the reply format the counts were read from.
	Family Family
	// InputTokens is input_tokens (Anthropic and Responses) or prompt_tokens (chat completions).
	InputTokens int64
	// OutputTokens is output_tokens (Anthropic and Responses) or completion_tokens (chat completions).
	OutputTokens int64
	// CacheCreationInputTokens is Anthropic's cache_creation_input_tokens, outside InputTokens.
	CacheCreationInputTokens int64
	// CacheCreation5mInputTokens is Anthropic's cache_creation.ephemeral_5m_input_tokens, the cache writes
	// with a five-minute lifetime, a part of CacheCreationInputTokens.
	CacheCreation5mInputTokens int64
	// CacheCreation1hInputTokens is Anthropic's cache_creation.ephemeral_1h_input_tokens, the cache writes
	// with a one-hour lifetime, a part of CacheCreationInputTokens.
	CacheCreation1hInputTokens int64
	// CacheReadInputTokens is Anthropic's cache_read_input_tokens, outside InputTokens.
	CacheReadInputTokens int64
	// CachedPromptTokens is OpenAI's cached_tokens (prompt_tokens_details or input_tokens_details), a part
	// of InputTokens.
	CachedPromptTokens int64
	// CacheWritePromptTokens is OpenAI's cache_write_tokens, a part of InputTokens, when reported.
	CacheWritePromptTokens int64
	// CacheWrite30mPromptTokens is the Responses API's input_tokens_details.cache_write_tokens_details
	// .ttl_30m_tokens, the cache writes with a 30-minute lifetime, a part of CacheWritePromptTokens.
	CacheWrite30mPromptTokens int64
	// CacheWrite12hPromptTokens is the Responses API's input_tokens_details.cache_write_tokens_details
	// .ttl_12h_tokens, the cache writes with a 12-hour lifetime, a part of CacheWritePromptTokens.
	CacheWrite12hPromptTokens int64
	// ReasoningTokens is OpenAI's reasoning_tokens (completion_tokens_details or output_tokens_details) or
	// Anthropic's output_tokens_details.thinking_tokens, a part of OutputTokens, when reported.
	ReasoningTokens int64
}

// TotalInputTokens is the whole prompt: every input token the provider counted, cached or not.
func (u Usage) TotalInputTokens() int64 {
	if u.Family == Anthropic {
		return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	}
	return u.InputTokens
}

// CacheReadTokens is the part of the prompt read from the provider's cache, in either family.
func (u Usage) CacheReadTokens() int64 {
	if u.Family == Anthropic {
		return u.CacheReadInputTokens
	}
	return u.CachedPromptTokens
}

// CacheWriteTokens is the part of the prompt written to the provider's cache, in either family.
func (u Usage) CacheWriteTokens() int64 {
	if u.Family == Anthropic {
		return u.CacheCreationInputTokens
	}
	return u.CacheWritePromptTokens
}

// UncachedInputTokens is the part of the prompt neither read from nor written to a cache.
func (u Usage) UncachedInputTokens() int64 {
	return u.TotalInputTokens() - u.CacheReadTokens() - u.CacheWriteTokens()
}

// TotalTokens is the whole prompt plus all output.
func (u Usage) TotalTokens() int64 { return u.TotalInputTokens() + u.OutputTokens }

// MaxTokensCeiling is the largest Limits.MaxTokens accepted. It keeps every sum of counts within int64.
const MaxTokensCeiling int64 = 1 << 60

// Limits bound what a parser reads and accepts. Every field must be positive.
type Limits struct {
	// MaxBytes bounds the bytes read from the reader, below math.MaxInt64. A longer reply is a *LimitError.
	MaxBytes int64
	// MaxLineBytes bounds one line of a stream, without its line ending, and the data of one event, its data
	// lines joined. At most math.MaxInt-5. ParseJSON does not use it.
	MaxLineBytes int
	// MaxTokens bounds every count, the whole prompt and the total. At most MaxTokensCeiling.
	MaxTokens int64
}

var (
	// ErrInvalidLimits is returned for a Limits value with a field out of range, or an unknown Family.
	ErrInvalidLimits = errors.New("modelusage: invalid limits or family")
	// ErrTooLarge matches a *LimitError for a reply longer than Limits.MaxBytes.
	ErrTooLarge = errors.New("modelusage: the reply is larger than the byte limit")
	// ErrLineTooLong matches a *LimitError for a stream line, or the data of one event, longer than
	// Limits.MaxLineBytes.
	ErrLineTooLong = errors.New("modelusage: a stream line or event is longer than the line limit")
	// ErrMalformed is returned for a reply that is not valid JSON or not a valid stream of the family, for a
	// member name that appears twice in one object, and for a count that is not an integer.
	ErrMalformed = errors.New("modelusage: the reply is malformed")
	// ErrNoUsage is returned for a reply without a usage block, or one without both its input and output
	// counts.
	ErrNoUsage = errors.New("modelusage: the reply has no usage")
	// ErrOutOfRange is returned for a count that is negative, above Limits.MaxTokens, or larger than the
	// count it is a part of. A count that is not an integer is ErrMalformed.
	ErrOutOfRange = errors.New("modelusage: a usage count is out of range")
	// ErrTruncated matches a *TruncatedError: a stream that ended, failed or reported an error before its
	// final event.
	ErrTruncated = errors.New("modelusage: the stream ended before its final event")
)

// TruncatedError is returned by ParseStream for a stream that ended, failed or carried an error event before
// its final event. It matches ErrTruncated with errors.Is, and unwraps to the reader's error when the reader
// failed.
//
// The Usage returned with it is never final. When Partial is true it holds the counts of the usage blocks
// read before the stream stopped, which can be lower than what the provider bills; when Partial is false it
// is zero.
type TruncatedError struct {
	// Partial is set when the Usage returned with the error holds the counts read so far.
	Partial bool
	// Err is the reader's error, or nil when the stream ended or carried an error event.
	Err error
}

func (e *TruncatedError) Error() string {
	msg := "modelusage: the stream ended before its final event"
	if e.Partial {
		msg += ", with partial usage"
	}
	if e.Err != nil {
		return msg + ": " + e.Err.Error()
	}
	return msg
}

// Is reports whether target is ErrTruncated.
func (e *TruncatedError) Is(target error) bool { return target == ErrTruncated }

// Unwrap returns the reader's error, or nil.
func (e *TruncatedError) Unwrap() error { return e.Err }

// LimitError is returned when a reply or a stream line passes its byte limit. It matches ErrTooLarge or
// ErrLineTooLong with errors.Is.
type LimitError struct {
	// Line is set when a stream line or the data of one event, not the whole reply, passed its limit.
	Line bool
	// Limit is the limit that was passed, in bytes.
	Limit int64
}

func (e *LimitError) Error() string {
	if e.Line {
		return fmt.Sprintf("modelusage: a stream line or event is longer than the line limit of %d bytes", e.Limit)
	}
	return fmt.Sprintf("modelusage: the reply is larger than the byte limit of %d bytes", e.Limit)
}

// Is reports whether target is ErrLineTooLong (for a line) or ErrTooLarge (for the reply).
func (e *LimitError) Is(target error) bool {
	if e.Line {
		return target == ErrLineTooLong
	}
	return target == ErrTooLarge
}

// check reports ErrInvalidLimits for an unknown family or a limit out of range. stream adds MaxLineBytes.
func (l Limits) check(f Family, stream bool) error {
	if f < Anthropic || f > OpenAIResponses {
		return ErrInvalidLimits
	}
	if l.MaxBytes <= 0 || l.MaxBytes == math.MaxInt64 || l.MaxTokens <= 0 || l.MaxTokens > MaxTokensCeiling {
		return ErrInvalidLimits
	}
	if stream && (l.MaxLineBytes <= 0 || l.MaxLineBytes > math.MaxInt-5) {
		return ErrInvalidLimits
	}
	return nil
}

// count is a token count read from a reply. set tells a count that is present apart from one that is absent
// or null; err holds the reason a present value was refused, reported once the usage block is checked.
type count struct {
	n   int64
	set bool
	err error
}

// parseCount accepts null (left unset) and integers only. A fraction, an exponent, a string or any other
// value is ErrMalformed, a negative or too large integer ErrOutOfRange. The error is kept in the count, so
// that the caller decides which error wins.
func parseCount(raw json.RawMessage) count {
	if bytes.Equal(raw, []byte("null")) {
		return count{}
	}
	// ParseInt takes only an optional sign and decimal digits, so it refuses every JSON value that is not an
	// integer; JSON itself has no leading plus sign.
	n, err := strconv.ParseInt(string(raw), 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange):
		return count{set: true, err: ErrOutOfRange}
	case err != nil:
		return count{set: true, err: ErrMalformed}
	case n < 0:
		return count{set: true, err: ErrOutOfRange}
	}
	return count{n: n, set: true}
}

// object is one JSON object of a reply, its members by their exact name.
type object map[string]json.RawMessage

// parseObject decodes data, which must be one JSON object and nothing more. A member name that appears twice
// is ErrMalformed, so two usage blocks never merge and a count is never read from the second of two keys.
// Names are matched exactly by the callers, never ignoring case.
func parseObject(data []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, ErrMalformed
	}
	o := object{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, ErrMalformed
		}
		name, ok := tok.(string)
		if !ok {
			return nil, ErrMalformed
		}
		if _, dup := o[name]; dup {
			return nil, ErrMalformed
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, ErrMalformed
		}
		o[name] = raw
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, ErrMalformed
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrMalformed
	}
	return o, nil
}

// child returns the member name as an object, or nil when it is absent or null. Any other value is
// ErrMalformed.
func (o object) child(name string) (object, error) {
	raw, ok := o[name]
	if !ok || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	return parseObject(raw)
}

// count returns the member name as a count, unset when it is absent or null.
func (o object) count(name string) count {
	raw, ok := o[name]
	if !ok {
		return count{}
	}
	return parseCount(raw)
}

// str returns the member name as a string, empty when it is absent or null. Any other value is ErrMalformed.
func (o object) str(name string) (string, error) {
	raw, ok := o[name]
	if !ok || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", ErrMalformed
	}
	return s, nil
}

// anthropicUsage is Anthropic's usage block, in a reply and in the message_start and message_delta events.
type anthropicUsage struct {
	InputTokens              count
	OutputTokens             count
	CacheCreationInputTokens count
	CacheCreation5m          count // cache_creation.ephemeral_5m_input_tokens
	CacheCreation1h          count // cache_creation.ephemeral_1h_input_tokens
	CacheReadInputTokens     count
	ThinkingTokens           count // output_tokens_details.thinking_tokens
}

// counts lists every count of the block, in a fixed order.
func (au *anthropicUsage) counts() []*count {
	return []*count{&au.InputTokens, &au.OutputTokens, &au.CacheCreationInputTokens, &au.CacheCreation5m,
		&au.CacheCreation1h, &au.CacheReadInputTokens, &au.ThinkingTokens}
}

// readAnthropicUsage reads the usage block o, which may be nil. server_tool_use counts requests, not tokens,
// and is not read.
func readAnthropicUsage(o object) (*anthropicUsage, error) {
	if o == nil {
		return nil, nil
	}
	creation, err := o.child("cache_creation")
	if err != nil {
		return nil, err
	}
	outDetails, err := o.child("output_tokens_details")
	if err != nil {
		return nil, err
	}
	au := &anthropicUsage{
		InputTokens:              o.count("input_tokens"),
		OutputTokens:             o.count("output_tokens"),
		CacheCreationInputTokens: o.count("cache_creation_input_tokens"),
		CacheReadInputTokens:     o.count("cache_read_input_tokens"),
	}
	if creation != nil {
		au.CacheCreation5m = creation.count("ephemeral_5m_input_tokens")
		au.CacheCreation1h = creation.count("ephemeral_1h_input_tokens")
	}
	if outDetails != nil {
		au.ThinkingTokens = outDetails.count("thinking_tokens")
	}
	return au, nil
}

// openAIUsage is the usage block of one of the OpenAI APIs, read under the names of its family.
type openAIUsage struct {
	in, out, cached, written, written30m, written12h, reasoning count
}

// readOpenAIUsage reads the usage block o of family f, which may be nil. Chat completions name the counts
// prompt_tokens and completion_tokens with prompt_tokens_details and completion_tokens_details, the Responses
// API input_tokens and output_tokens with input_tokens_details and output_tokens_details. Only the Responses
// API reference defines cache_write_tokens_details, so it is read for that family alone.
func readOpenAIUsage(o object, f Family) (*openAIUsage, error) {
	if o == nil {
		return nil, nil
	}
	inName, outName := "input_tokens", "output_tokens"
	if f == OpenAIChat {
		inName, outName = "prompt_tokens", "completion_tokens"
	}
	inDetails, err := o.child(inName + "_details")
	if err != nil {
		return nil, err
	}
	outDetails, err := o.child(outName + "_details")
	if err != nil {
		return nil, err
	}
	ou := &openAIUsage{in: o.count(inName), out: o.count(outName)}
	if inDetails != nil {
		ou.cached, ou.written = inDetails.count("cached_tokens"), inDetails.count("cache_write_tokens")
		if f == OpenAIResponses {
			lifetimes, err := inDetails.child("cache_write_tokens_details")
			if err != nil {
				return nil, err
			}
			if lifetimes != nil {
				ou.written30m, ou.written12h = lifetimes.count("ttl_30m_tokens"), lifetimes.count("ttl_12h_tokens")
			}
		}
	}
	if outDetails != nil {
		ou.reasoning = outDetails.count("reasoning_tokens")
	}
	return ou, nil
}

// firstErr returns the first refusal among the counts, or nil.
func firstErr(cs ...*count) error {
	for _, c := range cs {
		if c.err != nil {
			return c.err
		}
	}
	return nil
}

// anthropicOf converts a complete Anthropic usage block. Input and output must both be present.
func anthropicOf(au *anthropicUsage, maxTokens int64) (Usage, error) {
	if au == nil {
		return Usage{}, ErrNoUsage
	}
	if err := firstErr(au.counts()...); err != nil {
		return Usage{}, err
	}
	if !au.InputTokens.set || !au.OutputTokens.set {
		return Usage{}, ErrNoUsage
	}
	u := au.usage()
	if err := u.validate(maxTokens); err != nil {
		return Usage{}, err
	}
	return u, nil
}

// usage converts the counts of the block as they are, absent ones as zero.
func (au *anthropicUsage) usage() Usage {
	return Usage{
		Family:                     Anthropic,
		InputTokens:                au.InputTokens.n,
		OutputTokens:               au.OutputTokens.n,
		CacheCreationInputTokens:   au.CacheCreationInputTokens.n,
		CacheCreation5mInputTokens: au.CacheCreation5m.n,
		CacheCreation1hInputTokens: au.CacheCreation1h.n,
		CacheReadInputTokens:       au.CacheReadInputTokens.n,
		ReasoningTokens:            au.ThinkingTokens.n,
	}
}

// openAIOf converts a complete OpenAI usage block of family f. Input and output must both be present.
func openAIOf(ou *openAIUsage, f Family, maxTokens int64) (Usage, error) {
	if ou == nil {
		return Usage{}, ErrNoUsage
	}
	if err := firstErr(&ou.in, &ou.out, &ou.cached, &ou.written, &ou.written30m, &ou.written12h,
		&ou.reasoning); err != nil {
		return Usage{}, err
	}
	if !ou.in.set || !ou.out.set {
		return Usage{}, ErrNoUsage
	}
	u := Usage{
		Family:                    f,
		InputTokens:               ou.in.n,
		OutputTokens:              ou.out.n,
		CachedPromptTokens:        ou.cached.n,
		CacheWritePromptTokens:    ou.written.n,
		CacheWrite30mPromptTokens: ou.written30m.n,
		CacheWrite12hPromptTokens: ou.written12h.n,
		ReasoningTokens:           ou.reasoning.n,
	}
	if err := u.validate(maxTokens); err != nil {
		return Usage{}, err
	}
	return u, nil
}

// validate checks every count and total against maxTokens, and that no part is larger than its whole: the
// cache reads and writes within an OpenAI prompt, each lifetime breakdown within its cache writes, and the
// reasoning tokens within the output.
func (u Usage) validate(maxTokens int64) error {
	for _, n := range []int64{u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens,
		u.CacheCreation5mInputTokens, u.CacheCreation1hInputTokens, u.CacheReadInputTokens, u.CachedPromptTokens,
		u.CacheWritePromptTokens, u.CacheWrite30mPromptTokens, u.CacheWrite12hPromptTokens, u.ReasoningTokens} {
		if n < 0 || n > maxTokens {
			return ErrOutOfRange
		}
	}
	// Each count is at most MaxTokensCeiling, so no sum below overflows.
	if u.TotalInputTokens() > maxTokens || u.TotalTokens() > maxTokens {
		return ErrOutOfRange
	}
	if u.CachedPromptTokens+u.CacheWritePromptTokens > u.InputTokens && u.Family != Anthropic {
		return ErrOutOfRange
	}
	if u.CacheCreation5mInputTokens+u.CacheCreation1hInputTokens > u.CacheCreationInputTokens ||
		u.CacheWrite30mPromptTokens+u.CacheWrite12hPromptTokens > u.CacheWritePromptTokens {
		return ErrOutOfRange
	}
	if u.ReasoningTokens > u.OutputTokens {
		return ErrOutOfRange
	}
	return nil
}
