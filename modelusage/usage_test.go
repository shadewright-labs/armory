package modelusage

import (
	"errors"
	"testing"
)

func TestUsageTotals(t *testing.T) {
	tests := []struct {
		name                                                      string
		u                                                         Usage
		totalIn, totalOut, cacheRead, cacheWrite, uncached, total int64
	}{
		{
			name: "anthropic adds the cache counts to input",
			u: Usage{Family: Anthropic, InputTokens: 50, OutputTokens: 503, CacheCreationInputTokens: 248,
				CacheReadInputTokens: 1800},
			totalIn: 2098, totalOut: 503, cacheRead: 1800, cacheWrite: 248, uncached: 50, total: 2601,
		},
		{
			name: "anthropic adds compaction input, cache and output counts",
			u: Usage{Family: Anthropic, InputTokens: 23000, OutputTokens: 1000, CacheCreationInputTokens: 200,
				CacheReadInputTokens: 1000, CompactionInputTokens: 180000, CompactionOutputTokens: 3500,
				CompactionCacheCreationInputTokens: 400, CompactionCacheReadInputTokens: 50000},
			totalIn: 254600, totalOut: 4500, cacheRead: 51000, cacheWrite: 600, uncached: 203000, total: 259100,
		},
		{
			name: "anthropic adds compaction and advisor input, cache and output counts",
			u: Usage{Family: Anthropic, InputTokens: 1760, OutputTokens: 531, CacheCreationInputTokens: 100,
				CacheReadInputTokens: 412, CompactionInputTokens: 500, CompactionOutputTokens: 200,
				CompactionCacheCreationInputTokens: 50, CompactionCacheReadInputTokens: 300,
				AdvisorInputTokens: 823, AdvisorOutputTokens: 1612, AdvisorCacheCreationInputTokens: 40,
				AdvisorCacheReadInputTokens: 120},
			totalIn: 4105, totalOut: 2343, cacheRead: 832, cacheWrite: 190, uncached: 3083, total: 6448,
		},
		{
			name:    "anthropic without cache",
			u:       Usage{Family: Anthropic, InputTokens: 25, OutputTokens: 15},
			totalIn: 25, totalOut: 15, uncached: 25, total: 40,
		},
		{
			name: "chat counts cached tokens inside prompt_tokens",
			u: Usage{Family: OpenAIChat, InputTokens: 2006, OutputTokens: 300, CachedPromptTokens: 1920,
				ReasoningTokens: 192},
			totalIn: 2006, totalOut: 300, cacheRead: 1920, uncached: 86, total: 2306,
		},
		{
			name: "responses counts reads and writes inside input_tokens",
			u: Usage{Family: OpenAIResponses, InputTokens: 12000, OutputTokens: 20, CachedPromptTokens: 4000,
				CacheWritePromptTokens: 7000},
			totalIn: 12000, totalOut: 20, cacheRead: 4000, cacheWrite: 7000, uncached: 1000, total: 12020,
		},
		{
			name:    "chat total_tokens above sum raises TotalTokens only",
			u:       Usage{Family: OpenAIChat, InputTokens: 9, OutputTokens: 2, ReportedTotalTokens: 20},
			totalIn: 9, totalOut: 2, uncached: 9, total: 20,
		},
		{
			name:    "responses total_tokens above sum raises TotalTokens only",
			u:       Usage{Family: OpenAIResponses, InputTokens: 9, OutputTokens: 2, ReportedTotalTokens: 20},
			totalIn: 9, totalOut: 2, uncached: 9, total: 20,
		},
		{
			name: "anthropic unclassified iterations stay outside totals",
			u: Usage{Family: Anthropic, InputTokens: 1, OutputTokens: 1,
				UnclassifiedIterationInputTokens: 5000, UnclassifiedIterationOutputTokens: 5000},
			totalIn: 1, totalOut: 1, uncached: 1, total: 2,
		},
		{name: "zero", u: Usage{Family: OpenAIResponses}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := tt.u
			got := [6]int64{u.TotalInputTokens(), u.TotalOutputTokens(), u.CacheReadTokens(), u.CacheWriteTokens(),
				u.UncachedInputTokens(), u.TotalTokens()}
			want := [6]int64{tt.totalIn, tt.totalOut, tt.cacheRead, tt.cacheWrite, tt.uncached, tt.total}
			if got != want {
				t.Errorf("totals (in, out, read, write, uncached, total) = %v, want %v", got, want)
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

// TestBreakdownsArePartsOfTheirParent checks that a breakdown never adds to the totals, that validate refuses
// a breakdown larger than a parent the reference bounds it by, and that it accepts the cache write lifetimes
// as reported.
func TestBreakdownsArePartsOfTheirParent(t *testing.T) {
	anthropic := Usage{Family: Anthropic, InputTokens: 50, OutputTokens: 500, CacheCreationInputTokens: 248,
		CacheReadInputTokens: 1800}
	withAnthropic := anthropic
	withAnthropic.CacheCreation5mInputTokens, withAnthropic.CacheCreation1hInputTokens = 148, 100
	withAnthropic.CompactionCacheCreation5mInputTokens, withAnthropic.CompactionCacheCreation1hInputTokens = 20, 10
	withAnthropic.AdvisorCacheCreation5mInputTokens, withAnthropic.AdvisorCacheCreation1hInputTokens = 15, 5
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
	// Above their parent: the thinking, reasoning and cache tokens are refused, the cache write lifetimes are
	// reported as given.
	tests := []struct {
		name   string
		u      Usage
		change func(*Usage)
		ok     bool
	}{
		{name: "thinking above output", u: withAnthropic, change: func(u *Usage) { u.ReasoningTokens = u.OutputTokens + 1 }},
		{name: "reasoning above output", u: withResponses,
			change: func(u *Usage) { u.ReasoningTokens = u.OutputTokens + 1 }},
		{name: "cache reads and writes above input", u: withResponses,
			change: func(u *Usage) { u.CachedPromptTokens = u.InputTokens }},
		{name: "5m lifetime above writes", u: withAnthropic, change: func(u *Usage) { u.CacheCreation5mInputTokens++ },
			ok: true},
		{name: "1h lifetime without writes", u: withAnthropic, change: func(u *Usage) { u.CacheCreationInputTokens = 0 },
			ok: true},
		{name: "compaction lifetime without writes", u: withAnthropic,
			change: func(u *Usage) { u.CompactionCacheCreationInputTokens = 0 }, ok: true},
		{name: "advisor lifetime without writes", u: withAnthropic,
			change: func(u *Usage) { u.AdvisorCacheCreationInputTokens = 0 }, ok: true},
		{name: "30m lifetime above writes", u: withResponses, change: func(u *Usage) { u.CacheWrite30mPromptTokens++ },
			ok: true},
		{name: "12h lifetime without writes", u: withResponses, change: func(u *Usage) { u.CacheWritePromptTokens = 0 },
			ok: true},
		{name: "reported total below parts", u: withResponses,
			change: func(u *Usage) { u.ReportedTotalTokens = u.TotalTokens() - 1 }},
		{name: "reported total above parts", u: withResponses,
			change: func(u *Usage) { u.ReportedTotalTokens = u.TotalTokens() + 10 }, ok: true},
		{name: "unclassified input negative", u: withAnthropic,
			change: func(u *Usage) { u.UnclassifiedIterationInputTokens = -1 }},
		{name: "unclassified output negative", u: withAnthropic,
			change: func(u *Usage) { u.UnclassifiedIterationOutputTokens = -1 }},
		{name: "unclassified input above max", u: withAnthropic,
			change: func(u *Usage) { u.UnclassifiedIterationInputTokens = (1 << 40) + 1 }},
		{name: "unclassified output above max", u: withAnthropic,
			change: func(u *Usage) { u.UnclassifiedIterationOutputTokens = (1 << 40) + 1 }},
		{name: "unclassified counts at max", u: withAnthropic,
			change: func(u *Usage) {
				u.UnclassifiedIterationInputTokens = 1 << 40
				u.UnclassifiedIterationOutputTokens = 1 << 40
			}, ok: true},
	}
	for _, tt := range tests {
		u := tt.u
		tt.change(&u)
		err := u.validate(1 << 40)
		if tt.ok != (err == nil) || (err != nil && !errors.Is(err, ErrOutOfRange)) {
			t.Errorf("%s: validate = %v, want nil: %v", tt.name, err, tt.ok)
		}
	}
}

func TestCheckRangeNoInt64Overflow(t *testing.T) {
	// Nine Anthropic input fields each at MaxTokensCeiling would wrap int64 if summed directly.
	u := Usage{
		Family:                             Anthropic,
		InputTokens:                        MaxTokensCeiling,
		CacheCreationInputTokens:           MaxTokensCeiling,
		CacheReadInputTokens:               MaxTokensCeiling,
		CompactionInputTokens:              MaxTokensCeiling,
		CompactionCacheCreationInputTokens: MaxTokensCeiling,
		CompactionCacheReadInputTokens:     MaxTokensCeiling,
		AdvisorInputTokens:                 MaxTokensCeiling,
		AdvisorCacheCreationInputTokens:    MaxTokensCeiling,
		AdvisorCacheReadInputTokens:        MaxTokensCeiling,
	}
	if err := u.checkRange(MaxTokensCeiling); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("checkRange = %v, want ErrOutOfRange", err)
	}
}
