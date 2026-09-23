package sum

import "testing"

// Hidden acceptance test: overlaid by the verifier, never shown to the agent.
func TestAcceptanceSumIncludesNegatives(t *testing.T) {
	cases := []struct {
		in   []int
		want int
	}{{[]int{5, -2}, 3}, {[]int{-1, -1}, -2}, {nil, 0}}
	for _, c := range cases {
		if got := Sum(c.in); got != c.want {
			t.Fatalf("Sum(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
