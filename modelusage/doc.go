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
// reads is [ErrMalformed].
// Unknown fields and unknown stream events are ignored. A count that is not an integer (a fraction, an
// exponent, a string) is [ErrMalformed]. A count that is negative, above [Limits.MaxTokens], or
// inconsistent with the other counts (more cached tokens than input tokens, for example) is
// [ErrOutOfRange], never a number. Error text is fixed and never quotes the reply.
//
// Which errors come with a usable Usage:
//
//   - nil: the Usage is final.
//   - [*TrailingError] (ParseStream only): the stream sent its final event and then something not allowed
//     after it. The Usage is final, the same as with a nil error; the error is for logging.
//   - [*PartialError] (ParseStream only): the stream yielded at least one usage block, then stopped or broke
//     before its final event. The Usage is never zero and is a lower bound: every count is at least what the
//     stream reported before the problem, and nothing after a usage block can lower it. It can be lower than
//     what the provider bills; never treat it as final.
//
// Every other error comes with a zero Usage: ParseJSON returns a zero Usage with every error, and ParseStream
// does when no usage block was read. Both wrapper types unwrap to the cause, so errors.Is still matches
// [ErrTruncated], [ErrMalformed], [ErrOutOfRange], [ErrNoUsage], [ErrTooLarge], [ErrLineTooLong] or the
// reader's error. A caller can therefore write:
//
//	u, err := modelusage.ParseStream(f, body, lim)
//	var partial *modelusage.PartialError
//	var trailing *modelusage.TrailingError
//	switch {
//	case err == nil, errors.As(err, &trailing):
//		// u is final
//	case errors.As(err, &partial):
//		// u is a lower bound
//	default:
//		// no usage was read
//	}
//
// The package does not make HTTP requests, price tokens, know model names or log anything. Choosing the
// family from a request path or a provider setting, and what to charge when the usage cannot be read, stay
// with the caller.
package modelusage
