# modelusage

Package `modelusage` (`github.com/shadewright-labs/armory/modelusage`) reads the token usage a model provider reports in a reply. It reads an `io.Reader` within caller-set bounds and returns a `Usage` with the counts as the provider reported them. It makes no HTTP requests, knows no model names, does not price tokens and does not log.

## Families

Three `Family` constants select the reply format:

- `Anthropic` (`"anthropic"`): Anthropic Messages API (`POST /v1/messages`). `Usage.InputTokens` counts only input after the last cache breakpoint; `CacheCreationInputTokens` and `CacheReadInputTokens` are separate.
- `OpenAIChat` (`"openai-chat"`): OpenAI chat completions API (`POST /v1/chat/completions`) and compatible servers. `Usage.InputTokens` (from `prompt_tokens`) is the whole prompt; `CachedPromptTokens` and `CacheWritePromptTokens` are parts of it.
- `OpenAIResponses` (`"openai-responses"`): OpenAI Responses API (`POST /v1/responses`). `Usage.InputTokens` is the whole prompt; `CachedPromptTokens` and `CacheWritePromptTokens` (with optional `CacheWrite30mPromptTokens` and `CacheWrite12hPromptTokens`) are parts of it.

In all three families, `Usage.ReasoningTokens` is part of `Usage.OutputTokens`. Call `Usage.TotalInputTokens`, `Usage.CacheReadTokens`, `Usage.CacheWriteTokens`, `Usage.UncachedInputTokens` and `Usage.TotalTokens` so cached tokens are counted once.

## Entry points

- `ParseJSON(f Family, r io.Reader, lim Limits) (Usage, error)` reads a complete JSON object with a top-level `usage` block. It returns a `Usage` with a nil error, or a zero `Usage` with an error.
- `ParseStream(f Family, r io.Reader, lim Limits) (Usage, error)` reads a server-sent events stream. Final events are `message_stop` (`Anthropic`), `data: [DONE]` (`OpenAIChat`) and `response.completed`, `response.incomplete` or `response.failed` (`OpenAIResponses`).

## Limits

Every field of `Limits` must be positive; an out-of-range limit or unknown `Family` returns `ErrInvalidLimits`:

- `Limits.MaxBytes` (`int64`, below `math.MaxInt64`): total bytes read. Exceeding it returns `*LimitError` matching `ErrTooLarge`.
- `Limits.MaxLineBytes` (`int`, at most `math.MaxInt-5`, `ParseStream` only): one line without its line ending, and the joined `data` of one event. Exceeding it returns `*LimitError` matching `ErrLineTooLong`.
- `Limits.MaxTokens` (`int64`, at most `MaxTokensCeiling`, `1 << 60`): every count, `TotalInputTokens` and `TotalTokens`. Negative counts, counts above `MaxTokens` or breakdowns exceeding their parent return `ErrOutOfRange`.

Non-integer counts, duplicate keys and bad syntax return `ErrMalformed`. Missing usage or missing input/output counts return `ErrNoUsage`. Error text is fixed and bounded and never quotes the reply.

## Stream result classes

`ParseStream` returns `*TrailingError` and `*PartialError` unwrapped, separating three classes via a type switch or `errors.As`:

- Final (`nil` or `*TrailingError`): the stream reached its final event and `Usage` is exact. `*TrailingError` reports a trailing problem for logging.
- Partial (`*PartialError`): at least one usage block with a count was accepted before the stream ended or failed. `Usage` is the usage so far, not final. Unless `errors.Is(err, ErrInconsistent)`, `TotalInputTokens` and `TotalTokens` are lower bounds for that `Limits` value; `UncachedInputTokens` is not.
- None (any other error, zero `Usage`): no usage block with a count was accepted.

Both `*TrailingError` and `*PartialError` unwrap to their cause (`ErrTruncated`, `ErrMalformed`, `ErrInconsistent`, `ErrOutOfRange`, `ErrNoUsage`, `ErrTooLarge`, `ErrLineTooLong` or the reader's error via `errors.Is`).
