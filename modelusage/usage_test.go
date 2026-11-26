package modelusage

import (
	"errors"
	"testing"
)

func TestUsageTotals(t *testing.T) {
	tests := []struct {
		name                                            string
		u                                               Usage
		totalIn, cacheRead, cacheWrite, uncached, total int64
	}{
		{
			name: "anthropic adds the cache counts to input",
			u: Usage{Family: Anthropic, InputTokens: 50, OutputTokens: 503, CacheCreationInputTokens: 248,
				CacheReadInputTokens: 1800},
			totalIn: 2098, cacheRead: 1800, cacheWrite: 248, uncached: 50, total: 2601,
		},
		{
			name:    "anthropic without cache",
			u:       Usage{Family: Anthropic, InputTokens: 25, OutputTokens: 15},
			totalIn: 25, uncached: 25, total: 40,
		},
		{
			name: "chat counts cached tokens inside prompt_tokens",
			u: Usage{Family: OpenAIChat, InputTokens: 2006, OutputTokens: 300, CachedPromptTokens: 1920,
				ReasoningTokens: 192},
			totalIn: 2006, cacheRead: 1920, uncached: 86, total: 2306,
		},
		{
			name: "responses counts reads and writes inside input_tokens",
			u: Usage{Family: OpenAIResponses, InputTokens: 12000, OutputTokens: 20, CachedPromptTokens: 4000,
				CacheWritePromptTokens: 7000},
			totalIn: 12000, cacheRead: 4000, cacheWrite: 7000, uncached: 1000, total: 12020,
		},
		{name: "zero", u: Usage{Family: OpenAIResponses}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := tt.u
			got := [5]int64{u.TotalInputTokens(), u.CacheReadTokens(), u.CacheWriteTokens(), u.UncachedInputTokens(),
				u.TotalTokens()}
			want := [5]int64{tt.totalIn, tt.cacheRead, tt.cacheWrite, tt.uncached, tt.total}
			if got != want {
				t.Errorf("totals (in, read, write, uncached, total) = %v, want %v", got, want)
			}
		})
	}
}

func TestFamilyString(t *testing.T) {
	for f, want := range map[Family]string{Anthropic: "anthropic", OpenAIChat: "openai-chat",
		OpenAIResponses: "openai-responses", Family(0): "unknown"} {
		if got := f.String(); got != want {
			t.Errorf("Family(%d).String() = %q, want %q", int(f), got, want)
		}
	}
}

// TestBreakdownsArePartsOfTheirParent checks that a breakdown never adds to the totals and that validate
// refuses a breakdown whose sum is larger than its parent count.
func TestBreakdownsArePartsOfTheirParent(t *testing.T) {
	anthropic := Usage{Family: Anthropic, InputTokens: 50, OutputTokens: 500, CacheCreationInputTokens: 248,
		CacheReadInputTokens: 1800}
	withAnthropic := anthropic
	withAnthropic.CacheCreation5mInputTokens, withAnthropic.CacheCreation1hInputTokens = 148, 100
	withAnthropic.ReasoningTokens = 400
	responses := Usage{Family: OpenAIResponses, InputTokens: 12000, OutputTokens: 20, CachedPromptTokens: 4000,
		CacheWritePromptTokens: 7000}
	withResponses := responses
	withResponses.CacheWrite30mPromptTokens, withResponses.CacheWrite12hPromptTokens = 3000, 4000
	for _, pair := range [][2]Usage{{anthropic, withAnthropic}, {responses, withResponses}} {
		a, b := pair[0], pair[1]
		if a.TotalInputTokens() != b.TotalInputTokens() || a.TotalTokens() != b.TotalTokens() ||
			a.CacheWriteTokens() != b.CacheWriteTokens() || a.UncachedInputTokens() != b.UncachedInputTokens() {
			t.Errorf("a breakdown changed the totals of %+v", b)
		}
		if err := b.validate(1 << 40); err != nil {
			t.Errorf("validate(%+v) = %v, want nil", b, err)
		}
	}
	over := []func(*Usage){
		func(u *Usage) { u.CacheCreation5mInputTokens++ },
		func(u *Usage) { u.CacheCreation1hInputTokens++ },
		func(u *Usage) { u.ReasoningTokens = u.OutputTokens + 1 },
	}
	for i, f := range over {
		u := withAnthropic
		f(&u)
		if err := u.validate(1 << 40); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("anthropic case %d: validate = %v, want ErrOutOfRange", i, err)
		}
	}
	for i, f := range []func(*Usage){
		func(u *Usage) { u.CacheWrite30mPromptTokens++ },
		func(u *Usage) { u.CacheWrite12hPromptTokens++ },
	} {
		u := withResponses
		f(&u)
		if err := u.validate(1 << 40); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("responses case %d: validate = %v, want ErrOutOfRange", i, err)
		}
	}
}
