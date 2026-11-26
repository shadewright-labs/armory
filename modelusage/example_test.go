package modelusage_test

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shadewright-labs/armory/modelusage"
)

func Example() {
	const reply = `{
		"id": "msg_01",
		"type": "message",
		"role": "assistant",
		"usage": {
			"input_tokens": 50,
			"cache_creation_input_tokens": 240,
			"cache_read_input_tokens": 1800,
			"output_tokens": 120
		}
	}`
	lim := modelusage.Limits{MaxBytes: 1 << 20, MaxTokens: 1 << 30}
	u, err := modelusage.ParseJSON(modelusage.Anthropic, strings.NewReader(reply), lim)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("prompt=%d uncached=%d output=%d total=%d\n",
		u.TotalInputTokens(), u.UncachedInputTokens(), u.OutputTokens, u.TotalTokens())
	// Output:
	// prompt=2090 uncached=50 output=120 total=2210
}

func ExampleParseJSON() {
	const reply = `{
		"id": "chatcmpl-01",
		"object": "chat.completion",
		"usage": {
			"prompt_tokens": 1200,
			"completion_tokens": 300,
			"prompt_tokens_details": {"cached_tokens": 1000},
			"completion_tokens_details": {"reasoning_tokens": 180}
		}
	}`
	lim := modelusage.Limits{MaxBytes: 1 << 20, MaxTokens: 1 << 30}
	u, err := modelusage.ParseJSON(modelusage.OpenAIChat, strings.NewReader(reply), lim)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("prompt=%d uncached=%d cached=%d output=%d reasoning=%d total=%d\n",
		u.TotalInputTokens(), u.UncachedInputTokens(), u.CacheReadTokens(),
		u.OutputTokens, u.ReasoningTokens, u.TotalTokens())
	// Output:
	// prompt=1200 uncached=200 cached=1000 output=300 reasoning=180 total=1500
}

func ExampleParseJSON_responses() {
	const reply = `{
		"id": "resp_01",
		"object": "response",
		"status": "completed",
		"usage": {
			"input_tokens": 5000,
			"output_tokens": 80,
			"input_tokens_details": {
				"cached_tokens": 3000,
				"cache_write_tokens": 1500
			}
		}
	}`
	lim := modelusage.Limits{MaxBytes: 1 << 20, MaxTokens: 1 << 30}
	u, err := modelusage.ParseJSON(modelusage.OpenAIResponses, strings.NewReader(reply), lim)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("family=%s prompt=%d uncached=%d read=%d write=%d total=%d\n",
		u.Family, u.TotalInputTokens(), u.UncachedInputTokens(),
		u.CacheReadTokens(), u.CacheWriteTokens(), u.TotalTokens())
	// Output:
	// family=openai-responses prompt=5000 uncached=500 read=3000 write=1500 total=5080
}

func ExampleParseStream() {
	const stream = "" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":40,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":18}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	lim := modelusage.Limits{MaxBytes: 1 << 20, MaxLineBytes: 64 << 10, MaxTokens: 1 << 30}
	u, err := modelusage.ParseStream(modelusage.Anthropic, strings.NewReader(stream), lim)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("input=%d output=%d total=%d\n", u.TotalInputTokens(), u.OutputTokens, u.TotalTokens())
	// Output:
	// input=40 output=18 total=58
}

func ExamplePartialError() {
	const stream = "" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":35}}` + "\n\n"

	lim := modelusage.Limits{MaxBytes: 1 << 20, MaxLineBytes: 64 << 10, MaxTokens: 1 << 30}
	u, err := modelusage.ParseStream(modelusage.Anthropic, strings.NewReader(stream), lim)
	if partial, ok := errors.AsType[*modelusage.PartialError](err); ok {
		fmt.Printf("truncated=%v total=%d\n", errors.Is(partial, modelusage.ErrTruncated), u.TotalTokens())
	}
	// Output:
	// truncated=true total=135
}

func ExampleTrailingError() {
	const stream = "" +
		`data: {"usage":{"prompt_tokens":80,"completion_tokens":20}}` + "\n\n" +
		"data: [DONE]\n\n" +
		`data: {"extra":true}` + "\n\n"

	lim := modelusage.Limits{MaxBytes: 1 << 20, MaxLineBytes: 64 << 10, MaxTokens: 1 << 30}
	u, err := modelusage.ParseStream(modelusage.OpenAIChat, strings.NewReader(stream), lim)
	if trailing, ok := errors.AsType[*modelusage.TrailingError](err); ok {
		fmt.Printf("final_total=%d malformed=%v\n",
			u.TotalTokens(), errors.Is(trailing, modelusage.ErrMalformed))
	}
	// Output:
	// final_total=100 malformed=true
}

func ExampleLimitError() {
	reply := `{"usage":{"input_tokens":10,"output_tokens":5}}`
	_, err := modelusage.ParseJSON(modelusage.Anthropic, strings.NewReader(reply),
		modelusage.Limits{MaxBytes: 16, MaxTokens: 1 << 30})
	if le, ok := errors.AsType[*modelusage.LimitError](err); ok {
		fmt.Printf("reply: too_large=%v line=%v limit=%d\n",
			errors.Is(err, modelusage.ErrTooLarge), le.Line, le.Limit)
	}

	stream := "data: {\"type\":\"message_start\"}\n\n"
	_, err = modelusage.ParseStream(modelusage.Anthropic, strings.NewReader(stream),
		modelusage.Limits{MaxBytes: 1024, MaxLineBytes: 12, MaxTokens: 1 << 30})
	if le, ok := errors.AsType[*modelusage.LimitError](err); ok {
		fmt.Printf("stream: line_too_long=%v line=%v limit=%d\n",
			errors.Is(err, modelusage.ErrLineTooLong), le.Line, le.Limit)
	}
	// Output:
	// reply: too_large=true line=false limit=16
	// stream: line_too_long=true line=true limit=12
}

func ExampleUsage() {
	cases := []modelusage.Usage{
		{
			Family:                   modelusage.Anthropic,
			InputTokens:              50,
			CacheCreationInputTokens: 200,
			CacheReadInputTokens:     1000,
			OutputTokens:             100,
		},
		{
			Family:                 modelusage.OpenAIResponses,
			InputTokens:            1250,
			CachedPromptTokens:     1000,
			CacheWritePromptTokens: 200,
			OutputTokens:           100,
		},
	}
	for _, u := range cases {
		fmt.Printf("%s: prompt=%d uncached=%d read=%d write=%d total=%d\n",
			u.Family, u.TotalInputTokens(), u.UncachedInputTokens(),
			u.CacheReadTokens(), u.CacheWriteTokens(), u.TotalTokens())
	}
	// Output:
	// anthropic: prompt=1250 uncached=50 read=1000 write=200 total=1350
	// openai-responses: prompt=1250 uncached=50 read=1000 write=200 total=1350
}
