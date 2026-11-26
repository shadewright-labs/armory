package modelusage

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testLimits = Limits{MaxBytes: 1 << 20, MaxLineBytes: 64 << 10, MaxTokens: 1 << 40}

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name)) //nolint:gosec // G304: the names are fixtures of this test
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestParseJSONFixtures(t *testing.T) {
	tests := []struct {
		file    string
		family  Family
		want    Usage
		wantErr error
	}{
		{file: "anthropic-reply.json", family: Anthropic, want: Usage{Family: Anthropic, InputTokens: 25,
			OutputTokens: 15}},
		{file: "anthropic-reply-cache.json", family: Anthropic, want: Usage{Family: Anthropic, InputTokens: 50,
			OutputTokens: 503, CacheCreationInputTokens: 248, CacheReadInputTokens: 1800}},
		{file: "anthropic-reply-null-cache.json", family: Anthropic, want: Usage{Family: Anthropic, InputTokens: 12,
			OutputTokens: 7}},
		{file: "anthropic-reply-zero.json", family: Anthropic, want: Usage{Family: Anthropic}},
		{file: "anthropic-reply-no-usage.json", family: Anthropic, wantErr: ErrNoUsage},
		{file: "anthropic-reply-negative.json", family: Anthropic, wantErr: ErrOutOfRange},
		{file: "chat-reply.json", family: OpenAIChat, want: Usage{Family: OpenAIChat, InputTokens: 19,
			OutputTokens: 10}},
		{file: "chat-reply-cache.json", family: OpenAIChat, want: Usage{Family: OpenAIChat, InputTokens: 2006,
			OutputTokens: 300, CachedPromptTokens: 1920, ReasoningTokens: 192}},
		{file: "chat-reply-minimal.json", family: OpenAIChat, want: Usage{Family: OpenAIChat, InputTokens: 8,
			OutputTokens: 4}},
		{file: "chat-reply-cached-above-prompt.json", family: OpenAIChat, wantErr: ErrOutOfRange},
		// A chat reply holds prompt_tokens and completion_tokens; the Responses names are not read for it.
		{file: "chat-reply-input-names.json", family: OpenAIChat, wantErr: ErrNoUsage},
		{file: "responses-reply.json", family: OpenAIResponses, want: Usage{Family: OpenAIResponses, InputTokens: 75,
			OutputTokens: 1186, ReasoningTokens: 1024}},
		{file: "responses-reply-cache.json", family: OpenAIResponses, want: Usage{Family: OpenAIResponses,
			InputTokens: 12000, OutputTokens: 20, CachedPromptTokens: 4000, CacheWritePromptTokens: 7000}},
		{file: "responses-reply-chat-usage.json", family: OpenAIResponses, wantErr: ErrNoUsage},
		// The usage block of each family is read only under that family.
		{file: "anthropic-reply.json", family: OpenAIChat, wantErr: ErrNoUsage},
		{file: "chat-reply.json", family: Anthropic, wantErr: ErrNoUsage},
	}
	for _, tt := range tests {
		t.Run(tt.file+"/"+tt.family.String(), func(t *testing.T) {
			got, err := ParseJSON(tt.family, openFixture(t, tt.file), testLimits)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("usage = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseStreamFixtures(t *testing.T) {
	tests := []struct {
		file    string
		family  Family
		want    Usage
		wantErr error
	}{
		{file: "anthropic-stream.sse", family: Anthropic, want: Usage{Family: Anthropic, InputTokens: 25,
			OutputTokens: 15}},
		{file: "anthropic-stream-cache.sse", family: Anthropic, want: Usage{Family: Anthropic, InputTokens: 40,
			OutputTokens: 120, CacheReadInputTokens: 2600}},
		// Usage reported in message_start and in two message_delta events: each present count of a later
		// block replaces the earlier one, as the counts are cumulative.
		{file: "anthropic-stream-delta-input.sse", family: Anthropic, want: Usage{Family: Anthropic, InputTokens: 10682,
			OutputTokens: 510, CacheReadInputTokens: 512}},
		{file: "anthropic-stream-zero.sse", family: Anthropic, want: Usage{Family: Anthropic}},
		{file: "anthropic-stream-truncated.sse", family: Anthropic, wantErr: ErrTruncated,
			want: Usage{Family: Anthropic, InputTokens: 25, OutputTokens: 1}},
		{file: "anthropic-stream-no-stop.sse", family: Anthropic, wantErr: ErrTruncated,
			want: Usage{Family: Anthropic, InputTokens: 25, OutputTokens: 15}},
		{file: "anthropic-stream-error.sse", family: Anthropic, wantErr: ErrTruncated,
			want: Usage{Family: Anthropic, InputTokens: 25, OutputTokens: 1}},
		{file: "chat-stream.sse", family: OpenAIChat, want: Usage{Family: OpenAIChat, InputTokens: 1200,
			OutputTokens: 42, CachedPromptTokens: 1024}},
		// Usage on every chunk: the last one wins, as each reports the whole request so far.
		{file: "chat-stream-twice.sse", family: OpenAIChat, want: Usage{Family: OpenAIChat, InputTokens: 300,
			OutputTokens: 7}},
		{file: "chat-stream-zero.sse", family: OpenAIChat, want: Usage{Family: OpenAIChat}},
		{file: "chat-stream-no-usage.sse", family: OpenAIChat, wantErr: ErrNoUsage},
		{file: "chat-stream-truncated.sse", family: OpenAIChat, wantErr: ErrTruncated},
		{file: "chat-stream-no-done.sse", family: OpenAIChat, wantErr: ErrTruncated, want: Usage{Family: OpenAIChat,
			InputTokens: 1200, OutputTokens: 42, CachedPromptTokens: 1024}},
		{file: "chat-stream-error.sse", family: OpenAIChat, wantErr: ErrTruncated},
		{file: "responses-stream.sse", family: OpenAIResponses, want: Usage{Family: OpenAIResponses, InputTokens: 75,
			OutputTokens: 1186, ReasoningTokens: 1024}},
		{file: "responses-stream-cache.sse", family: OpenAIResponses, want: Usage{Family: OpenAIResponses,
			InputTokens: 12000, OutputTokens: 20, CachedPromptTokens: 4000, CacheWritePromptTokens: 7000}},
		{file: "responses-stream-incomplete.sse", family: OpenAIResponses, want: Usage{Family: OpenAIResponses,
			InputTokens: 75, OutputTokens: 512, ReasoningTokens: 512}},
		{file: "responses-stream-zero.sse", family: OpenAIResponses, want: Usage{Family: OpenAIResponses}},
		// Two final events: the counts of two responses cannot be told apart, so neither wins.
		{file: "responses-stream-twice.sse", family: OpenAIResponses, wantErr: ErrMalformed},
		{file: "responses-stream-truncated.sse", family: OpenAIResponses, wantErr: ErrTruncated},
		{file: "responses-stream-no-usage.sse", family: OpenAIResponses, wantErr: ErrNoUsage},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got, err := ParseStream(tt.family, openFixture(t, tt.file), testLimits)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("usage = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseJSONInline(t *testing.T) {
	const anthropicOK = `{"usage":{"input_tokens":3,"output_tokens":4}}`
	tests := []struct {
		name    string
		family  Family
		body    string
		lim     Limits
		want    Usage
		wantErr error
	}{
		{name: "ok", family: Anthropic, body: anthropicOK, want: Usage{Family: Anthropic, InputTokens: 3,
			OutputTokens: 4}},
		{name: "unknown fields ignored", family: Anthropic,
			body: `{"x":[1,{"y":null}],"usage":{"input_tokens":3,"output_tokens":4,"z":"w"}}`,
			want: Usage{Family: Anthropic, InputTokens: 3, OutputTokens: 4}},
		{name: "malformed json", family: Anthropic, body: `{"usage":{"input_tokens":3,`, wantErr: ErrMalformed},
		{name: "empty", family: OpenAIChat, body: ``, wantErr: ErrMalformed},
		{name: "array", family: OpenAIChat, body: `[{"usage":{}}]`, wantErr: ErrMalformed},
		{name: "trailing data", family: Anthropic, body: anthropicOK + `{}`, wantErr: ErrMalformed},
		{name: "usage not an object", family: Anthropic, body: `{"usage":5}`, wantErr: ErrMalformed},
		{name: "usage null", family: OpenAIResponses, body: `{"usage":null}`, wantErr: ErrNoUsage},
		{name: "output missing", family: Anthropic, body: `{"usage":{"input_tokens":3}}`, wantErr: ErrNoUsage},
		{name: "fraction", family: Anthropic, body: `{"usage":{"input_tokens":3.5,"output_tokens":4}}`,
			wantErr: ErrMalformed},
		{name: "exponent", family: Anthropic, body: `{"usage":{"input_tokens":3e2,"output_tokens":4}}`,
			wantErr: ErrMalformed},
		{name: "string count", family: OpenAIChat, body: `{"usage":{"prompt_tokens":"3","completion_tokens":4}}`,
			wantErr: ErrMalformed},
		{name: "beyond int64", family: OpenAIChat,
			body:    `{"usage":{"prompt_tokens":99999999999999999999,"completion_tokens":4}}`,
			wantErr: ErrOutOfRange},
		{name: "negative", family: OpenAIResponses, body: `{"usage":{"input_tokens":3,"output_tokens":-4}}`,
			wantErr: ErrOutOfRange},
		{name: "above max", family: Anthropic, body: `{"usage":{"input_tokens":101,"output_tokens":0}}`,
			lim: Limits{MaxBytes: 1024, MaxTokens: 100}, wantErr: ErrOutOfRange},
		{name: "at max", family: Anthropic, body: `{"usage":{"input_tokens":100,"output_tokens":0}}`,
			lim: Limits{MaxBytes: 1024, MaxTokens: 100}, want: Usage{Family: Anthropic, InputTokens: 100}},
		{name: "anthropic whole prompt above max", family: Anthropic,
			body: `{"usage":{"input_tokens":40,"cache_read_input_tokens":40,` +
				`"cache_creation_input_tokens":40,"output_tokens":0}}`,
			lim: Limits{MaxBytes: 1024, MaxTokens: 100}, wantErr: ErrOutOfRange},
		{name: "total above max", family: OpenAIChat, body: `{"usage":{"prompt_tokens":60,"completion_tokens":60}}`,
			lim: Limits{MaxBytes: 1024, MaxTokens: 100}, wantErr: ErrOutOfRange},
		{name: "reasoning above output", family: OpenAIResponses,
			body:    `{"usage":{"input_tokens":3,"output_tokens":4,"output_tokens_details":{"reasoning_tokens":5}}}`,
			wantErr: ErrOutOfRange},
		{name: "cache reads and writes above prompt", family: OpenAIResponses,
			body: `{"usage":{"input_tokens":10,"output_tokens":4,` +
				`"input_tokens_details":{"cached_tokens":6,"cache_write_tokens":5}}}`,
			wantErr: ErrOutOfRange},
		{name: "exactly the byte limit", family: Anthropic, body: anthropicOK,
			lim:  Limits{MaxBytes: int64(len(anthropicOK)), MaxTokens: 10},
			want: Usage{Family: Anthropic, InputTokens: 3, OutputTokens: 4}},
		{name: "one byte over the limit", family: Anthropic, body: anthropicOK + " ",
			lim: Limits{MaxBytes: int64(len(anthropicOK)), MaxTokens: 10}, wantErr: ErrTooLarge},
		{name: "unknown family", family: Family(9), body: anthropicOK, wantErr: ErrInvalidLimits},
		{name: "zero limits", family: Anthropic, body: anthropicOK, lim: Limits{MaxTokens: -1},
			wantErr: ErrInvalidLimits},
		{name: "max tokens above ceiling", family: Anthropic, body: anthropicOK, lim: Limits{MaxBytes: 10,
			MaxTokens: MaxTokensCeiling + 1},
			wantErr: ErrInvalidLimits},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lim := tt.lim
			if lim == (Limits{}) {
				lim = testLimits
			}
			got, err := ParseJSON(tt.family, strings.NewReader(tt.body), lim)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("usage = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseStreamInline(t *testing.T) {
	const (
		start = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n"
		delta = "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n"
		stop  = "data: {\"type\":\"message_stop\"}\n\n"
	)
	ok := Usage{Family: Anthropic, InputTokens: 5, OutputTokens: 9}
	long := "data: {\"type\":\"ping\",\"pad\":\"" + strings.Repeat("x", 100) + "\"}"
	tests := []struct {
		name    string
		family  Family
		body    string
		lim     Limits
		want    Usage
		wantErr error
	}{
		{name: "ok", family: Anthropic, body: start + delta + stop, want: ok},
		{name: "crlf and comments", family: Anthropic, body: strings.ReplaceAll(": keep-alive\n\n"+start+delta+stop,
			"\n", "\r\n"), want: ok},
		{name: "no space after colon", family: Anthropic, body: strings.ReplaceAll(start+delta+stop, "data: ",
			"data:"), want: ok},
		{name: "data split over lines", family: Anthropic,
			body: "data: {\"type\":\"message_start\",\n" +
				"data: \"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" + delta + stop,
			want: ok},
		{name: "last event without blank line", family: Anthropic, body: start + delta +
			"data: {\"type\":\"message_stop\"}", want: ok},
		{name: "event cut at the end", family: Anthropic, body: start + delta + "data: {\"type\":\"message_st",
			wantErr: ErrTruncated, want: ok},
		{name: "empty stream", family: Anthropic, body: "", wantErr: ErrTruncated},
		{name: "malformed event", family: Anthropic, body: start + "data: {nope}\n\n" + stop, wantErr: ErrMalformed},
		{name: "event after the final one", family: Anthropic, body: start + delta + stop + delta,
			wantErr: ErrMalformed},
		{name: "second message_start", family: Anthropic, body: start + start + delta + stop, wantErr: ErrMalformed},
		{name: "delta before start", family: Anthropic, body: delta + start + stop, wantErr: ErrMalformed},
		{name: "no input count", family: Anthropic, body: "data: {\"type\":\"message_start\",\"message\":{}}\n\n" +
			delta + stop, wantErr: ErrNoUsage},
		{name: "negative delta", family: Anthropic, body: start +
			"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":-9}}\n\n" + stop,
			wantErr: ErrOutOfRange},
		{name: "null delta count keeps the earlier one", family: Anthropic,
			body: start + delta + "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":null}}\n\n" + stop,
			want: ok},
		{name: "line at the limit", family: Anthropic, body: start + long + "\n\n" + delta + stop,
			lim: Limits{MaxBytes: 4096, MaxLineBytes: len(long), MaxTokens: 100}, want: ok},
		{name: "line at the limit with crlf", family: Anthropic, body: start + long + "\r\n\r\n" + delta + stop,
			lim: Limits{MaxBytes: 4096, MaxLineBytes: len(long), MaxTokens: 100}, want: ok},
		{name: "oversized line", family: Anthropic, body: start + long + "x\n\n" + delta + stop,
			lim: Limits{MaxBytes: 4096, MaxLineBytes: len(long), MaxTokens: 100}, wantErr: ErrLineTooLong},
		{name: "oversized line without newline", family: Anthropic, body: start + long + strings.Repeat("x", 1000),
			lim: Limits{MaxBytes: 4096, MaxLineBytes: len(long), MaxTokens: 100}, wantErr: ErrLineTooLong},
		{name: "stream over the byte limit", family: Anthropic, body: start + delta + stop + "\n",
			lim:     Limits{MaxBytes: int64(len(start + delta + stop)), MaxLineBytes: 1024, MaxTokens: 100},
			wantErr: ErrTooLarge},
		{name: "stream at the byte limit", family: Anthropic, body: start + delta + stop,
			lim: Limits{MaxBytes: int64(len(start + delta + stop)), MaxLineBytes: 1024, MaxTokens: 100}, want: ok},
		{name: "count above max", family: Anthropic, body: start + delta + stop,
			lim: Limits{MaxBytes: 4096, MaxLineBytes: 1024, MaxTokens: 8}, wantErr: ErrOutOfRange},
		{name: "chat data after done", family: OpenAIChat,
			body:    "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\ndata: {}\n\n",
			wantErr: ErrMalformed},
		{name: "chat non-json data", family: OpenAIChat, body: "data: hello\n\ndata: [DONE]\n\n",
			wantErr: ErrMalformed},
		{name: "chat usage without counts", family: OpenAIChat, body: "data: {\"usage\":{}}\n\ndata: [DONE]\n\n",
			wantErr: ErrNoUsage},
		{name: "chat usage above prompt", family: OpenAIChat,
			body: "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1," +
				"\"prompt_tokens_details\":{\"cached_tokens\":2}}}\n\ndata: [DONE]\n\n",
			wantErr: ErrOutOfRange},
		{name: "responses usage in created is ignored", family: OpenAIResponses,
			body: "data: {\"type\":\"response.created\"," +
				"\"response\":{\"usage\":{\"input_tokens\":99,\"output_tokens\":99}}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}}\n\n",
			want: Usage{Family: OpenAIResponses, InputTokens: 2, OutputTokens: 3}},
		{name: "responses failed with usage", family: OpenAIResponses,
			body: "data: {\"type\":\"response.failed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n",
			want: Usage{Family: OpenAIResponses, InputTokens: 2}},
		{name: "responses final without response", family: OpenAIResponses,
			body: "data: {\"type\":\"response.completed\"}\n\n", wantErr: ErrMalformed},
		{name: "responses error event", family: OpenAIResponses,
			body: "data: {\"type\":\"error\",\"code\":\"server_error\"}\n\n", wantErr: ErrTruncated},
		{name: "zero line limit", family: Anthropic, body: start, lim: Limits{MaxBytes: 10, MaxTokens: 10},
			wantErr: ErrInvalidLimits},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lim := tt.lim
			if lim == (Limits{}) {
				lim = testLimits
			}
			got, err := ParseStream(tt.family, strings.NewReader(tt.body), lim)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("usage = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// errReader returns its data, then err.
type errReader struct {
	data string
	err  error
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.data == "" {
		return 0, e.err
	}
	n := copy(p, e.data)
	e.data = e.data[n:]
	return n, nil
}

func TestReaderErrorIsWrapped(t *testing.T) {
	boom := errors.New("connection reset")
	if _, err := ParseJSON(Anthropic, &errReader{data: `{"usage":`, err: boom}, testLimits); !errors.Is(err, boom) {
		t.Errorf("ParseJSON err = %v, want the reader's error", err)
	}
	if _, err := ParseStream(Anthropic, &errReader{data: "data: {}\n", err: boom}, testLimits); !errors.Is(err, boom) {
		t.Errorf("ParseStream err = %v, want the reader's error", err)
	}
}

// infiniteReader never ends, so a parser that read without a bound would never return.
type infiniteReader struct{ n int64 }

func (r *infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
		if i%50 == 49 {
			p[i] = '\n'
		}
	}
	r.n += int64(len(p))
	return len(p), nil
}

func TestReadingIsBounded(t *testing.T) {
	lim := Limits{MaxBytes: 1 << 16, MaxLineBytes: 1 << 10, MaxTokens: 100}
	for _, parse := range []func(Family, io.Reader, Limits) (Usage, error){ParseJSON, ParseStream} {
		r := &infiniteReader{}
		if _, err := parse(OpenAIChat, r, lim); !errors.Is(err, ErrTooLarge) {
			t.Errorf("err = %v, want ErrTooLarge", err)
		}
		if r.n > lim.MaxBytes+1 {
			t.Errorf("read %d bytes, limit %d", r.n, lim.MaxBytes)
		}
	}
}

func TestLimitError(t *testing.T) {
	var le *LimitError
	_, err := ParseJSON(Anthropic, strings.NewReader("{}  "), Limits{MaxBytes: 2, MaxTokens: 1})
	if !errors.As(err, &le) || le.Line || le.Limit != 2 || errors.Is(err, ErrLineTooLong) {
		t.Errorf("err = %#v, want a reply LimitError of 2 bytes", err)
	}
	_, err = ParseStream(Anthropic, strings.NewReader("data: 12345\n"), Limits{MaxBytes: 100, MaxLineBytes: 4,
		MaxTokens: 1})
	if !errors.As(err, &le) || !le.Line || le.Limit != 4 || errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %#v, want a line LimitError of 4 bytes", err)
	}
	if le.Error() == "" || (&LimitError{Limit: 1}).Error() == "" {
		t.Error("empty error text")
	}
}
