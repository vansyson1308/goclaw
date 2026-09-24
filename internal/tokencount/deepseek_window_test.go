package tokencount

import "testing"

// DeepSeek V4.x (deepseek-flash = V4.1 Flash, deepseek-v4-pro) have a 1M-token
// context; older deepseek-* models keep the 128K entry.
func TestDeepSeekContextWindows(t *testing.T) {
	c := &FallbackCounter{}
	for model, want := range map[string]int{
		"deepseek-flash":  1_000_000,
		"deepseek-v4-pro": 1_000_000,
		"deepseek-coder":  128_000,
	} {
		if got := c.ModelContextWindow(model); got != want {
			t.Errorf("%s: context window %d, want %d", model, got, want)
		}
	}
}
