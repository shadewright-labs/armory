package modelusage

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// The result classes of ParseStream.
const (
	classFinal    = "final"
	classTrailing = "trailing"
	classPartial  = "partial"
	classNone     = "none"
)

// resultClass names the class of a ParseStream result with a type switch on the error, as doc.go tells callers
// to, and fails the test when errors.As would give another answer.
func resultClass(t testing.TB, err error) string {
	t.Helper()
	var class string
	switch err.(type) { //nolint:errorlint // ParseStream returns the result types unwrapped; this checks it
	case nil:
		class = classFinal
	case *TrailingError:
		class = classTrailing
	case *PartialError:
		class = classPartial
	default:
		class = classNone
	}
	var (
		pe *PartialError
		te *TrailingError
	)
	asPartial, asTrailing := errors.As(err, &pe), errors.As(err, &te)
	if asPartial && asTrailing || asPartial != (class == classPartial) || asTrailing != (class == classTrailing) {
		t.Fatalf("err = %v: the type switch says %s, errors.As partial %v, trailing %v", err, class, asPartial,
			asTrailing)
	}
	return class
}

// Exact inputs that once lost or mislabelled usage the provider had reported.
const (
	rcAnthropicStart = "data: {\"type\":\"message_start\"," +
		"\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n"
	rcAnthropicStop  = "data: {\"type\":\"message_stop\"}\n\n"
	rcChatUsage      = "data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n"
	rcChatDone       = "data: [DONE]\n\n"
	rcResponsesFinal = "data: {\"type\":\"response.completed\"," +
		"\"response\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n"
	rcCached0 = "data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2," +
		"\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\n"
	rcCached7 = "data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2," +
		"\"prompt_tokens_details\":{\"cached_tokens\":7}}}\n\n"
)

// regressionCases are the exact inputs, with the outcome the stream rules define. capByOne sets MaxBytes one
// below the length of the input.
var regressionCases = []struct {
	name     string
	family   Family
	input    string
	capByOne bool
	total    int64
	class    string
	cause    error
}{
	// One byte after MaxBytes used to erase the final usage when the reader returned it with the rest. Every
	// event ends within the limit, so the usage is final and the extra byte is a trailing problem, whatever
	// the reader.
	{name: "byte cap anthropic", family: Anthropic, input: rcAnthropicStart + rcAnthropicStop + "x", capByOne: true,
		total: 11, class: classTrailing, cause: ErrTooLarge},
	{name: "byte cap chat", family: OpenAIChat, input: rcChatUsage + rcChatDone + "x", capByOne: true,
		total: 11, class: classTrailing, cause: ErrTooLarge},
	{name: "byte cap responses", family: OpenAIResponses, input: rcResponsesFinal + "x", capByOne: true,
		total: 11, class: classTrailing, cause: ErrTooLarge},
	// The final event without the blank line that ends it is not an event yet.
	{name: "final event ending in one line end", family: OpenAIResponses,
		input: strings.TrimSuffix(rcResponsesFinal, "\n"), class: classNone, cause: ErrTruncated},
	{name: "final event joined to a later data line", family: OpenAIResponses,
		input: strings.TrimSuffix(rcResponsesFinal, "\n") + "data: x\n\n", class: classNone, cause: ErrMalformed},
	// The end marker is exactly [DONE]: a vertical tab is part of the data, so this is not the marker.
	{name: "end marker after a vertical tab", family: OpenAIChat, input: rcChatUsage + "data: \v[DONE]\n\n",
		total: 11, class: classPartial, cause: ErrMalformed},
	{name: "end marker with a trailing space", family: OpenAIChat, input: rcChatUsage + "data: [DONE] \n\n",
		total: 11, class: classPartial, cause: ErrMalformed},
	{name: "end marker without the space", family: OpenAIChat, input: rcChatUsage + "data:[DONE]\n\n",
		total: 11, class: classFinal},
	// One usage chunk and no end: partial, and its uncached part is 9 of that block only.
	{name: "uncached prefix", family: OpenAIChat, input: rcCached0, total: 11, class: classPartial, cause: ErrTruncated},
	// A second chunk whose cached count grew is consistent: the latest block, still partial without [DONE].
	// Its uncached part fell from 9 to 2, which is why UncachedInputTokens is no lower bound.
	{name: "uncached extended", family: OpenAIChat, input: rcCached0 + rcCached7, total: 11, class: classPartial,
		cause: ErrTruncated},
	// Lower later counts used to be merged by maximum into 17, a total no block reported.
	{name: "lower anthropic counts", family: Anthropic, input: rcAnthropicStart +
		"data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":1,\"output_tokens\":8}}\n\n" + rcAnthropicStop,
		total: 9, class: classPartial, cause: ErrInconsistent},
	{name: "differing chat blocks", family: OpenAIChat, input: rcChatUsage +
		"data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":8}}\n\n" + rcChatDone,
		total: 9, class: classPartial, cause: ErrInconsistent},
	// Cumulative usage on every chunk, as continuous usage stats send it: growing blocks, then [DONE].
	{name: "continuous usage chat stream", family: OpenAIChat,
		input: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":0}}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":1," +
			"\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3," +
			"\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\n" + rcChatDone,
		total: 12, class: classFinal},
	{name: "chat block with one lower count", family: OpenAIChat, input: rcCached7 + rcCached0 + rcChatDone,
		total: 11, class: classPartial, cause: ErrInconsistent},
	{name: "repeated equal chat blocks", family: OpenAIChat, input: rcChatUsage + rcChatUsage + rcChatDone,
		total: 11, class: classFinal},
	// The usage of a chunk with an error member is taken, grown, before the error ends the stream.
	{name: "error chunk with updated usage", family: OpenAIChat, input: rcChatUsage +
		"data: {\"error\":{},\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":5}}\n\n",
		total: 14, class: classPartial, cause: ErrTruncated},
	{name: "error chunk with the only usage", family: OpenAIChat,
		input: "data: {\"error\":{\"message\":\"stopped\"},\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n",
		total: 11, class: classPartial, cause: ErrTruncated},
	// Chat completions do not use type, so a type of any kind is not read.
	{name: "chat type that is a number", family: OpenAIChat,
		input: "data: {\"type\":42,\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n" + rcChatDone,
		total: 11, class: classFinal},
	{name: "responses in progress event", family: OpenAIResponses,
		input: "data: {\"type\":\"response.in_progress\",\"response\":{\"status\":\"in_progress\"," +
			"\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n",
		class: classNone, cause: ErrTruncated},
	// A known event after message_stop is a trailing problem; the final usage stands.
	{name: "content block after message_stop", family: Anthropic, input: rcAnthropicStart + rcAnthropicStop +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		total: 11, class: classTrailing, cause: ErrMalformed},
	// An empty usage block carries no count, so no usage was seen.
	{name: "empty anthropic usage then the end", family: Anthropic,
		input: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{}}}\n\n", class: classNone, cause: ErrTruncated},
	{name: "second end marker", family: OpenAIChat, input: rcChatUsage + rcChatDone + rcChatDone,
		total: 11, class: classTrailing, cause: ErrMalformed},
	{name: "event after the end marker", family: OpenAIChat, input: rcChatUsage + rcChatDone + rcChatUsage,
		total: 11, class: classTrailing, cause: ErrMalformed},
}

func TestStreamRegressions(t *testing.T) {
	for _, tc := range regressionCases {
		lim := testLimits
		if tc.capByOne {
			lim.MaxBytes = int64(len(tc.input) - 1)
		}
		readers := map[string]func() io.Reader{
			"whole": func() io.Reader { return strings.NewReader(tc.input) },
			"one byte": func() io.Reader {
				return iotest.OneByteReader(strings.NewReader(tc.input))
			},
		}
		for rname, reader := range readers {
			t.Run(tc.name+"/"+rname, func(t *testing.T) {
				u, err := ParseStream(tc.family, reader(), lim)
				if got := resultClass(t, err); got != tc.class {
					t.Fatalf("class %s (err %v), want %s", got, err, tc.class)
				}
				if u.TotalTokens() != tc.total {
					t.Errorf("total %d (usage %+v), want %d", u.TotalTokens(), u, tc.total)
				}
				if (tc.cause == nil) != (err == nil) || tc.cause != nil && !errors.Is(err, tc.cause) {
					t.Errorf("err = %v, want %v", err, tc.cause)
				}
			})
		}
	}
}

// TestJSONRegressions holds the exact JSON replies. Three of them read as final although they describe a
// problem the package does not detect yet; doc.go lists them under Known limits.
func TestJSONRegressions(t *testing.T) {
	tests := []struct {
		name   string
		family Family
		body   string
		total  int64
		err    error
	}{
		{name: "exponent with a long mantissa", family: Anthropic,
			body: `{"usage":{"input_tokens":100000000000000000000e-20,"output_tokens":2}}`, err: ErrMalformed},
		{name: "fraction with a long mantissa", family: OpenAIChat,
			body: `{"usage":{"prompt_tokens":99999999999999999999.0,"completion_tokens":2}}`, err: ErrMalformed},
		{name: "integer beyond int64", family: OpenAIChat,
			body: `{"usage":{"prompt_tokens":99999999999999999999,"completion_tokens":2}}`, err: ErrOutOfRange},
		{name: "json byte order mark", family: Anthropic, body: "\ufeff{\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}",
			total: 11},
		// Known limit: the status is not read.
		{name: "responses reply in progress", family: OpenAIResponses,
			body: `{"status":"in_progress","usage":{"input_tokens":9,"output_tokens":2}}`, total: 11},
		// Known limit: total_tokens is not checked against its parts.
		{name: "chat total that contradicts its parts", family: OpenAIChat,
			body: `{"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":20}}`, total: 11},
		{name: "responses total that contradicts its parts", family: OpenAIResponses,
			body: `{"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":20}}`, total: 11},
		// Known limit: compaction iterations are not counted.
		{name: "anthropic compaction iterations", family: Anthropic,
			body: `{"usage":{"input_tokens":9,"output_tokens":2,"iterations":[{"type":"message","input_tokens":9,` +
				`"output_tokens":2},{"type":"compaction","input_tokens":8,"output_tokens":1}]}}`, total: 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := ParseJSON(tt.family, strings.NewReader(tt.body), testLimits)
			if (tt.err == nil) != (err == nil) || tt.err != nil && !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if u.TotalTokens() != tt.total {
				t.Errorf("total %d, want %d", u.TotalTokens(), tt.total)
			}
		})
	}
}

// TestReaderReturnsAResultType has a reader fail with the package's own *TrailingError after a usage chunk and
// before [DONE]. The result is partial, and neither a type switch nor errors.As may call it final.
func TestReaderReturnsAResultType(t *testing.T) {
	for _, readerErr := range []error{
		&TrailingError{Err: io.ErrUnexpectedEOF},
		&PartialError{Err: &TrailingError{Err: io.ErrUnexpectedEOF}},
	} {
		r := io.MultiReader(strings.NewReader(rcChatUsage), iotest.ErrReader(readerErr))
		u, err := ParseStream(OpenAIChat, r, testLimits)
		if got := resultClass(t, err); got != classPartial {
			t.Fatalf("class %s (err %v), want partial", got, err)
		}
		if _, ok := errors.AsType[*TrailingError](err); ok {
			t.Errorf("errors.As finds a *TrailingError in %v", err)
		}
		if u.TotalTokens() != 11 || !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, ErrTruncated) {
			t.Errorf("usage %+v, err %v: want 11 tokens matching io.ErrUnexpectedEOF and ErrTruncated", u, err)
		}
		// The opaque cause matches sentinels, never a result type of this package, even the reader's own value.
		if errors.Is(err, readerErr) {
			t.Errorf("err = %v matches the reader's %T value", err, readerErr)
		}
	}
	// Before any usage the reader's error is the cause alone; it is still no result type.
	_, err := ParseStream(OpenAIChat, iotest.ErrReader(&PartialError{Err: io.EOF}), testLimits)
	if got := resultClass(t, err); got != classNone {
		t.Errorf("class %s (err %v), want none", got, err)
	}
}

func TestErrorText(t *testing.T) {
	for _, err := range []error{new(PartialError), new(TrailingError), &PartialError{Err: ErrTruncated},
		&TrailingError{Err: ErrMalformed}} {
		if text := err.Error(); text == "" || !strings.HasPrefix(text, "modelusage: ") {
			t.Errorf("%T: text %q", err, text)
		}
	}
	long := "detail\x1b[31m\n" + strings.Repeat("y", 500)
	_, err := ParseStream(OpenAIChat, io.MultiReader(strings.NewReader(rcChatUsage),
		iotest.ErrReader(errors.New(long))), testLimits)
	text := err.Error()
	if strings.ContainsAny(text, "\x1b\n") || strings.Contains(text, strings.Repeat("y", maxReaderText)) {
		t.Errorf("reader text copied into the message: %q", text)
	}
	if len(text) > 200 {
		t.Errorf("message of %d bytes, want a bounded one", len(text))
	}
	if !strings.Contains(text, "detail?[31m?") {
		t.Errorf("message %q lacks the sanitised start of the reader's text", text)
	}
}
