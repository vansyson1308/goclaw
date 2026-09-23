package sum

import "testing"

func TestSumPositive(t *testing.T) {
	if got := Sum([]int{1, 2, 3}); got != 6 {
		t.Fatalf("got %d", got)
	}
}
