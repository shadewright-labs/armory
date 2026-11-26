// Package modelusage reads the token usage a model provider reports in a reply.
//
// It covers three reply formats, each a [Family]: the Anthropic Messages API, OpenAI chat completions and
// the OpenAI Responses API. [ParseJSON] reads a complete JSON reply and [ParseStream] a streamed reply sent
// as server-sent events. Both read an [io.Reader] within the byte bounds of a [Limits] value the caller
// sets, and return a [Usage] holding the counts as the provider reported them. [Usage] documents how the
// counts of each family add up, so that a token is never counted twice and a cached token is never missed.
//
// Reading is bounded. A reply longer than [Limits.MaxBytes], or a stream line longer than
// [Limits.MaxLineBytes], is a [*LimitError]. Malformed input is an error, never a panic. Unknown fields and
// unknown stream events are ignored. A count that is negative, not an integer, above [Limits.MaxTokens], or
// inconsistent with the other counts (more cached tokens than input tokens, for example) is
// [ErrOutOfRange], never a number. Error text is fixed and never quotes the reply.
//
// The package does not make HTTP requests, price tokens, know model names or log anything. Choosing the
// family from a request path or a provider setting, and what to charge when the usage cannot be read, stay
// with the caller.
package modelusage
