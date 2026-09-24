package providers

import (
	"encoding/json"
	"testing"
)

// DeepSeek reports cache hits as prompt_cache_hit_tokens (prompt_tokens =
// hits + misses) rather than prompt_tokens_details.cached_tokens. Without
// reading it, cache hits are billed at the full input price.
func TestDeepSeekCacheHitTokensAreCacheReads(t *testing.T) {
	var u openAIUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,
		"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":200,
		"completion_tokens_details":{"reasoning_tokens":30}}`), &u); err != nil {
		t.Fatal(err)
	}
	got := &Usage{PromptTokens: u.PromptTokens}
	fillCacheUsage(got, &u)
	if got.CacheReadTokens != 800 || !got.PromptTokensIncludeCachedSegments {
		t.Fatalf("cache hits not read: %+v", got)
	}

	// The OpenAI-style field keeps working and wins when present.
	var o openAIUsage
	_ = json.Unmarshal([]byte(`{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":40}}`), &o)
	got = &Usage{}
	fillCacheUsage(got, &o)
	if got.CacheReadTokens != 40 || !got.PromptTokensIncludeCachedSegments {
		t.Fatalf("openai cached_tokens: %+v", got)
	}
}
