# modelusage

Package `modelusage` (`github.com/shadewright-labs/armory/modelusage`) reads the token usage a model provider reports in a reply. It reads an `io.Reader` within caller-set bounds and returns a `Usage` with the counts as the provider reported them. It makes no HTTP requests, knows no model names, does not price tokens and does not log.

## Families

Three `Family` constants select the reply format:

- `Anthropic` (`"anthropic"`): Anthropic Messages API (`POST /v1/messages`). `Usage.InputTokens` counts only executor input after the last cache breakpoint; `CacheCreationInputTokens` and `CacheReadInputTokens` are separate. Compaction entries (`usage.iterations` items with `"type": "compaction"`) and advisor entries (`"type": "advisor_message"`, kept apart because they bill at the advisor model's rate) are reported apart from top-level counts and summed into the `Compaction*` and `Advisor*` fields (input, output, cache creation, cache read, and the 5m/1h cache write lifetimes). `message` entries have their counts validated without being added again because they are already in top-level counts. Iteration entries the package does not classify (`fallback_message`, and entries with a null, missing or unknown type) have their counts validated and summed into `UnclassifiedIterationInputTokens` (all input-side counts including cache creation and cache read) and `UnclassifiedIterationOutputTokens`. Those two fields are not part of `TotalInputTokens`, `TotalOutputTokens` or `TotalTokens` because an unknown entry may repeat counts already in the top-level usage, keeping the totals a lower bound; a caller enforcing a budget should add them.
- `OpenAIChat` (`"openai-chat"`): OpenAI chat completions API (`POST /v1/chat/completions`) and compatible servers. `Usage.InputTokens` (from `prompt_tokens`) is the whole prompt; `CachedPromptTokens` and `CacheWritePromptTokens` are parts of it. When `total_tokens` is present and non-zero, it is stored in `ReportedTotalTokens`.
- `OpenAIResponses` (`"openai-responses"`): OpenAI Responses API (`POST /v1/responses`). `Usage.InputTokens` is the whole prompt; `CachedPromptTokens` and `CacheWritePromptTokens` (with optional `CacheWrite30mPromptTokens` and `CacheWrite12hPromptTokens`) are parts of it. When `total_tokens` is present and non-zero, it is stored in `ReportedTotalTokens`.

In all three families, `Usage.ReasoningTokens` is part of `Usage.OutputTokens`. Call `Usage.TotalInputTokens`, `Usage.TotalOutputTokens`, `Usage.CacheReadTokens`, `Usage.CacheWriteTokens`, `Usage.UncachedInputTokens` and `Usage.TotalTokens` so cached, compaction and advisor tokens are counted once.

For `Anthropic` replies, `TotalInputTokens`, `TotalOutputTokens`, `TotalTokens`, `CacheReadTokens`, `CacheWriteTokens` and `UncachedInputTokens` include compaction and advisor iteration usage in addition to top-level counts, and exclude `UnclassifiedIterationInputTokens` and `UnclassifiedIterationOutputTokens` (callers enforcing a budget should add them). `OutputTokens` remains the top-level executor output count only: a caller pricing from `OutputTokens` directly must add `CompactionOutputTokens` and `AdvisorOutputTokens` (billing advisor tokens at the advisor model's rate when priced apart), or call `TotalOutputTokens`. A cache-write lifetime field without its parent cache-write count adds nothing to totals because the parent count is never inferred from lifetimes; this rule covers the top-level lifetime fields as well as the `Compaction*` and `Advisor*` cache-lifetime fields. Always build `Usage` literals with keyed fields, because new fields may be added in minor versions.

## Entry points

- `ParseJSON(f Family, r io.Reader, lim Limits) (Usage, error)` reads a complete JSON object with a top-level `usage` block. For `OpenAIResponses`, a present non-null `status` member must be terminal (`completed`, `incomplete`, `failed` or `cancelled`); non-terminal statuses (`in_progress`, `queued`) and any unknown status string fail closed with `ErrTruncated`, even when `usage` is null or absent (a change of error class from `ErrNoUsage` so callers do not treat an unfinished reply as final). It returns a `Usage` with a nil error, or a zero `Usage` with an error.
- `ParseStream(f Family, r io.Reader, lim Limits) (Usage, error)` reads a server-sent events stream. Final events are `message_stop` (`Anthropic`), `data: [DONE]` (`OpenAIChat`) and `response.completed`, `response.incomplete` or `response.failed` (`OpenAIResponses`, also checking `response.status` when present).

## Limits

The caller sets the bounds in a `Limits` value. A limit a call uses must be positive, and an out-of-range limit or an unknown `Family` gives `ErrInvalidLimits`:

- `Limits.MaxBytes`: the total bytes read. Past it the error is a `*LimitError` that matches `ErrTooLarge`.
- `Limits.MaxLineBytes` (`ParseStream` only): one line without its line ending, and the joined `data` of one event. Past it the error is a `*LimitError` that matches `ErrLineTooLong`.
- `Limits.MaxTokens` (at most `MaxTokensCeiling`): every count and the totals. A negative count, one above `MaxTokens`, or a positive OpenAI `total_tokens` smaller than input plus output is `ErrOutOfRange`.

Bad syntax and a count that is not an integer are `ErrMalformed`. A complete JSON reply with no usage, or without its input or output count, is `ErrNoUsage`. Parser error text never quotes the reply. In a stream, any of these errors can arrive inside a `*PartialError` or `*TrailingError`, as the next section explains. For OpenAI families, a `total_tokens` of `0` next to non-zero counts is treated as absent (`ReportedTotalTokens` stays `0`), and a `total_tokens` larger than input plus output (within `MaxTokens`) is accepted and kept in `ReportedTotalTokens`. `Usage.TotalTokens` returns the larger of `TotalInputTokens() + TotalOutputTokens()` and `ReportedTotalTokens`, so it can exceed `TotalInputTokens() + TotalOutputTokens()` when the provider reports a higher total; `TotalInputTokens` and `TotalOutputTokens` stay unchanged. The package comment in the source states every rule exactly.

## Stream result classes

`ParseStream` returns `*TrailingError` and `*PartialError` unwrapped, separating three classes via a type switch or `errors.As`:

- Final (`nil` or `*TrailingError`): the stream reached its final event and `Usage` is exact. `*TrailingError` reports a trailing problem for logging.
- Partial (`*PartialError`): at least one usage block with a count was accepted before the stream ended or failed. `Usage` is the usage so far, not final. Unless `errors.Is(err, ErrInconsistent)`, `TotalInputTokens` and `TotalOutputTokens` (and every individual count except `ReportedTotalTokens`, which is compared in chat streams only when non-zero) are lower bounds for that `Limits` value; `UncachedInputTokens` is not.
- None (any other error, zero `Usage`): no usage block with a count was accepted.

Both types unwrap to their cause, so `errors.Is` finds the sentinel error inside.
