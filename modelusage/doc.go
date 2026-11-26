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
// reads is [ErrMalformed]. Unknown fields and unknown stream events are ignored. A count that is not an
// integer (a fraction, an exponent, a string) is [ErrMalformed] whatever its magnitude, so 1e2 and
// 100000000000000000000e-20 are both malformed, never out of range. A count that is negative, above
// [Limits.MaxTokens], or inconsistent with the other counts (more cached tokens than input tokens, for
// example) is [ErrOutOfRange], never a number. Error text is fixed and bounded: it never
// quotes the reply, and a reader's error text is cut to a short length with every byte that is not printable
// ASCII replaced.
//
// # Result classes
//
// ParseJSON returns a Usage with a nil error, or a zero Usage with an error. ParseStream returns one of three
// classes, and only these three:
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
// errors.As gives the same answer, because the cause inside either type never holds the other: a reader's
// error is kept as an opaque cause that errors.Is matches (io.ErrUnexpectedEOF, a context error) but
// errors.As does not unwrap. Both types unwrap to their cause, so errors.Is matches [ErrTruncated],
// [ErrMalformed], [ErrInconsistent], [ErrOutOfRange], [ErrNoUsage], [ErrTooLarge], [ErrLineTooLong] or the
// reader's error.
//
// # Streams
//
// Limits apply to the bytes consumed, so the result never depends on how a reader splits its input: an event
// that ends within [Limits.MaxBytes] is handled even when the reader already returned bytes past the limit.
// An event counts only once the blank line that ends it was read, and the chat end marker is data whose whole
// text is [DONE]. The parse stops at the first problem; a problem after the final event leaves the final usage
// exact and is reported as a *TrailingError.
//
// Counts accumulate by the rule of each family. Anthropic: each count takes the latest value a block reports,
// and a lower value than before is a contradiction. Chat completions: one usage chunk is the contract; with
// several, the latest complete block wins as a whole, and a block that differs from the one before is a
// contradiction. Responses: only the block of the final event counts. A contradiction stops the parse with
// [ErrInconsistent] inside a *PartialError and the latest values.
//
// What a partial result bounds: unless the error matches ErrInconsistent, every count, and so TotalInputTokens
// and TotalTokens, is at least what ParseStream returns for any shorter part of the same stream cut after an
// event. UncachedInputTokens is a difference and is not a lower bound, since a later report can raise the
// cached part of an unchanged prompt.
//
// # Known limits
//
//   - A total_tokens member that contradicts the input and output counts is not read or checked: the counts
//     are taken as reported.
//   - ParseJSON does not read the status of a Responses reply, so a complete JSON body whose status is
//     in_progress is read as final.
//   - Anthropic beta compaction usage, reported apart from the top-level counts (usage.iterations entries of
//     type compaction), is not counted, so a reply that used compaction is under-counted.
//
// The package does not make HTTP requests, price tokens, know model names or log anything. Choosing the
// family from a request path or a provider setting, and what to charge when the usage cannot be read, stay
// with the caller.
package modelusage
