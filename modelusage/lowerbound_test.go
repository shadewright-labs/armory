package modelusage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file check one rule of ParseStream: once a stream has yielded a usage block, nothing that
// follows can make the result less than that usage. They cut a clean stream after each of its events, read the
// usage U of that prefix with ParseStream, append bytes (a table of problems, or fuzzed ones) and check that
//
//   - every count of the result is at least the same count of U (the counts of every family only grow: the
//     Anthropic and chat completions counts are cumulative, and the Responses usage comes once, in the final
//     event, so U is zero before it);
//   - when the prefix holds the final event, the result is exactly U, with a nil error or a *TrailingError;
//   - when U holds any usage, an error is a *PartialError or a *TrailingError, so the usage comes with it;
//   - the error is nil only for a clean complete stream: never after a reader error, and never without the
//     final event of the family in the input.

// cleanStreams are complete streams that ParseStream reads with a nil error.
var cleanStreams = []struct {
	file   string
	family Family
}{
	{"anthropic-stream.sse", Anthropic},
	{"anthropic-stream-cache.sse", Anthropic},
	{"anthropic-stream-delta-input.sse", Anthropic},
	{"anthropic-stream-zero.sse", Anthropic},
	{"chat-stream.sse", OpenAIChat},
	{"chat-stream-twice.sse", OpenAIChat},
	{"chat-stream-zero.sse", OpenAIChat},
	{"responses-stream.sse", OpenAIResponses},
	{"responses-stream-cache.sse", OpenAIResponses},
	{"responses-stream-incomplete.sse", OpenAIResponses},
	{"responses-stream-zero.sse", OpenAIResponses},
}

// finalMarkers are the bytes of the final event of each family; a clean complete stream holds one of them.
var finalMarkers = map[Family][]string{
	Anthropic:       {`"message_stop"`},
	OpenAIChat:      {"[DONE]"},
	OpenAIResponses: {`"response.completed"`, `"response.incomplete"`, `"response.failed"`},
}

// eventEnds returns the offsets just after each event of the LF-only stream data: after each blank line.
func eventEnds(data []byte) []int {
	var ends []int
	for i := 0; ; {
		k := bytes.Index(data[i:], []byte("\n\n"))
		if k < 0 {
			return ends
		}
		i += k + 2
		ends = append(ends, i)
	}
}

// counts lists the token counts of u, in a fixed order.
func (u Usage) countList() []int64 {
	return []int64{u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheCreation5mInputTokens,
		u.CacheCreation1hInputTokens, u.CacheReadInputTokens, u.CachedPromptTokens, u.CacheWritePromptTokens,
		u.CacheWrite30mPromptTokens, u.CacheWrite12hPromptTokens, u.ReasoningTokens}
}

// prefixCase is a clean stream cut after an event, with the usage ParseStream reads from that prefix.
type prefixCase struct {
	family Family
	prefix []byte
	usage  Usage // U: the usage of the prefix
	final  bool  // the prefix holds the final event: its error was nil
}

func readPrefix(t testing.TB, family Family, prefix []byte) prefixCase {
	t.Helper()
	u, err := ParseStream(family, bytes.NewReader(prefix), fuzzLimits)
	return prefixCase{family: family, prefix: prefix, usage: u, final: err == nil}
}

// allPrefixes cuts every clean stream after each of its events, and before the first.
func allPrefixes(t testing.TB) []prefixCase {
	t.Helper()
	var out []prefixCase
	for _, s := range cleanStreams {
		data, err := os.ReadFile(filepath.Join("testdata", s.file))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, readPrefix(t, s.family, nil))
		for _, end := range eventEnds(data) {
			out = append(out, readPrefix(t, s.family, data[:end]))
		}
	}
	return out
}

// checkLowerBound reads pc.prefix followed by suffix, then a reader error when readErr is set, and checks the
// rule. It returns the error of the parse.
func checkLowerBound(t *testing.T, pc prefixCase, suffix []byte, readErr error) error {
	t.Helper()
	input := string(pc.prefix) + string(suffix)
	r := &errReader{data: input, err: readErr}
	if readErr == nil {
		r.err = io.EOF
	}
	u, err := ParseStream(pc.family, r, fuzzLimits)
	var (
		pe *PartialError
		te *TrailingError
	)
	isPartial, isTrailing := errors.As(err, &pe), errors.As(err, &te)
	got, low := u.countList(), pc.usage.countList()
	for i := range low {
		if got[i] < low[i] {
			t.Fatalf("usage %+v (err %v) is less than the usage %+v of the prefix", u, err, pc.usage)
		}
	}
	if u != (Usage{}) && u.Family != pc.family {
		t.Fatalf("family %v, want %v", u.Family, pc.family)
	}
	if pc.final {
		if u != pc.usage {
			t.Fatalf("usage %+v (err %v) after the final event, want %+v", u, err, pc.usage)
		}
		if err != nil && !isTrailing {
			t.Fatalf("err = %v after the final event, want nil or a *TrailingError", err)
		}
	}
	if pc.usage != (Usage{}) && err != nil && !isPartial && !isTrailing {
		t.Fatalf("err = %v drops the usage %+v the prefix yielded", err, pc.usage)
	}
	if isPartial && isTrailing {
		t.Fatalf("err = %v is both partial and trailing", err)
	}
	if (isPartial || isTrailing) && u == (Usage{}) {
		t.Fatalf("err = %v comes with no usage", err)
	}
	if err == nil {
		if readErr != nil {
			t.Fatalf("nil error after the reader failed with %v", readErr)
		}
		hasFinal := false
		for _, m := range finalMarkers[pc.family] {
			hasFinal = hasFinal || strings.Contains(input, m)
		}
		if !hasFinal {
			t.Fatalf("nil error for a stream without its final event: %q", input)
		}
		if verr := u.validate(fuzzLimits.MaxTokens); verr != nil {
			t.Fatalf("usage %+v breaks its rules: %v", u, verr)
		}
	}
	if isPartial || isTrailing {
		if rerr := u.checkRange(fuzzLimits.MaxTokens); rerr != nil {
			t.Fatalf("usage %+v with %v is out of range: %v", u, err, rerr)
		}
	}
	return err
}

// problems are suffixes that each break a stream, per family where the bytes differ.
func problems(f Family) map[string]string {
	long := "data: " + strings.Repeat("x", fuzzLimits.MaxLineBytes+1) + "\n\n"
	over := strings.Repeat(": "+strings.Repeat("p", 100)+"\n", int(fuzzLimits.MaxBytes/100)+1)
	p := map[string]string{
		"malformed event":   "data: {nope}\n\n",
		"second done":       "data: [DONE]\n\ndata: [DONE]\n\n",
		"oversized line":    long,
		"over the byte cap": over,
		"garbage":           "data: \x00\xff{{{\n\n",
		"cut event":         "data: {\"type\":",
	}
	switch f {
	case Anthropic:
		p["duplicate name"] = "data: {\"type\":\"ping\",\"type\":\"ping\"}\n\n"
		p["second final event"] = "data: {\"type\":\"message_stop\"}\n\ndata: {\"type\":\"message_stop\"}\n\n"
		p["later count out of range"] = "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":-1}}\n\n"
		p["later count not an integer"] = "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1.5}}\n\n"
		p["later lower counts"] = "data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0," +
			"\"cache_read_input_tokens\":0}}\n\n"
		p["later count above the limit"] = "data: {\"type\":\"message_delta\"," +
			"\"usage\":{\"output_tokens\":1099511627777}}\n\n"
		p["error event"] = "data: {\"type\":\"error\"}\n\n"
		p["second start"] = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n"
	case OpenAIChat:
		p["duplicate name"] = "data: {\"usage\":null,\"usage\":null}\n\n"
		p["second final event"] = p["second done"]
		p["later count out of range"] = "data: {\"usage\":{\"prompt_tokens\":-1,\"completion_tokens\":1}}\n\n"
		p["later count not an integer"] = "data: {\"usage\":{\"prompt_tokens\":1.5,\"completion_tokens\":1}}\n\n"
		p["later lower counts"] = "data: {\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0}}\n\n"
		p["later usage without counts"] = "data: {\"usage\":{}}\n\n"
		p["error chunk"] = "data: {\"error\":{\"message\":\"x\"}}\n\n"
	case OpenAIResponses:
		final := func(in string) string {
			return "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":" + in +
				",\"output_tokens\":1}}}\n\n"
		}
		p["duplicate name"] = "data: {\"type\":\"x\",\"type\":\"x\"}\n\n"
		p["second final event"] = final("1") + final("2")
		p["later count out of range"] = final("-1")
		p["later count not an integer"] = final("1.5")
		p["error event"] = "data: {\"type\":\"error\"}\n\n"
	}
	return p
}

func TestUsageIsALowerBound(t *testing.T) {
	cut := errors.New("connection reset")
	for _, pc := range allPrefixes(t) {
		for name, suffix := range problems(pc.family) {
			for _, readErr := range []error{nil, cut} {
				t.Run(pc.family.String()+"/"+name, func(t *testing.T) {
					err := checkLowerBound(t, pc, []byte(suffix), readErr)
					if err == nil {
						t.Fatalf("nil error for %q after %q", suffix, pc.prefix)
					}
					if readErr != nil && !errors.Is(err, cut) && !errors.Is(err, ErrMalformed) &&
						!errors.Is(err, ErrLineTooLong) && !errors.Is(err, ErrTooLarge) &&
						!errors.Is(err, ErrOutOfRange) && !errors.Is(err, ErrTruncated) && !errors.Is(err, ErrNoUsage) {
						t.Fatalf("err = %v names no cause", err)
					}
				})
			}
		}
		// The clean prefix itself, and with only a reader error after it.
		if err := checkLowerBound(t, pc, nil, nil); (err == nil) != pc.final {
			t.Fatalf("err = %v for the prefix %q, want nil: %v", err, pc.prefix, pc.final)
		}
		if err := checkLowerBound(t, pc, nil, cut); !errors.Is(err, cut) {
			t.Fatalf("err = %v, want it to wrap the reader's error", err)
		}
	}
}

func FuzzStreamLowerBound(f *testing.F) {
	for fam := range uint8(3) {
		for _, suffix := range problems(Family(fam) + Anthropic) {
			f.Add(uint16(fam), uint16(5), []byte(suffix), false)
		}
	}
	f.Add(uint16(0), uint16(9), []byte("data: {\"type\":\"ping\"}\n\n"), true)
	streams := make([][]byte, len(cleanStreams))
	for i, s := range cleanStreams {
		data, err := os.ReadFile(filepath.Join("testdata", s.file))
		if err != nil {
			f.Fatal(err)
		}
		streams[i] = data
	}
	f.Fuzz(func(t *testing.T, pick, k uint16, suffix []byte, fail bool) {
		i := int(pick) % len(cleanStreams)
		s, data := cleanStreams[i], streams[i]
		ends := append([]int{0}, eventEnds(data)...)
		pc := readPrefix(t, s.family, data[:ends[int(k)%len(ends)]])
		var readErr error
		if fail {
			readErr = errors.New("connection reset")
		}
		_ = checkLowerBound(t, pc, suffix, readErr)
	})
}
