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
//     CachedPromptTokens, CacheWritePromptTokens and ReasoningTokens stay zero. OutputTokens counts every
//     output token, thinking included.
//   - OpenAIChat and OpenAIResponses: prompt_tokens (chat) or input_tokens (responses) is the whole prompt.
//     Its details, cached_tokens (cache reads) and cache_write_tokens (cache writes), are parts of it, kept
//     in CachedPromptTokens and CacheWritePromptTokens and never added again. completion_tokens or
//     output_tokens is all output, and ReasoningTokens (reasoning_tokens) is a part of it.
//     CacheCreationInputTokens and CacheReadInputTokens stay zero.
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
	// CacheReadInputTokens is Anthropic's cache_read_input_tokens, outside InputTokens.
	CacheReadInputTokens int64
	// CachedPromptTokens is OpenAI's cached_tokens (prompt_tokens_details or input_tokens_details), a part
	// of InputTokens.
	CachedPromptTokens int64
	// CacheWritePromptTokens is OpenAI's cache_write_tokens, a part of InputTokens, when reported.
	CacheWritePromptTokens int64
	// ReasoningTokens is OpenAI's reasoning_tokens (completion_tokens_details or output_tokens_details), a
	// part of OutputTokens, when reported.
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
	// MaxLineBytes bounds one line of a stream, without its line ending. ParseJSON does not use it.
	MaxLineBytes int
	// MaxTokens bounds every count, the whole prompt and the total. At most MaxTokensCeiling.
	MaxTokens int64
}

var (
	// ErrInvalidLimits is returned for a Limits value with a field out of range, or an unknown Family.
	ErrInvalidLimits = errors.New("modelusage: invalid limits or family")
	// ErrTooLarge matches a *LimitError for a reply longer than Limits.MaxBytes.
	ErrTooLarge = errors.New("modelusage: the reply is larger than the byte limit")
	// ErrLineTooLong matches a *LimitError for a stream line longer than Limits.MaxLineBytes.
	ErrLineTooLong = errors.New("modelusage: a stream line is longer than the line limit")
	// ErrMalformed is returned for a reply that is not valid JSON or not a valid stream of the family.
	ErrMalformed = errors.New("modelusage: the reply is malformed")
	// ErrNoUsage is returned for a reply without a usage block, or one without both its input and output
	// counts.
	ErrNoUsage = errors.New("modelusage: the reply has no usage")
	// ErrOutOfRange is returned for a count that is negative, not an integer, above Limits.MaxTokens, or
	// larger than the count it is a part of.
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
	// Line is set when a stream line, not the whole reply, passed its limit.
	Line bool
	// Limit is the limit that was passed, in bytes.
	Limit int64
}

func (e *LimitError) Error() string {
	if e.Line {
		return fmt.Sprintf("modelusage: a stream line is longer than the line limit of %d bytes", e.Limit)
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
	if stream && (l.MaxLineBytes <= 0 || l.MaxLineBytes >= math.MaxInt-1) {
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

// UnmarshalJSON accepts null (left unset) and integers only. A fraction, an exponent or a string is
// ErrMalformed, a negative or too large integer ErrOutOfRange. It never fails the decoding itself, so that
// the caller decides which error wins.
func (c *count) UnmarshalJSON(b []byte) error {
	*c = count{}
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	c.set = true
	if len(b) == 0 || (b[0] != '-' && (b[0] < '0' || b[0] > '9')) || bytes.ContainsAny(b, ".eE") {
		c.err = ErrMalformed
		return nil
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	switch {
	case err != nil && errors.Is(err, strconv.ErrRange):
		c.err = ErrOutOfRange
	case err != nil:
		c.err = ErrMalformed
	case n < 0:
		c.err = ErrOutOfRange
	default:
		c.n = n
	}
	return nil
}

// anthropicUsage is Anthropic's usage block, in a reply and in the message_start and message_delta events.
type anthropicUsage struct {
	InputTokens              count `json:"input_tokens"`
	OutputTokens             count `json:"output_tokens"`
	CacheCreationInputTokens count `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     count `json:"cache_read_input_tokens"`
}

// openAIUsage is the usage block of both OpenAI APIs. Chat completions name the counts prompt_tokens and
// completion_tokens, the Responses API input_tokens and output_tokens; usageOf reads the pair of the family.
type openAIUsage struct {
	PromptTokens        count `json:"prompt_tokens"`
	CompletionTokens    count `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     count `json:"cached_tokens"`
		CacheWriteTokens count `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens count `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`

	InputTokens        count `json:"input_tokens"`
	OutputTokens       count `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens     count `json:"cached_tokens"`
		CacheWriteTokens count `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens count `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// firstErr returns the first refusal among the counts, or nil.
func firstErr(cs ...count) error {
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
	if err := firstErr(au.InputTokens, au.OutputTokens, au.CacheCreationInputTokens,
		au.CacheReadInputTokens); err != nil {
		return Usage{}, err
	}
	if !au.InputTokens.set || !au.OutputTokens.set {
		return Usage{}, ErrNoUsage
	}
	u := Usage{
		Family:                   Anthropic,
		InputTokens:              au.InputTokens.n,
		OutputTokens:             au.OutputTokens.n,
		CacheCreationInputTokens: au.CacheCreationInputTokens.n,
		CacheReadInputTokens:     au.CacheReadInputTokens.n,
	}
	if err := u.validate(maxTokens); err != nil {
		return Usage{}, err
	}
	return u, nil
}

// openAIOf converts a complete OpenAI usage block of family f. Input and output must both be present.
func openAIOf(ou *openAIUsage, f Family, maxTokens int64) (Usage, error) {
	if ou == nil {
		return Usage{}, ErrNoUsage
	}
	in, out := ou.PromptTokens, ou.CompletionTokens
	var cached, written, reasoning count
	if f == OpenAIChat {
		if d := ou.PromptTokensDetails; d != nil {
			cached, written = d.CachedTokens, d.CacheWriteTokens
		}
		if d := ou.CompletionTokensDetails; d != nil {
			reasoning = d.ReasoningTokens
		}
	} else {
		in, out = ou.InputTokens, ou.OutputTokens
		if d := ou.InputTokensDetails; d != nil {
			cached, written = d.CachedTokens, d.CacheWriteTokens
		}
		if d := ou.OutputTokensDetails; d != nil {
			reasoning = d.ReasoningTokens
		}
	}
	if err := firstErr(in, out, cached, written, reasoning); err != nil {
		return Usage{}, err
	}
	if !in.set || !out.set {
		return Usage{}, ErrNoUsage
	}
	u := Usage{
		Family:                 f,
		InputTokens:            in.n,
		OutputTokens:           out.n,
		CachedPromptTokens:     cached.n,
		CacheWritePromptTokens: written.n,
		ReasoningTokens:        reasoning.n,
	}
	if err := u.validate(maxTokens); err != nil {
		return Usage{}, err
	}
	return u, nil
}

// validate checks every count and total against maxTokens, and that no part is larger than its whole.
func (u Usage) validate(maxTokens int64) error {
	for _, n := range []int64{u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens,
		u.CachedPromptTokens, u.CacheWritePromptTokens, u.ReasoningTokens} {
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
	if u.ReasoningTokens > u.OutputTokens {
		return ErrOutOfRange
	}
	return nil
}

// decodeObject decodes data, which must be one JSON object, into v. Anything else is ErrMalformed.
func decodeObject(data []byte, v any) error {
	t := bytes.TrimSpace(data)
	if len(t) == 0 || t[0] != '{' {
		return ErrMalformed
	}
	if err := json.Unmarshal(t, v); err != nil {
		return ErrMalformed
	}
	return nil
}
