package cmd

import (
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

// Review D/L1: a mix of priced and unpriced calls is an unknown cost, not
// the sum of the priced ones.
func TestMissionRunCostIsUnknownUnlessEveryCallIsPriced(t *testing.T) {
	if c := missionRunCost(nil); c != nil {
		t.Fatalf("no calls: %v", *c)
	}
	if c := missionRunCost([]providers.CallUsage{{CostUSD: 0.2}, {CostUSD: 0}}); c != nil {
		t.Fatalf("partially priced run reported as $%v", *c)
	}
	if c := missionRunCost([]providers.CallUsage{{CostUSD: 0.2}, {CostUSD: 0.05}}); c == nil || *c != 0.25 {
		t.Fatalf("fully priced run: %v", c)
	}
}
