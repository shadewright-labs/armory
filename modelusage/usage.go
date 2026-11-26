package modelusage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
// parent without a breakdown (an older model, or a field it does not send).
//
// A breakdown is checked against its parent only where the provider's reference bounds it and the parent is
// present: cached_tokens and cache_write_tokens within the OpenAI prompt ("a detailed breakdown of the input
// tokens"), reasoning_tokens within the OpenAI output ("a detailed breakdown of the output tokens"), and
// Anthropic's thinking_tokens within output_tokens ("Always <= output_tokens"). A larger one is
// ErrOutOfRange. In an Anthropic stream that check runs once, on the counts at message_stop, never between
// two events that update the part and the whole.
//
// The cache write lifetimes are reported as given and never checked: neither reference says they sum to at
// most the cache writes (Anthropic calls cache_creation a "breakdown of cached tokens by TTL", and the
// Responses API describes ttl_30m_tokens and ttl_12h_tokens only as tokens written with that retention), and
// Anthropic sends them in message_start while the cumulative cache writes may change in a later event. They
// can therefore sum to more than CacheCreationInputTokens or CacheWritePromptTokens, or be set while that
// count is absent and zero; the count is never made up from them. A caller pricing by lifetime should cap
// the lifetimes at the cache writes, or price the writes as a whole.
//
// A Usage returned with a nil error or a [*TrailingError] keeps these rules, and every count and total is
// within the caller's [Limits.MaxTokens]. One returned with a [*PartialError] keeps the limit but not the
// part rules, since it may hold counts from between two updates.
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
	// ErrTruncated is returned for a stream that ended, failed or carried an error event before its final
	// event. When the stream yielded usage first, it comes inside a *PartialError.
	ErrTruncated = errors.New("modelusage: the stream ended before its final event")
)

// PartialError is returned by ParseStream when a stream yielded at least one usage block and then stopped or
// broke before its final event: it ended or was cut, carried an error event, a later event was malformed or
// held a refused count, or it passed a byte limit. Err is the cause, so errors.Is still matches ErrTruncated,
// ErrMalformed, ErrOutOfRange, ErrNoUsage, ErrTooLarge, ErrLineTooLong or the reader's error.
//
// The Usage returned with it is never zero and is a lower bound of what the provider reported: every count
// is the largest value the stream carried before the problem, and nothing after the last usage block can
// lower it. It is not final, and can be lower than what the provider bills. Each count and total is within
// Limits.MaxTokens, but a breakdown may be larger than its parent, since the counts may be from between two
// updates.
type PartialError struct {
	// Err is the cause. It is never nil.
	Err error
}

func (e *PartialError) Error() string { return "modelusage: partial usage: " + e.Err.Error() }

// Unwrap returns the cause.
func (e *PartialError) Unwrap() error { return e.Err }

// TrailingError is returned by ParseStream when a stream sent its final event and then something that is not
// allowed after it: an event that would change the counts or end the reply again, a malformed event, an
// event cut by the end of the stream, a byte limit passed, or a reader error. Err is the cause, as in
// PartialError.
//
// The Usage returned with it is the final usage, the same as with a nil error, since the counts were complete
// at the final event. The error tells the caller that the bytes after it were wrong, for logging; it is not a
// reason to doubt the counts.
type TrailingError struct {
	// Err is the cause. It is never nil.
	Err error
}

func (e *TrailingError) Error() string {
	return "modelusage: the usage is final, but the stream is wrong after its final event: " + e.Err.Error()
}

// Unwrap returns the cause.
func (e *TrailingError) Unwrap() error { return e.Err }

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

// parseCount accepts null (left unset) and integers only. Any value that is not an optional minus sign and
// decimal digits (a fraction, an exponent, a string) is ErrMalformed, whatever its magnitude; a negative or too
// large integer is ErrOutOfRange. The error is kept in the count, so that the caller decides which error wins.
func parseCount(raw json.RawMessage) count {
	if bytes.Equal(raw, []byte("null")) {
		return count{}
	}
	digits := bytes.TrimPrefix(raw, []byte("-"))
	if len(digits) == 0 || bytes.ContainsFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) {
		return count{set: true, err: ErrMalformed}
	}
	// Only a sign and digits are left, so ParseInt fails only on range.
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || n < 0 {
		return count{set: true, err: ErrOutOfRange}
	}
	return count{n: n, set: true}
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

// readAnthropicUsage reads the raw usage block v, which may be nil (absent) or null. server_tool_use counts
// requests, not tokens, and is not read.
func readAnthropicUsage(v []byte) (*anthropicUsage, error) {
	if v == nil || isNull(v) {
		return nil, nil
	}
	au := &anthropicUsage{}
	err := members(v, func(name, value []byte) error {
		switch string(name) {
		case "input_tokens":
			au.InputTokens = parseCount(value)
		case "output_tokens":
			au.OutputTokens = parseCount(value)
		case "cache_creation_input_tokens":
			au.CacheCreationInputTokens = parseCount(value)
		case "cache_read_input_tokens":
			au.CacheReadInputTokens = parseCount(value)
		case "cache_creation":
			return object(value, func(name, value []byte) error {
				switch string(name) {
				case "ephemeral_5m_input_tokens":
					au.CacheCreation5m = parseCount(value)
				case "ephemeral_1h_input_tokens":
					au.CacheCreation1h = parseCount(value)
				}
				return nil
			})
		case "output_tokens_details":
			return object(value, func(name, value []byte) error {
				if string(name) == "thinking_tokens" {
					au.ThinkingTokens = parseCount(value)
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return au, nil
}

// openAIUsage is the usage block of one of the OpenAI APIs, read under the names of its family.
type openAIUsage struct {
	in, out, cached, written, written30m, written12h, reasoning count
}

// readOpenAIUsage reads the raw usage block v of family f, which may be nil (absent) or null. Chat completions
// name the counts prompt_tokens and completion_tokens with prompt_tokens_details and completion_tokens_details,
// the Responses API input_tokens and output_tokens with input_tokens_details and output_tokens_details. Only
// the Responses API reference defines cache_write_tokens_details, so it is read for that family alone.
func readOpenAIUsage(v []byte, f Family) (*openAIUsage, error) {
	if v == nil || isNull(v) {
		return nil, nil
	}
	inName, outName := "input_tokens", "output_tokens"
	if f == OpenAIChat {
		inName, outName = "prompt_tokens", "completion_tokens"
	}
	ou := &openAIUsage{}
	inDetails := func(name, value []byte) error {
		switch string(name) {
		case "cached_tokens":
			ou.cached = parseCount(value)
		case "cache_write_tokens":
			ou.written = parseCount(value)
		case "cache_write_tokens_details":
			if f != OpenAIResponses {
				return nil
			}
			return object(value, func(name, value []byte) error {
				switch string(name) {
				case "ttl_30m_tokens":
					ou.written30m = parseCount(value)
				case "ttl_12h_tokens":
					ou.written12h = parseCount(value)
				}
				return nil
			})
		}
		return nil
	}
	err := members(v, func(name, value []byte) error {
		switch n := string(name); n {
		case inName:
			ou.in = parseCount(value)
		case outName:
			ou.out = parseCount(value)
		case inName + "_details":
			return object(value, inDetails)
		case outName + "_details":
			return object(value, func(name, value []byte) error {
				if string(name) == "reasoning_tokens" {
					ou.reasoning = parseCount(value)
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
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

// validate checks every count and total against maxTokens (checkRange) and that a part the provider's
// reference bounds by its whole is no larger (checkParts).
func (u Usage) validate(maxTokens int64) error {
	if err := u.checkRange(maxTokens); err != nil {
		return err
	}
	return u.checkParts()
}

// checkRange checks that every count, the whole prompt and the total are within [0, maxTokens].
func (u Usage) checkRange(maxTokens int64) error {
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
	return nil
}

// checkParts checks the parts the references bound by their whole: the cache reads and writes within an
// OpenAI prompt, and the reasoning or thinking tokens within the output. The cache write lifetimes are not
// checked (see Usage).
func (u Usage) checkParts() error {
	if u.CachedPromptTokens+u.CacheWritePromptTokens > u.InputTokens && u.Family != Anthropic {
		return ErrOutOfRange
	}
	if u.ReasoningTokens > u.OutputTokens {
		return ErrOutOfRange
	}
	return nil
}

// maxOf returns, count by count, the larger of a and b, with the family of b.
func maxOf(a, b Usage) Usage {
	return Usage{
		Family:                     b.Family,
		InputTokens:                max(a.InputTokens, b.InputTokens),
		OutputTokens:               max(a.OutputTokens, b.OutputTokens),
		CacheCreationInputTokens:   max(a.CacheCreationInputTokens, b.CacheCreationInputTokens),
		CacheCreation5mInputTokens: max(a.CacheCreation5mInputTokens, b.CacheCreation5mInputTokens),
		CacheCreation1hInputTokens: max(a.CacheCreation1hInputTokens, b.CacheCreation1hInputTokens),
		CacheReadInputTokens:       max(a.CacheReadInputTokens, b.CacheReadInputTokens),
		CachedPromptTokens:         max(a.CachedPromptTokens, b.CachedPromptTokens),
		CacheWritePromptTokens:     max(a.CacheWritePromptTokens, b.CacheWritePromptTokens),
		CacheWrite30mPromptTokens:  max(a.CacheWrite30mPromptTokens, b.CacheWrite30mPromptTokens),
		CacheWrite12hPromptTokens:  max(a.CacheWrite12hPromptTokens, b.CacheWrite12hPromptTokens),
		ReasoningTokens:            max(a.ReasoningTokens, b.ReasoningTokens),
	}
}
