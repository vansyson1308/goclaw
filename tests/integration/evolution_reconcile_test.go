//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

func TestEvolutionReconcileReportsLegacyAndStuckRecords(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	ctx := tenantCtx(tenantID)
	ss := pg.NewPGEvolutionSuggestionStore(db)

	mk := func(typ store.SuggestionType, status, params string) uuid.UUID {
		id := uuid.New()
		if err := ss.CreateSuggestion(ctx, store.EvolutionSuggestion{
			ID: id, AgentID: agentID, SuggestionType: typ, Suggestion: "s", Rationale: "r",
			Parameters: json.RawMessage(params), Status: status,
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	legacyThreshold := mk(store.SuggestThreshold, "applied", `{"_baseline":{}}`)
	legacyTool := mk(store.SuggestToolOrder, "applied", `{"tool":"exec"}`)
	stuck := mk(store.SuggestSkillAdd, "applying", `{}`)
	healthy := mk(store.SuggestToolOrder, "pending", `{"tool":"browser"}`)
	if _, err := db.Exec(`UPDATE agent_evolution_suggestions SET created_at = $1 WHERE id = $2`, time.Now().Add(-time.Hour), stuck); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO builtin_tools (name, display_name, description, category, enabled)
		VALUES ('exec', 'exec', 'x', 'runtime', true) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("seed builtin tool: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO builtin_tool_tenant_configs (tool_name, tenant_id, enabled) VALUES ('exec', $1, false)`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM builtin_tool_tenant_configs WHERE tenant_id = $1`, tenantID) })

	findings, err := pg.EvolutionReconcile(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]string{}
	for _, f := range findings {
		if f.TenantID == tenantID && f.SuggestionID != nil {
			got[*f.SuggestionID] = f.Kind
		}
	}
	want := map[uuid.UUID]string{
		legacyThreshold: "legacy_threshold_applied",
		legacyTool:      "legacy_tenant_wide_tool_disable",
		stuck:           "stuck_applying",
	}
	for id, kind := range want {
		if got[id] != kind {
			t.Errorf("suggestion %s: kind %q, want %q", id, got[id], kind)
		}
	}
	if _, flagged := got[healthy]; flagged {
		t.Error("healthy pending suggestion must not be reported")
	}
	// Read-only: nothing changed.
	var status string
	_ = db.QueryRow(`SELECT status FROM agent_evolution_suggestions WHERE id = $1`, stuck).Scan(&status)
	if status != "applying" {
		t.Fatalf("report modified data: %s", status)
	}
}
