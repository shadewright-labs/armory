package modelusage

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// benchSize is the size of the replies the benchmarks read.
const benchSize = 1 << 20

var benchLimits = Limits{MaxBytes: 2 << 20, MaxLineBytes: 64 << 10, MaxTokens: 1 << 40}

// benchStream builds a stream of family f of about benchSize bytes: a first event, text deltas until the size
// is reached, and the events that carry the usage and end the reply.
func benchStream(f Family) []byte {
	var b bytes.Buffer
	text := strings.Repeat("The pod is out of memory. ", 4)
	switch f {
	case Anthropic:
		b.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_bench\"," +
			"\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":2679," +
			"\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0,\"output_tokens\":3}}}\n\n")
		for i := 0; b.Len() < benchSize; i++ {
			fmt.Fprintf(&b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,"+
				"\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
		}
		b.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}," +
			"\"usage\":{\"output_tokens\":9000}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	case OpenAIChat:
		for b.Len() < benchSize {
			fmt.Fprintf(&b, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,"+
				"\"model\":\"gpt-example\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"logprobs\":null,"+
				"\"finish_reason\":null}],\"usage\":null}\n\n", text)
		}
		b.WriteString("data: {\"id\":\"chatcmpl-bench\",\"choices\":[],\"usage\":{\"prompt_tokens\":1200," +
			"\"completion_tokens\":9000,\"prompt_tokens_details\":{\"cached_tokens\":1024}}}\n\ndata: [DONE]\n\n")
	default:
		for i := 0; b.Len() < benchSize; i++ {
			fmt.Fprintf(&b, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\","+
				"\"sequence_number\":%d,\"item_id\":\"msg_bench\",\"output_index\":0,\"content_index\":0,\"delta\":%q}\n\n",
				i, text)
		}
		b.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\"," +
			"\"response\":{\"id\":\"resp_bench\",\"output\":[],\"usage\":{\"input_tokens\":1200,\"output_tokens\":9000}}}\n\n")
	}
	return b.Bytes()
}

// benchReply builds a complete Anthropic reply of about benchSize bytes, most of it content the parser skips.
func benchReply() []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":"msg_bench","type":"message","role":"assistant","content":[`)
	text := strings.Repeat("The pod is out of memory. ", 4)
	for i := 0; b.Len() < benchSize; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"type":"text","text":%q,"citations":[{"n":%d}]}`, text, i)
	}
	b.WriteString(`],"stop_reason":"end_turn","usage":{"input_tokens":2679,"output_tokens":9000}}`)
	return b.Bytes()
}

func BenchmarkParseStream(b *testing.B) {
	for _, f := range []Family{Anthropic, OpenAIChat, OpenAIResponses} {
		data := benchStream(f)
		b.Run(f.String(), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := ParseStream(f, bytes.NewReader(data), benchLimits); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkParseJSON(b *testing.B) {
	data := benchReply()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseJSON(Anthropic, bytes.NewReader(data), benchLimits); err != nil {
			b.Fatal(err)
		}
	}
}

// TestStreamAllocation bounds what one 1 MiB stream allocates, so that decoding every member of every event,
// which allocated about 6 to 8 MB per stream, cannot come back unnoticed.
func TestStreamAllocation(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("allocation counts differ under the race detector")
	}
	for _, f := range []Family{Anthropic, OpenAIChat, OpenAIResponses} {
		data := benchStream(f)
		parse := func() {
			if _, err := ParseStream(f, bytes.NewReader(data), benchLimits); err != nil {
				t.Fatal(err)
			}
		}
		parse()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		const runs = 4
		for range runs {
			parse()
		}
		runtime.ReadMemStats(&after)
		if got := (after.TotalAlloc - before.TotalAlloc) / runs; got > 256<<10 {
			t.Errorf("%v: %d bytes allocated for a stream of %d bytes, want at most 256 KiB", f, got, len(data))
		}
	}
}
