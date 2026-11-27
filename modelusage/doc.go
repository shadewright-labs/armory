// Package modelusage reads the token usage a model provider reports in a reply.
//
// It covers three reply formats, each a [Family]: the Anthropic Messages API, OpenAI chat completions and
// the OpenAI Responses API. [ParseJSON] reads a complete JSON reply and [ParseStream] a streamed reply sent
// as server-sent events. Both read an [io.Reader] within the byte bounds of a [Limits] value the caller
// sets, and return a [Usage] holding the counts as the provider reported them. [Usage] documents how the
// counts of each family add up, so that a token is never counted twice and a cached token is never missed.
//
// Reading is bounded. A reply longer than [Limits.MaxBytes], or a stream line or the joined data of one
// stream event longer than [Limits.MaxLineBytes], is a [*LimitError]. Malformed input is an error, never a
// panic. Member names match exactly, case included, and a name that appears twice in an object the parser
// reads is [ErrMalformed]. Unknown fields and unknown stream events are ignored. Counts in every Anthropic
// usage.iterations entry are validated the same way: compaction and advisor_message entries sum into the
// Compaction* and Advisor* fields, message entries are validated without adding again (already in top-level
// counts), and entries the package does not classify (fallback_message, and entries with a null, missing or
// unknown type) sum into [Usage.UnclassifiedIterationInputTokens] (all input-side counts including cache
// creation and cache read) and [Usage.UnclassifiedIterationOutputTokens]. A count that is not an integer (a
// fraction, an exponent, a string) is [ErrMalformed] whatever its magnitude, so 1e2 and
// 100000000000000000000e-20 are both malformed, never out of range. A count that is negative, above
// [Limits.MaxTokens], or inconsistent with the other counts (more cached tokens than input tokens, or a
// positive OpenAI total_tokens smaller than input plus output, for example) is [ErrOutOfRange], never a
// number. Error text is fixed and bounded: it never quotes the reply, and a reader's error text is cut to a
// short length with every byte that is not printable ASCII replaced.
//
// For [OpenAIResponses], a present non-null status member must be one of the terminal statuses (completed,
// incomplete, failed or cancelled). Non-terminal statuses (in_progress, queued) and any unknown status string
// fail closed with [ErrTruncated]. Status is checked before usage presence, so a non-terminal or unknown
// status with usage null or absent returns [ErrTruncated] instead of [ErrNoUsage] (a change of error class
// so callers never mistake an unfinished reply for a final reply missing usage). A non-string status is
// [ErrMalformed].
//
// # Result classes
//
// [ParseJSON] returns a [Usage] with a nil error, or a zero [Usage] with an error. [ParseStream] returns one
// of three classes, and only these three:
//
//   - final: a nil error, or a [*TrailingError]. The stream reached its final event; the Usage is exactly the
//     usage at that event. A *TrailingError says that something was wrong after it, for logging.
//   - partial: a [*PartialError]. At least one usage block with a count was accepted, then the stream ended
//     or broke before its final event. The Usage is the usage so far; never treat it as final.
//   - none: any other error, with a zero Usage. No usage block with a count was accepted.
//
// ParseStream returns the two error types themselves, never wrapped, so a type switch on the error tells the
// classes apart:
//
//	u, err := modelusage.ParseStream(f, body, lim)
//	switch err.(type) {
//	case nil, *modelusage.TrailingError:
//		// u is final
//	case *modelusage.PartialError:
//		// u is the usage so far: price it by u.TotalTokens(), never as final
//	default:
//		// no usage was read
//	}
//
// [errors.As] gives the same answer, because the cause inside either type never holds the other: a reader's
// error is kept as an opaque cause that [errors.Is] matches (the reader's error value itself, by identity, and
// everything in its chain, such as [io.ErrUnexpectedEOF] or a context error) but errors.As does not unwrap, so
// errors.As never finds a *PartialError or *TrailingError the reader returned. Every method of the package's
// error types is safe on a nil value. Both types unwrap to their cause, so errors.Is matches [ErrTruncated],
// [ErrMalformed], [ErrInconsistent], [ErrOutOfRange], [ErrNoUsage], [ErrTooLarge], [ErrLineTooLong] or the
// reader's error.
//
// # Readers
//
// Only an error that is [io.EOF] itself ends the input cleanly. Any other error, including one that wraps or
// joins io.EOF, and a typed nil error value, is a reader failure, reported after every byte the reader
// returned before it. A reader that returns no byte and no error 100 times in a row fails with
// [io.ErrNoProgress], as [bufio] does; the result class then follows the usual rules, so usage read before it
// is partial.
//
// JSON nesting has no depth limit of its own: values are checked and skipped without recursion, one byte of
// memory per open object or array, so the byte limits bound the depth. [encoding/json] alone would refuse
// nesting past 10,000 levels.
//
// # Streams
//
// Limits apply to the bytes consumed, so the result never depends on how a reader splits its input: an event
// that ends within [Limits.MaxBytes] is handled even when the reader already returned bytes past the limit.
// An event counts only once the blank line that ends it was read, and the chat end marker is data whose whole
// text is [DONE]. The parse stops at the first problem; a problem after the final event leaves the final usage
// exact and is reported as a [*TrailingError].
//
// Counts accumulate by the rule of each family. [Anthropic]: each count takes the latest value a block
// reports, and only a value lower than before is a contradiction (including a present non-null
// usage.iterations array that omits a previously reported iteration category or sends an empty array, dropping
// its sum to zero). Chat completions ([OpenAIChat]): the latest complete block wins as a whole; a block with
// no count lower than in the block before is consistent (servers that send cumulative usage on every chunk
// send growing blocks, and ReportedTotalTokens is compared only when the later block carries a non-zero
// total_tokens), and only a block with any lower count is a contradiction. Responses ([OpenAIResponses]): only
// the block of the final event counts. A contradiction stops the parse with [ErrInconsistent] inside a
// [*PartialError] and the latest values.
//
// What a partial result bounds: unless the error matches [ErrInconsistent], every count except
// [Usage.ReportedTotalTokens], and so [Usage.TotalInputTokens] and [Usage.TotalOutputTokens], is at least what
// [ParseStream] returns, under the same [Limits], for any shorter part of the same stream cut after an event.
// In [OpenAIChat] streams, ReportedTotalTokens (and [Usage.TotalTokens] when an earlier chunk reported a
// higher total) is also a lower bound whenever the later chunk carries a non-zero total_tokens; a later chunk
// without total_tokens leaves ReportedTotalTokens zero without triggering ErrInconsistent. The rule holds for
// the same Limits only: a larger [Limits.MaxTokens] can accept a block that a smaller one refuses.
// [Usage.UncachedInputTokens] is a difference and is not a lower bound, since a later report can raise the
// cached part of an unchanged prompt.
//
// # Method semantics and struct literals
//
// For [Anthropic] replies, [Usage.TotalInputTokens], [Usage.TotalOutputTokens], [Usage.TotalTokens],
// [Usage.CacheReadTokens], [Usage.CacheWriteTokens] and [Usage.UncachedInputTokens] include both compaction
// and advisor iteration usage on top of the top-level counts. [Usage.OutputTokens] still holds only the
// top-level executor output count: a caller pricing from OutputTokens directly must add
// [Usage.CompactionOutputTokens] and [Usage.AdvisorOutputTokens] (billing advisor tokens at the advisor
// model's rate when priced apart), or call [Usage.TotalOutputTokens]. [Usage.UnclassifiedIterationInputTokens]
// and [Usage.UnclassifiedIterationOutputTokens] are not included in TotalInputTokens, TotalOutputTokens or
// TotalTokens, because an unknown iteration entry may repeat counts already in the top-level usage (as message
// entries do) and the totals stay a lower bound; callers enforcing a budget should add those two fields.
//
// [Usage.TotalTokens] returns the larger of TotalInputTokens() + TotalOutputTokens() and
// [Usage.ReportedTotalTokens]. TotalInputTokens() and TotalOutputTokens() are unchanged, so TotalTokens() can
// exceed their sum when an OpenAI or compatible server reports a higher total_tokens.
//
// Cache-write lifetime fields without their parent cache-write count add nothing to totals, because the parent
// count is never inferred from lifetimes. This rule covers the top-level fields (CacheCreation5mInputTokens,
// CacheCreation1hInputTokens, CacheWrite30mPromptTokens, CacheWrite12hPromptTokens) as well as the Compaction*
// and Advisor* cache-lifetime fields.
//
// Always construct [Usage] values with keyed fields, because new fields may be added in minor versions.
//
// # Known limits
//
//   - For [Anthropic], usage.iterations entries that the package does not classify (fallback_message, and
//     entries with a null, missing or unknown type) have their counts validated and summed into
//     [Usage.UnclassifiedIterationInputTokens] (all input-side counts including cache counts) and
//     [Usage.UnclassifiedIterationOutputTokens]. They are excluded from TotalInputTokens, TotalOutputTokens
//     and TotalTokens because Anthropic's documentation does not say whether top-level counts already include
//     fallback_message or future iteration types; callers enforcing a budget should add them.
//   - For [OpenAIChat] and [OpenAIResponses], a total_tokens member that is negative, overflows int64,
//     exceeds [Limits.MaxTokens] or is positive and smaller than input plus output is [ErrOutOfRange] (and a
//     non-integer is [ErrMalformed]). A total_tokens of 0 next to non-zero counts is treated as absent, leaving
//     [Usage.ReportedTotalTokens] zero. A total_tokens larger than input plus output within [Limits.MaxTokens]
//     is accepted and kept in [Usage.ReportedTotalTokens]; [Usage.TotalTokens] returns the larger of
//     TotalInputTokens() + TotalOutputTokens() and ReportedTotalTokens, while TotalInputTokens() and
//     TotalOutputTokens() stay unchanged.
//
// The package does not make HTTP requests, price tokens, know model names or log anything. Choosing the
// family from a request path or a provider setting, and what to charge when the usage cannot be read, stay
// with the caller.
package modelusage
