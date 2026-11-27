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
//   - [Anthropic]: input_tokens counts only executor input after the last cache breakpoint. The cache writes
//     (cache_creation_input_tokens) and cache reads (cache_read_input_tokens) are reported apart and are
//     not part of it. Compaction iterations (usage.iterations entries with type "compaction") and advisor
//     iterations (entries with type "advisor_message", billed at the advisor model's rate) are also reported
//     apart from the top-level counts and are summed into the Compaction* and Advisor* fields
//     (CompactionInputTokens, CompactionOutputTokens, CompactionCacheCreationInputTokens,
//     CompactionCacheReadInputTokens, AdvisorInputTokens, AdvisorOutputTokens,
//     AdvisorCacheCreationInputTokens and AdvisorCacheReadInputTokens, with their 5m and 1h cache write
//     lifetimes). message entries are already reflected in the top-level executor counts, so their counts are
//     validated but not added again. Iteration entries the package does not classify (fallback_message, and
//     entries with a null, missing or unknown type) have their counts validated and summed into
//     UnclassifiedIterationInputTokens (all input-side counts: input_tokens, cache_creation_input_tokens and
//     cache_read_input_tokens) and UnclassifiedIterationOutputTokens (output_tokens). Those two fields are
//     excluded from TotalInputTokens, TotalOutputTokens and TotalTokens because an unknown entry may repeat
//     counts already in the top-level usage (as message entries do), keeping the totals a lower bound; callers
//     enforcing a budget should add them. The whole prompt is therefore top-level input and cache counts plus
//     the compaction and advisor input and cache counts, and all output is OutputTokens +
//     CompactionOutputTokens + AdvisorOutputTokens. The cache writes by lifetime
//     (cache_creation.ephemeral_5m_input_tokens and ephemeral_1h_input_tokens) are parts of
//     CacheCreationInputTokens (or the corresponding compaction or advisor cache creation field). OutputTokens
//     counts top-level executor output tokens, thinking included, and ReasoningTokens
//     (output_tokens_details.thinking_tokens) is a part of it; callers pricing directly from OutputTokens must
//     also add CompactionOutputTokens and AdvisorOutputTokens, or call TotalOutputTokens. CachedPromptTokens,
//     CacheWritePromptTokens, the OpenAI lifetimes and ReportedTotalTokens stay zero.
//   - [OpenAIChat] and [OpenAIResponses]: prompt_tokens (chat) or input_tokens (responses) is the whole prompt.
//     Its details, cached_tokens (cache reads) and cache_write_tokens (cache writes), are parts of it, kept
//     in CachedPromptTokens and CacheWritePromptTokens and never added again. The Responses API also splits
//     the cache writes by lifetime (cache_write_tokens_details.ttl_30m_tokens and ttl_12h_tokens), parts of
//     CacheWritePromptTokens. completion_tokens or output_tokens is all output, and ReasoningTokens
//     (reasoning_tokens) is a part of it. When total_tokens is present and positive it must be an integer
//     within [Limits.MaxTokens] and at least InputTokens + OutputTokens, and is kept in ReportedTotalTokens;
//     a total_tokens of 0 next to non-zero counts is treated as absent (ReportedTotalTokens stays zero).
//     TotalTokens returns the larger of InputTokens + OutputTokens and ReportedTotalTokens, so it can exceed
//     TotalInputTokens() + TotalOutputTokens() when the provider reports a higher total.
//     CacheCreationInputTokens, CacheReadInputTokens, the Anthropic lifetimes, the compaction fields, the
//     advisor fields and the unclassified iteration fields stay zero.
//
// A breakdown is a part of its parent count and never adds to a total. A provider may leave part of a
// parent without a breakdown (an older model, or a field it does not send).
//
// A breakdown is checked against its parent only where the provider's reference bounds it and the parent is
// present: cached_tokens and cache_write_tokens within the OpenAI prompt ("a detailed breakdown of the input
// tokens"), reasoning_tokens within the OpenAI output ("a detailed breakdown of the output tokens"), and
// Anthropic's thinking_tokens within output_tokens ("Always <= output_tokens"). A larger one is
// [ErrOutOfRange]. In an Anthropic stream that check runs once, on the counts at message_stop, never between
// two events that update the part and the whole.
//
// The cache write lifetimes are reported as given and never checked: neither reference says they sum to at
// most the cache writes (Anthropic calls cache_creation a "breakdown of cached tokens by TTL", and the
// Responses API describes ttl_30m_tokens and ttl_12h_tokens only as tokens written with that retention), and
// Anthropic sends them in message_start while the cumulative cache writes may change in a later event. This
// rule covers the top-level fields (CacheCreation5mInputTokens, CacheCreation1hInputTokens,
// CacheWrite30mPromptTokens, CacheWrite12hPromptTokens) and the Compaction* and Advisor* cache lifetime fields
// (CompactionCacheCreation5mInputTokens, CompactionCacheCreation1hInputTokens,
// AdvisorCacheCreation5mInputTokens, AdvisorCacheCreation1hInputTokens). They can therefore sum to more than
// their parent cache write count, or be set while that parent count is absent and zero; the parent count is
// never inferred from them, so a lifetime field without its parent count adds nothing to totals. A caller
// pricing by lifetime should cap the lifetimes at the parent cache writes, or price the writes as a whole.
//
// A Usage returned with a nil error or a [*TrailingError] keeps these rules, and every count and total is
// within the caller's [Limits.MaxTokens]. One returned with a [*PartialError] keeps the limit but not the
// part rules, since it may hold counts from between two updates. Callers building a Usage literal must use
// keyed fields, because new fields may be added in minor versions.
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
	// CompactionInputTokens is the sum of input_tokens across Anthropic usage.iterations entries of type
	// compaction, outside InputTokens.
	CompactionInputTokens int64
	// CompactionOutputTokens is the sum of output_tokens across Anthropic usage.iterations entries of type
	// compaction, outside OutputTokens.
	CompactionOutputTokens int64
	// CompactionCacheCreationInputTokens is the sum of cache_creation_input_tokens across Anthropic
	// usage.iterations entries of type compaction, outside CacheCreationInputTokens.
	CompactionCacheCreationInputTokens int64
	// CompactionCacheCreation5mInputTokens is the sum of cache_creation.ephemeral_5m_input_tokens across
	// Anthropic usage.iterations entries of type compaction, a part of CompactionCacheCreationInputTokens.
	CompactionCacheCreation5mInputTokens int64
	// CompactionCacheCreation1hInputTokens is the sum of cache_creation.ephemeral_1h_input_tokens across
	// Anthropic usage.iterations entries of type compaction, a part of CompactionCacheCreationInputTokens.
	CompactionCacheCreation1hInputTokens int64
	// CompactionCacheReadInputTokens is the sum of cache_read_input_tokens across Anthropic usage.iterations
	// entries of type compaction, outside CacheReadInputTokens.
	CompactionCacheReadInputTokens int64
	// AdvisorInputTokens is the sum of input_tokens across Anthropic usage.iterations entries of type
	// advisor_message, outside InputTokens because advisor tokens bill at the advisor model's rate.
	AdvisorInputTokens int64
	// AdvisorOutputTokens is the sum of output_tokens across Anthropic usage.iterations entries of type
	// advisor_message, outside OutputTokens.
	AdvisorOutputTokens int64
	// AdvisorCacheCreationInputTokens is the sum of cache_creation_input_tokens across Anthropic
	// usage.iterations entries of type advisor_message, outside CacheCreationInputTokens.
	AdvisorCacheCreationInputTokens int64
	// AdvisorCacheCreation5mInputTokens is the sum of cache_creation.ephemeral_5m_input_tokens across
	// Anthropic usage.iterations entries of type advisor_message, a part of AdvisorCacheCreationInputTokens.
	AdvisorCacheCreation5mInputTokens int64
	// AdvisorCacheCreation1hInputTokens is the sum of cache_creation.ephemeral_1h_input_tokens across
	// Anthropic usage.iterations entries of type advisor_message, a part of AdvisorCacheCreationInputTokens.
	AdvisorCacheCreation1hInputTokens int64
	// AdvisorCacheReadInputTokens is the sum of cache_read_input_tokens across Anthropic usage.iterations
	// entries of type advisor_message, outside CacheReadInputTokens.
	AdvisorCacheReadInputTokens int64
	// UnclassifiedIterationInputTokens is the sum of input-side token counts (input_tokens,
	// cache_creation_input_tokens and cache_read_input_tokens) across Anthropic usage.iterations entries
	// that the package does not classify (fallback_message, and entries with a null, missing or unknown type).
	// It is not included in TotalInputTokens or TotalTokens because an unknown entry may repeat counts that
	// are already in the top-level usage; callers enforcing a budget should add it.
	UnclassifiedIterationInputTokens int64
	// UnclassifiedIterationOutputTokens is the sum of output_tokens across Anthropic usage.iterations
	// entries that the package does not classify (fallback_message, and entries with a null, missing or
	// unknown type). It is not included in TotalOutputTokens or TotalTokens because an unknown entry may
	// repeat counts that are already in the top-level usage; callers enforcing a budget should add it.
	UnclassifiedIterationOutputTokens int64
	// ReportedTotalTokens is OpenAI's total_tokens when reported and non-zero. It is zero when the reply
	// omits total_tokens, sends null, sends 0 next to non-zero counts, or uses [Anthropic].
	ReportedTotalTokens int64
}

// TotalInputTokens is the whole prompt: every input token the provider counted, cached, compaction and
// advisor iterations included. For [Anthropic], it excludes UnclassifiedIterationInputTokens; callers
// enforcing a budget should add that field.
func (u Usage) TotalInputTokens() int64 {
	if u.Family == Anthropic {
		return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens +
			u.CompactionInputTokens + u.CompactionCacheCreationInputTokens + u.CompactionCacheReadInputTokens +
			u.AdvisorInputTokens + u.AdvisorCacheCreationInputTokens + u.AdvisorCacheReadInputTokens
	}
	return u.InputTokens
}

// TotalOutputTokens is all output tokens: OutputTokens plus CompactionOutputTokens and AdvisorOutputTokens
// for [Anthropic] (excluding UnclassifiedIterationOutputTokens; callers enforcing a budget should add that
// field), and OutputTokens for the OpenAI families.
func (u Usage) TotalOutputTokens() int64 {
	if u.Family == Anthropic {
		return u.OutputTokens + u.CompactionOutputTokens + u.AdvisorOutputTokens
	}
	return u.OutputTokens
}

// CacheReadTokens is the part of the prompt read from the provider's cache, for any family. For [Anthropic]
// this includes compaction and advisor iterations.
func (u Usage) CacheReadTokens() int64 {
	if u.Family == Anthropic {
		return u.CacheReadInputTokens + u.CompactionCacheReadInputTokens + u.AdvisorCacheReadInputTokens
	}
	return u.CachedPromptTokens
}

// CacheWriteTokens is the part of the prompt written to the provider's cache, for any family. For [Anthropic]
// this includes compaction and advisor iterations.
func (u Usage) CacheWriteTokens() int64 {
	if u.Family == Anthropic {
		return u.CacheCreationInputTokens + u.CompactionCacheCreationInputTokens +
			u.AdvisorCacheCreationInputTokens
	}
	return u.CacheWritePromptTokens
}

// UncachedInputTokens is the part of the prompt neither read from nor written to a cache. On a [Usage]
// returned with a [*PartialError] it is not a lower bound (see [PartialError]).
func (u Usage) UncachedInputTokens() int64 {
	return u.TotalInputTokens() - u.CacheReadTokens() - u.CacheWriteTokens()
}

// TotalTokens is the larger of TotalInputTokens() + TotalOutputTokens() and ReportedTotalTokens. It can
// therefore exceed TotalInputTokens() + TotalOutputTokens() when the provider reports a higher total_tokens
// (for example when thinking tokens are left out of completion_tokens). For [Anthropic], it excludes
// UnclassifiedIterationInputTokens and UnclassifiedIterationOutputTokens; callers enforcing a budget should
// add those fields.
func (u Usage) TotalTokens() int64 {
	return max(u.TotalInputTokens()+u.TotalOutputTokens(), u.ReportedTotalTokens)
}

// MaxTokensCeiling is the largest [Limits.MaxTokens] accepted. It keeps every sum of counts within int64.
const MaxTokensCeiling int64 = 1 << 60

// Limits bound what a parser reads and accepts. Every field the parser uses must be positive.
type Limits struct {
	// MaxBytes bounds the bytes read from the reader, below [math.MaxInt64]. A longer reply is a [*LimitError].
	MaxBytes int64
	// MaxLineBytes bounds one line of a stream, without its line ending, and the data of one event, its data
	// lines joined. At most [math.MaxInt]-5. [ParseJSON] does not use it.
	MaxLineBytes int
	// MaxTokens bounds every count, the whole prompt and the total. At most [MaxTokensCeiling].
	MaxTokens int64
}

var (
	// ErrInvalidLimits is returned for a [Limits] value with a field out of range, or an unknown [Family].
	ErrInvalidLimits = errors.New("modelusage: invalid limits or family")
	// ErrTooLarge matches a [*LimitError] for a reply longer than [Limits.MaxBytes].
	ErrTooLarge = errors.New("modelusage: the reply is larger than the byte limit")
	// ErrLineTooLong matches a [*LimitError] for a stream line, or the data of one event, longer than
	// [Limits.MaxLineBytes].
	ErrLineTooLong = errors.New("modelusage: a stream line or event is longer than the line limit")
	// ErrMalformed is returned for a reply that is not valid JSON or not a valid stream of the family, for a
	// member name that appears twice in one object, and for a count that is not an integer.
	ErrMalformed = errors.New("modelusage: the reply is malformed")
	// ErrNoUsage is returned for a reply without a usage block, or one without both its input and output
	// counts.
	ErrNoUsage = errors.New("modelusage: the reply has no usage")
	// ErrOutOfRange is returned for a count that is negative, above [Limits.MaxTokens], larger than the
	// count it is a part of, or a positive OpenAI total_tokens smaller than input plus output. A count that
	// is not an integer is [ErrMalformed].
	ErrOutOfRange = errors.New("modelusage: a usage count is out of range")
	// ErrTruncated is returned for a stream that ended, failed or carried an error event before its final
	// event, and for an [OpenAIResponses] response whose status is not terminal (completed, incomplete,
	// failed or cancelled), including when usage is null or absent. When a stream yielded usage first, it
	// comes inside a [*PartialError].
	ErrTruncated = errors.New("modelusage: the reply ended before reaching a final state")
	// ErrInconsistent is returned inside a [*PartialError] for a stream that contradicts itself before its
	// final event: an [Anthropic] count lower than the value an earlier block reported (including a present
	// non-null usage.iterations array that omits a previously reported iteration category or sends an empty
	// array, dropping that iteration sum to zero), or a chat completions usage block with any count lower than
	// in the block before (comparing ReportedTotalTokens only when the later block carries a non-zero
	// total_tokens). The [Usage] then holds the latest values.
	ErrInconsistent = errors.New("modelusage: the stream reported usage that contradicts its earlier usage")
)

// PartialError is returned by [ParseStream] when a stream yielded at least one usage block with a count and
// then stopped or broke before its final event. That covers a stream that ended, an event cut by the end, a
// reader failure, an error event, a later event that was malformed, contradicted the usage so far or held a
// refused count, or a byte limit passed. Err is the cause, so [errors.Is] still matches [ErrTruncated],
// [ErrMalformed], [ErrInconsistent], [ErrOutOfRange], [ErrNoUsage], [ErrTooLarge], [ErrLineTooLong] or the
// reader's error. ParseStream returns it unwrapped, and its cause never holds a *PartialError or a
// [*TrailingError].
//
// The [Usage] returned with it is the usage so far under the rules of the family (see [ParseStream]). It is
// not final and can be lower than what the provider bills. What a caller may rely on:
//
//   - Unless errors.Is(err, ErrInconsistent), every count except ReportedTotalTokens is at least the same
//     count that ParseStream returns under the same [Limits] for any shorter part of the same stream cut after
//     an event (only for the same Limits: a larger [Limits.MaxTokens] can accept a block a smaller one
//     refuses). The sums [Usage.TotalInputTokens] and [Usage.TotalOutputTokens] are therefore lower bounds of
//     the usage the stream reported. In [OpenAIChat] streams, ReportedTotalTokens (and [Usage.TotalTokens]
//     when an earlier chunk reported a higher total) is also a lower bound whenever the later chunk carries a
//     non-zero total_tokens; a later chunk that omits total_tokens leaves ReportedTotalTokens zero without
//     triggering ErrInconsistent.
//   - [Usage.UncachedInputTokens] is a difference, not a sum, and is not a lower bound: a later report can
//     raise the cached or cache write count of an unchanged prompt and so lower it. Price a partial result by
//     its sums, not by its uncached part.
//   - With ErrInconsistent the counts are the latest the stream sent, and at least one is lower than an
//     earlier report. They bound nothing; the stream disagrees with itself.
//
// Each count and total is within Limits.MaxTokens, but a breakdown may be larger than its parent, since the
// counts may be from between two updates.
type PartialError struct {
	// Err is the cause. [ParseStream] never leaves it nil.
	Err error
}

// Error returns fixed text and the bounded text of the cause. It is safe on a nil or zero value.
func (e *PartialError) Error() string {
	if e == nil {
		return "modelusage: partial usage: nil error value"
	}
	return "modelusage: partial usage: " + causeText(e.Err, maxCauseText)
}

// Unwrap returns the cause, nil for a nil value.
func (e *PartialError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// TrailingError is returned by [ParseStream] when a stream sent its final event and then something that is not
// allowed after it. That includes an event that would change the counts or end the reply again, a malformed
// event, an event the input ended inside, a byte limit passed, a reader error or an error event. Err is the
// cause, as in [PartialError], and ParseStream returns it unwrapped.
//
// The [Usage] returned with it is the final usage, the same as with a nil error, since the counts were
// complete at the final event. The error tells the caller that the bytes after it were wrong, for logging; it
// is not a reason to doubt the counts.
type TrailingError struct {
	// Err is the cause. [ParseStream] never leaves it nil.
	Err error
}

// Error returns fixed text and the bounded text of the cause. It is safe on a nil or zero value.
func (e *TrailingError) Error() string {
	const prefix = "modelusage: the usage is final, but the stream is wrong after its final event: "
	if e == nil {
		return prefix + "nil error value"
	}
	return prefix + causeText(e.Err, maxCauseText)
}

// Unwrap returns the cause, nil for a nil value.
func (e *TrailingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// maxCauseText is how many bytes of a cause's text the message of a PartialError or TrailingError carries.
const maxCauseText = 160

// causeText returns the text of err for an error message: at most limit bytes, with every byte that is not
// printable ASCII replaced by a question mark, so that a cause cannot carry control bytes or long text into a
// log line.
func causeText(err error, limit int) string {
	if err == nil {
		return "no cause recorded"
	}
	text := err.Error()
	b := make([]byte, 0, min(len(text), limit)+3)
	for i := range len(text) {
		if i == limit {
			b = append(b, "..."...)
			break
		}
		c := text[i]
		if c < ' ' || c > '~' {
			c = '?'
		}
		b = append(b, c)
	}
	return string(b)
}

// LimitError is returned when a reply, a stream line or the data of one event passes its byte limit. It
// matches [ErrTooLarge] or [ErrLineTooLong] with [errors.Is].
type LimitError struct {
	// Line is set when a stream line or the data of one event, not the whole reply, passed its limit.
	Line bool
	// Limit is the limit that was passed, in bytes.
	Limit int64
}

// Error returns fixed text with the limit. It is safe on a nil value.
func (e *LimitError) Error() string {
	if e == nil {
		return "modelusage: a byte limit was passed"
	}
	if e.Line {
		return fmt.Sprintf("modelusage: a stream line or event is longer than the line limit of %d bytes", e.Limit)
	}
	return fmt.Sprintf("modelusage: the reply is larger than the byte limit of %d bytes", e.Limit)
}

// Is reports whether target is [ErrLineTooLong] (for a line) or [ErrTooLarge] (for the reply).
func (e *LimitError) Is(target error) bool {
	if e == nil {
		return false
	}
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

	// Compaction totals summed across usage.iterations entries of type "compaction".
	CompactionInputTokens              count
	CompactionOutputTokens             count
	CompactionCacheCreationInputTokens count
	CompactionCacheCreation5m          count
	CompactionCacheCreation1h          count
	CompactionCacheReadInputTokens     count

	// Advisor totals summed across usage.iterations entries of type "advisor_message".
	AdvisorInputTokens              count
	AdvisorOutputTokens             count
	AdvisorCacheCreationInputTokens count
	AdvisorCacheCreation5m          count
	AdvisorCacheCreation1h          count
	AdvisorCacheReadInputTokens     count

	// Unclassified totals summed across usage.iterations entries the package does not classify
	// ("fallback_message", and entries with a null, missing or unknown type).
	UnclassifiedIterationInputTokens  count
	UnclassifiedIterationOutputTokens count

	// ignoredMax records the first refusal and the largest count seen across validated-only counts ("message"
	// iteration entries, and cache_creation lifetimes inside unclassified iteration entries).
	ignoredMax    count
	iterationsSet bool // a non-null usage.iterations array was present
}

// topCounts lists the top-level counts of the block, in a fixed order.
func (au *anthropicUsage) topCounts() []*count {
	return []*count{&au.InputTokens, &au.OutputTokens, &au.CacheCreationInputTokens, &au.CacheCreation5m,
		&au.CacheCreation1h, &au.CacheReadInputTokens, &au.ThinkingTokens}
}

// iterationCounts lists the compaction, advisor and unclassified iteration counts of the block, in a fixed order.
func (au *anthropicUsage) iterationCounts() []*count {
	return []*count{
		&au.CompactionInputTokens, &au.CompactionOutputTokens, &au.CompactionCacheCreationInputTokens,
		&au.CompactionCacheCreation5m, &au.CompactionCacheCreation1h, &au.CompactionCacheReadInputTokens,
		&au.AdvisorInputTokens, &au.AdvisorOutputTokens, &au.AdvisorCacheCreationInputTokens,
		&au.AdvisorCacheCreation5m, &au.AdvisorCacheCreation1h, &au.AdvisorCacheReadInputTokens,
		&au.UnclassifiedIterationInputTokens, &au.UnclassifiedIterationOutputTokens,
	}
}

// counts lists every count of the block, including the validated-only tracker, in a fixed order.
func (au *anthropicUsage) counts() []*count {
	all := make([]*count, 0, 22)
	all = append(all, au.topCounts()...)
	all = append(all, au.iterationCounts()...)
	return append(all, &au.ignoredMax)
}

// addCount adds src into dst without overflowing MaxTokensCeiling, preserving the first error seen.
func addCount(dst *count, src count) {
	if !src.set {
		return
	}
	dst.set = true
	if dst.err != nil {
		return
	}
	if src.err != nil {
		dst.err = src.err
		return
	}
	if src.n > MaxTokensCeiling-dst.n {
		dst.err = ErrOutOfRange
		return
	}
	dst.n += src.n
}

// recordCheckedCount records src's error or maximum value into dst without adding to any Usage field.
func recordCheckedCount(dst *count, src count) {
	if !src.set || dst.err != nil {
		return
	}
	if src.err != nil {
		dst.err = src.err
		return
	}
	if src.n > dst.n {
		dst.n = src.n
	}
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
		case nameInputTokens:
			au.InputTokens = parseCount(value)
		case nameOutputTokens:
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
		case "iterations":
			return readAnthropicIterations(value, au)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return au, nil
}

// readAnthropicIterations reads Anthropic's usage.iterations array into au. Compaction and advisor_message
// entries are summed into the corresponding fields on au; message entries have their counts validated without
// changing totals; entries the package does not classify (fallback_message, and entries with a null, missing
// or unknown type) have their counts validated and summed into UnclassifiedIterationInputTokens and
// UnclassifiedIterationOutputTokens. Nothing is retained per entry, so Limits.MaxBytes and
// Limits.MaxLineBytes bound the array.
func readAnthropicIterations(v []byte, au *anthropicUsage) error {
	if isNull(v) {
		return nil
	}
	err := elements(v, func(elem []byte) error {
		return readAnthropicIterationEntry(elem, au)
	})
	if err != nil {
		return err
	}
	au.iterationsSet = true
	return nil
}

// readAnthropicIterationEntry reads one element of usage.iterations, validating counts on every entry and
// summing compaction, advisor_message and unclassified entries into au.
func readAnthropicIterationEntry(elem []byte, au *anthropicUsage) error {
	var (
		typ                                            []byte
		in, out, cacheCreate, cacheRead, cacheCreation []byte
	)
	err := members(elem, func(name, value []byte) error {
		switch string(name) {
		case "type":
			t, err := str(value)
			typ = t
			return err
		case nameInputTokens:
			in = value
		case nameOutputTokens:
			out = value
		case "cache_creation_input_tokens":
			cacheCreate = value
		case "cache_read_input_tokens":
			cacheRead = value
		case "cache_creation":
			cacheCreation = value
		}
		return nil
	})
	if err != nil {
		return err
	}
	var dst [6]*count
	switch string(typ) {
	case "compaction":
		dst = [6]*count{
			&au.CompactionInputTokens, &au.CompactionOutputTokens, &au.CompactionCacheCreationInputTokens,
			&au.CompactionCacheCreation5m, &au.CompactionCacheCreation1h, &au.CompactionCacheReadInputTokens,
		}
	case "advisor_message":
		dst = [6]*count{
			&au.AdvisorInputTokens, &au.AdvisorOutputTokens, &au.AdvisorCacheCreationInputTokens,
			&au.AdvisorCacheCreation5m, &au.AdvisorCacheCreation1h, &au.AdvisorCacheReadInputTokens,
		}
	case "message":
	default:
		// fallback_message, and entries with a null, missing or unknown type. All input-side counts
		// (input_tokens, cache_creation_input_tokens, cache_read_input_tokens) sum into
		// UnclassifiedIterationInputTokens; cache_creation lifetimes are validated without adding again.
		dst = [6]*count{
			&au.UnclassifiedIterationInputTokens, &au.UnclassifiedIterationOutputTokens,
			&au.UnclassifiedIterationInputTokens, nil, nil, &au.UnclassifiedIterationInputTokens,
		}
	}
	var cache5m, cache1h []byte
	if cacheCreation != nil {
		err = object(cacheCreation, func(name, value []byte) error {
			switch string(name) {
			case "ephemeral_5m_input_tokens":
				cache5m = value
			case "ephemeral_1h_input_tokens":
				cache1h = value
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	raws := [6][]byte{in, out, cacheCreate, cache5m, cache1h, cacheRead}
	for i, raw := range raws {
		if raw == nil {
			continue
		}
		c := parseCount(raw)
		if dst[i] != nil {
			addCount(dst[i], c)
		} else {
			recordCheckedCount(&au.ignoredMax, c)
		}
	}
	return nil
}

// openAIUsage is the usage block of one of the OpenAI APIs, read under the names of its family.
type openAIUsage struct {
	in, out, total, cached, written, written30m, written12h, reasoning count
}

// readOpenAIUsage reads the raw usage block v of family f, which may be nil (absent) or null. Chat completions
// name the counts prompt_tokens and completion_tokens with prompt_tokens_details and completion_tokens_details,
// the Responses API input_tokens and output_tokens with input_tokens_details and output_tokens_details. Only
// the Responses API reference defines cache_write_tokens_details, so it is read for that family alone.
func readOpenAIUsage(v []byte, f Family) (*openAIUsage, error) {
	if v == nil || isNull(v) {
		return nil, nil
	}
	inName, outName := nameInputTokens, nameOutputTokens
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
		case "total_tokens":
			ou.total = parseCount(value)
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
	if au.ignoredMax.n > maxTokens {
		return Usage{}, ErrOutOfRange
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
		Family:                               Anthropic,
		InputTokens:                          au.InputTokens.n,
		OutputTokens:                         au.OutputTokens.n,
		CacheCreationInputTokens:             au.CacheCreationInputTokens.n,
		CacheCreation5mInputTokens:           au.CacheCreation5m.n,
		CacheCreation1hInputTokens:           au.CacheCreation1h.n,
		CacheReadInputTokens:                 au.CacheReadInputTokens.n,
		ReasoningTokens:                      au.ThinkingTokens.n,
		CompactionInputTokens:                au.CompactionInputTokens.n,
		CompactionOutputTokens:               au.CompactionOutputTokens.n,
		CompactionCacheCreationInputTokens:   au.CompactionCacheCreationInputTokens.n,
		CompactionCacheCreation5mInputTokens: au.CompactionCacheCreation5m.n,
		CompactionCacheCreation1hInputTokens: au.CompactionCacheCreation1h.n,
		CompactionCacheReadInputTokens:       au.CompactionCacheReadInputTokens.n,
		AdvisorInputTokens:                   au.AdvisorInputTokens.n,
		AdvisorOutputTokens:                  au.AdvisorOutputTokens.n,
		AdvisorCacheCreationInputTokens:      au.AdvisorCacheCreationInputTokens.n,
		AdvisorCacheCreation5mInputTokens:    au.AdvisorCacheCreation5m.n,
		AdvisorCacheCreation1hInputTokens:    au.AdvisorCacheCreation1h.n,
		AdvisorCacheReadInputTokens:          au.AdvisorCacheReadInputTokens.n,
		UnclassifiedIterationInputTokens:     au.UnclassifiedIterationInputTokens.n,
		UnclassifiedIterationOutputTokens:    au.UnclassifiedIterationOutputTokens.n,
	}
}

// openAIOf converts a complete OpenAI usage block of family f. Input and output must both be present.
func openAIOf(ou *openAIUsage, f Family, maxTokens int64) (Usage, error) {
	if ou == nil {
		return Usage{}, ErrNoUsage
	}
	if err := firstErr(&ou.in, &ou.out, &ou.total, &ou.cached, &ou.written, &ou.written30m, &ou.written12h,
		&ou.reasoning); err != nil {
		return Usage{}, err
	}
	if !ou.in.set || !ou.out.set {
		return Usage{}, ErrNoUsage
	}
	var reportedTotal int64
	if ou.total.set && ou.total.n != 0 {
		reportedTotal = ou.total.n
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
		ReportedTotalTokens:       reportedTotal,
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

// checkRange checks that every count, the whole prompt, all output and the total are within [0, maxTokens].
func (u Usage) checkRange(maxTokens int64) error {
	for _, n := range [...]int64{u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens,
		u.CacheCreation5mInputTokens, u.CacheCreation1hInputTokens, u.CacheReadInputTokens, u.CachedPromptTokens,
		u.CacheWritePromptTokens, u.CacheWrite30mPromptTokens, u.CacheWrite12hPromptTokens, u.ReasoningTokens,
		u.CompactionInputTokens, u.CompactionOutputTokens, u.CompactionCacheCreationInputTokens,
		u.CompactionCacheCreation5mInputTokens, u.CompactionCacheCreation1hInputTokens,
		u.CompactionCacheReadInputTokens, u.AdvisorInputTokens, u.AdvisorOutputTokens,
		u.AdvisorCacheCreationInputTokens, u.AdvisorCacheCreation5mInputTokens, u.AdvisorCacheCreation1hInputTokens,
		u.AdvisorCacheReadInputTokens, u.UnclassifiedIterationInputTokens, u.UnclassifiedIterationOutputTokens,
		u.ReportedTotalTokens} {
		if n < 0 || n > maxTokens {
			return ErrOutOfRange
		}
	}
	// Each count is at most MaxTokensCeiling (1<<60). Checking each three-term Anthropic input group first
	// bounds every group by maxTokens (at most 1<<60), so summing all three groups in TotalInputTokens cannot
	// overflow int64. Checking TotalInputTokens and TotalOutputTokens next guarantees TotalTokens sums two
	// values at most MaxTokensCeiling.
	if u.Family == Anthropic {
		topIn := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
		compIn := u.CompactionInputTokens + u.CompactionCacheCreationInputTokens + u.CompactionCacheReadInputTokens
		advIn := u.AdvisorInputTokens + u.AdvisorCacheCreationInputTokens + u.AdvisorCacheReadInputTokens
		if topIn > maxTokens || compIn > maxTokens || advIn > maxTokens {
			return ErrOutOfRange
		}
	}
	if u.TotalInputTokens() > maxTokens || u.TotalOutputTokens() > maxTokens {
		return ErrOutOfRange
	}
	if u.TotalTokens() > maxTokens {
		return ErrOutOfRange
	}
	return nil
}

// checkParts checks the parts the references bound by their whole: the cache reads and writes within an
// OpenAI prompt, a positive OpenAI total_tokens against input plus output, and the reasoning or thinking
// tokens within the output. The cache write lifetimes are not checked (see Usage).
func (u Usage) checkParts() error {
	if u.Family != Anthropic {
		if u.CachedPromptTokens+u.CacheWritePromptTokens > u.InputTokens {
			return ErrOutOfRange
		}
		if u.ReportedTotalTokens > 0 && u.ReportedTotalTokens < u.TotalInputTokens()+u.TotalOutputTokens() {
			return ErrOutOfRange
		}
	}
	if u.ReasoningTokens > u.OutputTokens {
		return ErrOutOfRange
	}
	return nil
}
