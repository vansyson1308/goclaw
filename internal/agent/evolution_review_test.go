package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func TestSuppressesNewSuggestion(t *testing.T) {
	now := time.Now()
	recent, old := now.Add(-time.Hour), now.Add(-8*24*time.Hour)
	cases := []struct {
		sg   store.EvolutionSuggestion
		want bool
	}{
		{store.EvolutionSuggestion{Status: store.SuggestionPending}, true},
		{store.EvolutionSuggestion{Status: store.SuggestionApproved, ReviewedAt: &recent}, true},
		{store.EvolutionSuggestion{Status: store.SuggestionApproved, ReviewedAt: &old}, false},
		{store.EvolutionSuggestion{Status: store.SuggestionApplying}, true},
		{store.EvolutionSuggestion{Status: store.SuggestionApplied}, true},
		{store.EvolutionSuggestion{Status: store.SuggestionRejected, ReviewedAt: &recent}, true},
		{store.EvolutionSuggestion{Status: store.SuggestionRejected, ReviewedAt: &old}, false},
		{store.EvolutionSuggestion{Status: store.SuggestionRolledBack, RolledBackAt: &recent}, true},
		{store.EvolutionSuggestion{Status: store.SuggestionRolledBack, RolledBackAt: &old}, false},
	}
	for _, c := range cases {
		if got := suppressesNewSuggestion(c.sg, now); got != c.want {
			t.Errorf("status %s: got %v want %v", c.sg.Status, got, c.want)
		}
	}
}

// memSuggestions is a minimal in-memory store for engine tests.
type memSuggestions struct {
	store.EvolutionSuggestionStore
	rows []store.EvolutionSuggestion
}

func (m *memSuggestions) ListSuggestions(_ context.Context, _ uuid.UUID, status string, _ int) ([]store.EvolutionSuggestion, error) {
	var out []store.EvolutionSuggestion
	for _, r := range m.rows {
		if status == "" || r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memSuggestions) CreateSuggestion(_ context.Context, sg store.EvolutionSuggestion) error {
	m.rows = append(m.rows, sg)
	return nil
}

type fixedMetrics struct {
	store.EvolutionMetricsStore
	tools []store.ToolAggregate
}

func (f fixedMetrics) AggregateToolMetrics(context.Context, uuid.UUID, time.Time) ([]store.ToolAggregate, error) {
	return f.tools, nil
}

func (f fixedMetrics) AggregateRetrievalMetrics(context.Context, uuid.UUID, time.Time) ([]store.RetrievalAggregate, error) {
	return nil, nil
}

// Regression: only "pending" used to suppress duplicates, so an applied or
// just-rejected tool_order was re-proposed on every 6-hourly run.
func TestAnalyzeDoesNotReproposeReviewedSuggestion(t *testing.T) {
	failing := []store.ToolAggregate{{ToolName: "exec", CallCount: 50, SuccessRate: 0.02}}
	now := time.Now()
	for _, status := range []string{store.SuggestionApplied, store.SuggestionRejected} {
		sugs := &memSuggestions{rows: []store.EvolutionSuggestion{{
			SuggestionType: store.SuggestToolOrder, Status: status, ReviewedAt: &now,
			Parameters: json.RawMessage(`{"tool":"exec"}`),
		}}}
		e := NewSuggestionEngine(fixedMetrics{tools: failing}, sugs)
		created, err := e.Analyze(context.Background(), uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		if len(created) != 0 {
			t.Fatalf("status %s: re-proposed %d suggestions", status, len(created))
		}
	}
	// A different tool is still proposed.
	sugs := &memSuggestions{rows: []store.EvolutionSuggestion{{
		SuggestionType: store.SuggestToolOrder, Status: store.SuggestionApplied,
		Parameters: json.RawMessage(`{"tool":"browser"}`),
	}}}
	created, _ := NewSuggestionEngine(fixedMetrics{tools: failing}, sugs).Analyze(context.Background(), uuid.New())
	if len(created) != 1 {
		t.Fatalf("want 1 new suggestion for another tool, got %d", len(created))
	}
}

func TestGuardrailsForAgent(t *testing.T) {
	ag := &store.AgentData{OtherConfig: json.RawMessage(`{"evolution_guardrails":{"min_data_points":5,"max_delta_per_cycle":0,"locked_params":["tools_config.deny"]}}`)}
	g := GuardrailsForAgent(ag)
	if g.MinDataPoints != 5 {
		t.Errorf("MinDataPoints = %d, want 5 (per-agent)", g.MinDataPoints)
	}
	if g.MaxDeltaPerCycle != 0.1 {
		t.Errorf("non-positive MaxDeltaPerCycle must keep default, got %v", g.MaxDeltaPerCycle)
	}
	if err := CheckLockedTarget(g, "tools_config", []string{"deny"}); err == nil {
		t.Error("locked target must be rejected")
	}
	if d := GuardrailsForAgent(&store.AgentData{OtherConfig: json.RawMessage(`not json`)}); d.MinDataPoints != 100 {
		t.Errorf("malformed config must fall back to defaults, got %+v", d)
	}
}
