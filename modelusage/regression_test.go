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
	// Chat chunk without total_tokens after one with total_tokens is consistent; a lower non-zero total is not.
	{name: "chat chunk without total after chunk with total", family: OpenAIChat,
		input: "data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2,\"total_tokens\":15}}\n\n" +
			rcChatUsage + rcChatDone,
		total: 11, class: classFinal},
	{name: "chat chunk with lower non-zero total", family: OpenAIChat,
		input: "data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2,\"total_tokens\":20}}\n\n" +
			"data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" + rcChatDone,
		total: 15, class: classPartial, cause: ErrInconsistent},
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

// TestJSONRegressions holds the exact JSON replies for the fixed limits and the remaining total_tokens
// upper-bound limit documented under Known limits in doc.go.
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
		// Issue 25: non-terminal Responses statuses are refused as truncated; terminal ones and absent status pass.
		{name: "responses reply in progress", family: OpenAIResponses,
			body: `{"status":"in_progress","usage":{"input_tokens":9,"output_tokens":2}}`, err: ErrTruncated},
		{name: "responses reply queued", family: OpenAIResponses,
			body: `{"status":"queued","usage":{"input_tokens":9,"output_tokens":2}}`, err: ErrTruncated},
		{name: "responses reply unknown status", family: OpenAIResponses,
			body: `{"status":"pending","usage":{"input_tokens":9,"output_tokens":2}}`, err: ErrTruncated},
		{name: "responses reply empty status", family: OpenAIResponses,
			body: `{"status":"","usage":{"input_tokens":9,"output_tokens":2}}`, err: ErrTruncated},
		{name: "responses reply non-string status", family: OpenAIResponses,
			body: `{"status":123,"usage":{"input_tokens":9,"output_tokens":2}}`, err: ErrMalformed},
		{name: "responses reply cancelled", family: OpenAIResponses,
			body: `{"status":"cancelled","usage":{"input_tokens":9,"output_tokens":2}}`, total: 11},
		{name: "responses reply completed", family: OpenAIResponses,
			body: `{"status":"completed","usage":{"input_tokens":9,"output_tokens":2}}`, total: 11},
		{name: "responses reply incomplete", family: OpenAIResponses,
			body: `{"status":"incomplete","usage":{"input_tokens":9,"output_tokens":2}}`, total: 11},
		{name: "responses reply failed", family: OpenAIResponses,
			body: `{"status":"failed","usage":{"input_tokens":9,"output_tokens":2}}`, total: 11},
		// Issue 24: total_tokens below input+output, negative, overflow or non-integer is refused;
		// total_tokens of 0 next to non-zero parts is treated as absent; total_tokens above the sum is kept in
		// ReportedTotalTokens.
		{name: "chat total smaller than its parts", family: OpenAIChat,
			body: `{"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":10}}`, err: ErrOutOfRange},
		{name: "responses total smaller than its parts", family: OpenAIResponses,
			body: `{"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":10}}`, err: ErrOutOfRange},
		{name: "chat total zero next to non-zero parts", family: OpenAIChat,
			body: `{"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":0}}`, total: 11},
		{name: "responses total zero next to non-zero parts", family: OpenAIResponses,
			body: `{"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":0}}`, total: 11},
		{name: "chat total negative", family: OpenAIChat,
			body: `{"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":-1}}`, err: ErrOutOfRange},
		{name: "responses total overflow", family: OpenAIResponses,
			body: `{"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":99999999999999999999}}`, err: ErrOutOfRange},
		{name: "chat total not an integer", family: OpenAIChat,
			body: `{"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11.5}}`, err: ErrMalformed},
		{name: "chat total larger than its parts", family: OpenAIChat,
			body: `{"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":20}}`, total: 20},
		{name: "responses total larger than its parts", family: OpenAIResponses,
			body: `{"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":20}}`, total: 20},
		// Issue 26: Anthropic compaction and advisor_message iterations are counted; message, fallback_message
		// and unknown entries are not double-counted.
		{name: "anthropic compaction iterations", family: Anthropic,
			body: `{"usage":{"input_tokens":9,"output_tokens":2,"iterations":[{"type":"message","input_tokens":9,` +
				`"output_tokens":2},{"type":"compaction","input_tokens":8,"output_tokens":1}]}}`, total: 20},
		{name: "anthropic on-demand compaction", family: Anthropic,
			body: `{"usage":{"input_tokens":0,"output_tokens":0,"iterations":[{"type":"compaction",` +
				`"input_tokens":144,"output_tokens":276}]}}`, total: 420},
		{name: "anthropic advisor documented example", family: Anthropic,
			body: `{"usage":{"input_tokens":1760,"output_tokens":531,"cache_read_input_tokens":412,"iterations":[` +
				`{"type":"advisor_message","input_tokens":823,"output_tokens":1612}]}}`, total: 5138},
		{name: "anthropic unknown iteration type kept outside total", family: Anthropic,
			body: `{"usage":{"input_tokens":1,"output_tokens":1,"iterations":[{"type":"advisor_message_v2",` +
				`"input_tokens":5000,"output_tokens":5000}]}}`, total: 2},
		{name: "anthropic compaction with fallback and unknown entry types", family: Anthropic,
			body: `{"usage":{"input_tokens":9,"output_tokens":2,"iterations":[{"type":"compaction",` +
				`"input_tokens":8,"output_tokens":1},{"type":"fallback_message","input_tokens":50,"output_tokens":10},` +
				`{"type":"future_type","input_tokens":99,"output_tokens":99}]}}`, total: 20},
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
		// The opaque cause matches the reader's own error value by identity, while errors.As stays false.
		if !errors.Is(err, readerErr) {
			t.Errorf("err = %v does not match the reader's %T value", err, readerErr)
		}
	}
	// Before any usage the reader's error is the cause alone; it is still no result type.
	_, err := ParseStream(OpenAIChat, iotest.ErrReader(&PartialError{Err: io.EOF}), testLimits)
	if got := resultClass(t, err); got != classNone {
		t.Errorf("class %s (err %v), want none", got, err)
	}
}

func TestErrorText(t *testing.T) {
	if strings.Contains(ErrTruncated.Error(), "stream") {
		t.Errorf("ErrTruncated text %q mentions stream only, but ParseJSON returns it too", ErrTruncated.Error())
	}
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

// Inputs of the reader and framing regressions below.
const (
	rfBlock = "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n"
	rfGrown = "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2}}\n\n"
	rfDone  = "data: [DONE]\n\n"
)

var rfLimits = Limits{MaxBytes: 4096, MaxLineBytes: 1024, MaxTokens: 100}

// emptyReads returns (0, nil) n times, then all of data in one read, then io.EOF.
type emptyReads struct {
	n    int
	data string
	done bool
}

func (e *emptyReads) Read(p []byte) (int, error) {
	if e.n > 0 {
		e.n--
		return 0, nil
	}
	if e.done {
		return 0, io.EOF
	}
	e.done = true
	return copy(p, e.data), nil
}

func TestReaderAndFramingRegressions(t *testing.T) {
	type result struct {
		total int64
		class string
		cause error
	}
	sized := func(data string, size int, end error, together bool) func() io.Reader {
		return func() io.Reader {
			return &chunkReader{data: []byte(data), size: func() int { return size }, err: end, together: together}
		}
	}
	whole := 1 << 20
	tooFew := rfLimits
	tooFew.MaxTokens = 3
	deep := Limits{MaxBytes: 32768, MaxLineBytes: 32768, MaxTokens: 100}
	deepInput := "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1},\"x\":" + strings.Repeat("[", 10000) +
		strings.Repeat("]", 10000) + "}\n\n" + rfDone
	crlf := rfBlock + "data: [DONE]\r\n\r\n"
	joined := errors.Join(io.EOF, io.ErrUnexpectedEOF)
	tests := []struct {
		name   string
		family Family
		lim    Limits
		reader func() io.Reader
		want   result
	}{
		// 1: a cap between the CR and the LF of the final blank line cuts that line: partial, never final.
		{"cap inside CR LF, whole", OpenAIChat, Limits{MaxBytes: 74, MaxLineBytes: 1024, MaxTokens: 100},
			sized(crlf, whole, io.EOF, false), result{3, classPartial, ErrTooLarge}},
		{"cap inside CR LF, one byte", OpenAIChat, Limits{MaxBytes: 74, MaxLineBytes: 1024, MaxTokens: 100},
			sized(crlf, 1, io.EOF, false), result{3, classPartial, ErrTooLarge}},
		{"cap before the CR LF", OpenAIChat, Limits{MaxBytes: 73, MaxLineBytes: 1024, MaxTokens: 100},
			sized(crlf, whole, io.EOF, false), result{3, classPartial, ErrTooLarge}},
		{"cap after the CR LF", OpenAIChat, Limits{MaxBytes: 75, MaxLineBytes: 1024, MaxTokens: 100},
			sized(crlf, whole, io.EOF, false), result{3, classFinal, nil}},
		// 2: a typed nil error from the reader is a reader failure, never a panic, whatever the chunking.
		{"typed nil with the last bytes", OpenAIChat, tooFew,
			sized(rfBlock+rfGrown+rfDone, whole, (*PartialError)(nil), true), result{3, classPartial, ErrOutOfRange}},
		{"typed nil, one byte", OpenAIChat, tooFew,
			sized(rfBlock+rfGrown+rfDone, 1, (*PartialError)(nil), true), result{3, classPartial, ErrOutOfRange}},
		{"typed nil PartialError later", OpenAIChat, rfLimits, sized(rfBlock, whole, (*PartialError)(nil), false),
			result{3, classPartial, ErrTruncated}},
		{"typed nil TrailingError later", OpenAIChat, rfLimits, sized(rfBlock, whole, (*TrailingError)(nil), false),
			result{3, classPartial, ErrTruncated}},
		{"typed nil LimitError later", OpenAIChat, rfLimits, sized(rfBlock, whole, (*LimitError)(nil), false),
			result{3, classPartial, ErrTruncated}},
		// 3: the lower bound holds under the same Limits: the shorter part gives 2/1 too.
		{"refused block, whole stream", OpenAIChat, tooFew, sized(rfBlock+rfGrown+rfDone, whole, io.EOF, false),
			result{3, classPartial, ErrOutOfRange}},
		{"refused block, shorter part, same limits", OpenAIChat, tooFew, sized(rfBlock+rfGrown, whole, io.EOF, false),
			result{3, classPartial, ErrOutOfRange}},
		// 4: only io.EOF itself ends the input; a join with io.EOF is a reader failure.
		{"joined EOF after the final event", OpenAIChat, rfLimits, sized(rfBlock+rfDone, whole, joined, false),
			result{3, classTrailing, io.ErrUnexpectedEOF}},
		{"joined EOF before the final event", OpenAIChat, rfLimits, sized(rfBlock, whole, joined, false),
			result{3, classPartial, io.ErrUnexpectedEOF}},
		// 7: io.ErrNoProgress after 100 empty reads in a row, with the usual result classes.
		{"99 empty reads", OpenAIChat, rfLimits, func() io.Reader { return &emptyReads{n: 99, data: rfBlock + rfDone} },
			result{3, classFinal, nil}},
		{"100 empty reads before any byte", OpenAIChat, rfLimits,
			func() io.Reader { return &emptyReads{n: 100, data: rfBlock + rfDone} }, result{0, classNone, io.ErrNoProgress}},
		{"100 empty reads after usage", OpenAIChat, rfLimits, func() io.Reader {
			return io.MultiReader(strings.NewReader(rfBlock), &emptyReads{n: 100, data: rfDone})
		}, result{3, classPartial, io.ErrNoProgress}},
		// 8: nesting deeper than encoding/json accepts, within the byte limits, still parses.
		{"deeply nested unknown member", OpenAIChat, deep, sized(deepInput, 32768, io.EOF, false),
			result{3, classFinal, nil}},
		// 9: an unfinished event of any fields after the final event is a trailing problem.
		{"unfinished named event after the final event", OpenAIResponses, rfLimits,
			sized("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,"+
				"\"output_tokens\":1}}}\n\nevent: response.completed\n", whole, io.EOF, false),
			result{3, classTrailing, ErrTruncated}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := ParseStream(tt.family, tt.reader(), tt.lim)
			if got := resultClass(t, err); got != tt.want.class {
				t.Fatalf("class %s (err %v), want %s", got, err, tt.want.class)
			}
			if u.TotalTokens() != tt.want.total {
				t.Errorf("total %d (usage %+v), want %d", u.TotalTokens(), u, tt.want.total)
			}
			if (tt.want.cause == nil) != (err == nil) || tt.want.cause != nil && !errors.Is(err, tt.want.cause) {
				t.Errorf("err = %v, want %v", err, tt.want.cause)
			}
		})
	}
}

// TestNilErrorValues checks that every method of every error type is safe on a nil receiver.
func TestNilErrorValues(t *testing.T) {
	var (
		pe *PartialError
		te *TrailingError
		le *LimitError
		re *readError
	)
	for _, err := range []error{pe, te, le, re} {
		if text := err.Error(); !strings.HasPrefix(text, "modelusage: ") {
			t.Errorf("%T: text %q", err, text)
		}
		if errors.Is(err, ErrTooLarge) || errors.Is(err, ErrTruncated) {
			t.Errorf("nil %T matches a sentinel", err)
		}
	}
	if pe.Unwrap() != nil || te.Unwrap() != nil || le.Is(ErrTooLarge) || re.Is(ErrTruncated) {
		t.Error("a nil value unwraps or matches")
	}
}

// TestJSONReaderErrorText checks that ParseJSON bounds and sanitises a reader's error text, as ParseStream does.
func TestJSONReaderErrorText(t *testing.T) {
	readerErr := errors.New(strings.Repeat("x", 8192) + "\x1b\n")
	u, err := ParseJSON(Anthropic, iotest.ErrReader(readerErr), rfLimits)
	text := err.Error()
	if u != (Usage{}) || len(text) > 200 || strings.ContainsAny(text, "\x1b\n") {
		t.Errorf("usage %+v, error of %d bytes: %q", u, len(text), text)
	}
	if !errors.Is(err, readerErr) {
		t.Errorf("err = %v does not match the reader's error", err)
	}
	if _, ok := errors.AsType[*PartialError](err); ok {
		t.Errorf("err = %v is a partial result", err)
	}
}
