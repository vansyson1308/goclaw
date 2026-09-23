package providers

import "testing"

// A CLI provider hidden behind the model-fallback wrapper must still be
// recognised as running its own tools (mission tool guards cannot see them).
func TestFallbackWrapperReportsNativeToolCandidates(t *testing.T) {
	cli := &ClaudeCLIProvider{}
	plain := &OpenAIProvider{}
	cases := []struct {
		name string
		p    *ModelFallbackProvider
		want bool
	}{
		{"cli primary", NewModelFallbackProvider(FallbackCandidate{Provider: cli}, []FallbackCandidate{{Provider: plain}}, 2, false), true},
		{"cli fallback", NewModelFallbackProvider(FallbackCandidate{Provider: plain}, []FallbackCandidate{{Provider: cli}}, 2, false), true},
		{"no cli", NewModelFallbackProvider(FallbackCandidate{Provider: plain}, []FallbackCandidate{{Provider: plain}}, 2, false), false},
	}
	for _, tc := range cases {
		var n NativeToolExecutor = tc.p
		if got := n.ExecutesToolsNatively(); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
