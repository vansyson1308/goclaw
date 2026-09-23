//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	httppkg "github.com/nextlevelbuilder/goclaw/internal/http"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

type evoHTTP struct {
	h        *httppkg.EvolutionHandler
	tenantID uuid.UUID
	agentID  uuid.UUID
}

func (e evoHTTP) patch(t *testing.T, sgID uuid.UUID, user string, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	ctx := store.WithUserID(tenantCtx(e.tenantID), user)
	req := httptest.NewRequest(http.MethodPatch, "/v1/agents/"+e.agentID.String()+"/evolution/suggestions/"+sgID.String(), bytes.NewReader(b)).WithContext(ctx)
	req.SetPathValue("agentID", e.agentID.String())
	req.SetPathValue("suggestionID", sgID.String())
	w := httptest.NewRecorder()
	e.h.HandleUpdateSuggestionForTest(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (e evoHTTP) events(t *testing.T, sgID uuid.UUID) []store.EvolutionEvent {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(tenantCtx(e.tenantID))
	req.SetPathValue("agentID", e.agentID.String())
	req.SetPathValue("suggestionID", sgID.String())
	w := httptest.NewRecorder()
	e.h.HandleListEventsForTest(w, req)
	var out []store.EvolutionEvent
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("events: %d %s", w.Code, w.Body.String())
	}
	return out
}

func newEvoHTTP(t *testing.T) (evoHTTP, *pg.PGEvolutionSuggestionStore, *pg.PGEvolutionMetricsStore) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	ss := pg.NewPGEvolutionSuggestionStore(db)
	ms := pg.NewPGEvolutionMetricsStore(db)
	h := httppkg.NewEvolutionHandler(ms, ss, httppkg.WithAgentStore(pg.NewPGAgentStore(db)))
	return evoHTTP{h: h, tenantID: tenantID, agentID: agentID}, ss, ms
}

func recordToolCalls(t *testing.T, ms *pg.PGEvolutionMetricsStore, tenantID, agentID uuid.UUID, tool string, n int) {
	t.Helper()
	ctx := tenantCtx(tenantID)
	for range n {
		pg.RecordToolMetric(ctx, ms, agentID, "s", tool, false, 5)
	}
}

func TestEvolutionHTTP_ToolOrderApproveRollbackAndAudit(t *testing.T) {
	e, ss, ms := newEvoHTTP(t)
	db := testDB(t)
	// Per-agent guardrail: 5 data points suffice (default would need 100).
	if _, err := db.Exec(`UPDATE agents SET other_config = '{"evolution_guardrails":{"min_data_points":5}}' WHERE id = $1`, e.agentID); err != nil {
		t.Fatal(err)
	}
	sg := evoToolOrder(t, tenantCtx(e.tenantID), ss, e.agentID, "web_fetch")

	// Not enough data yet → guardrail blocks, nothing changes.
	recordToolCalls(t, ms, e.tenantID, e.agentID, "web_fetch", 3)
	if code, body := e.patch(t, sg.ID, "alice", map[string]any{"status": "approved"}); code != http.StatusBadRequest {
		t.Fatalf("guardrail: want 400, got %d %v", code, body)
	}
	recordToolCalls(t, ms, e.tenantID, e.agentID, "web_fetch", 3)

	// Client-supplied reviewed_by must not become the audit actor.
	code, body := e.patch(t, sg.ID, "alice", map[string]any{"status": "approved", "reviewed_by": "mallory"})
	if code != http.StatusOK || body["action"] != "tool_denied_for_agent" {
		t.Fatalf("approve: %d %v", code, body)
	}
	if got := agentColumn(t, db, e.agentID, "tools_config"); !sameJSON(got, json.RawMessage(`{"deny":["web_fetch"]}`)) {
		t.Fatalf("tools_config = %s", got)
	}
	// Approving again is a conflict, not a silent re-apply.
	if code, _ := e.patch(t, sg.ID, "alice", map[string]any{"status": "approved"}); code != http.StatusConflict {
		t.Fatalf("repeat approve: want 409, got %d", code)
	}
	// REST rolled_back really restores config.
	code, body = e.patch(t, sg.ID, "bob", map[string]any{"status": "rolled_back", "reason": "regression"})
	if code != http.StatusOK {
		t.Fatalf("rollback: %d %v", code, body)
	}
	if got := agentColumn(t, db, e.agentID, "tools_config"); !sameJSON(got, json.RawMessage(`{}`)) {
		t.Fatalf("rollback did not restore config: %s", got)
	}
	ev := e.events(t, sg.ID)
	if len(ev) != 2 || ev[0].Actor != "alice" || ev[1].Actor != "bob" || ev[1].Action != "rollback" {
		t.Fatalf("audit: %+v", ev)
	}
	stored, _ := ss.GetSuggestion(tenantCtx(e.tenantID), sg.ID)
	if stored.ReviewedBy == "mallory" || stored.AppliedBy != "alice" {
		t.Fatalf("spoofed actor recorded: %+v", stored)
	}
}

func TestEvolutionHTTP_RolledBackOnPendingIsRejected(t *testing.T) {
	e, ss, _ := newEvoHTTP(t)
	sg := evoToolOrder(t, tenantCtx(e.tenantID), ss, e.agentID, "exec")
	if code, _ := e.patch(t, sg.ID, "u", map[string]any{"status": "rolled_back"}); code != http.StatusConflict {
		t.Fatalf("rolled_back on pending: want 409, got %d", code)
	}
	stored, _ := ss.GetSuggestion(tenantCtx(e.tenantID), sg.ID)
	if stored.Status != store.SuggestionPending {
		t.Fatalf("status changed: %s", stored.Status)
	}
}

func TestEvolutionHTTP_ThresholdIsAdvisory(t *testing.T) {
	e, ss, _ := newEvoHTTP(t)
	db := testDB(t)
	before := agentColumn(t, db, e.agentID, "other_config")
	sg := store.EvolutionSuggestion{
		ID: uuid.New(), AgentID: e.agentID, SuggestionType: store.SuggestThreshold,
		Suggestion: "raise", Rationale: "r", Parameters: json.RawMessage(`{"source":"memory_search"}`),
		Status: store.SuggestionPending,
	}
	if err := ss.CreateSuggestion(tenantCtx(e.tenantID), sg); err != nil {
		t.Fatal(err)
	}
	code, body := e.patch(t, sg.ID, "alice", map[string]any{"status": "approved"})
	if code != http.StatusOK || body["action"] != "acknowledged" {
		t.Fatalf("approve threshold: %d %v", code, body)
	}
	if got := agentColumn(t, db, e.agentID, "other_config"); !sameJSON(got, before) {
		t.Fatalf("advisory approval changed config: %s", got)
	}
	// Rejecting after acknowledgement is allowed; rollback is not (nothing applied).
	if code, _ := e.patch(t, sg.ID, "alice", map[string]any{"status": "rolled_back"}); code != http.StatusConflict {
		t.Fatalf("rollback of advisory: want 409, got %d", code)
	}
	if code, _ := e.patch(t, sg.ID, "alice", map[string]any{"status": "rejected"}); code != http.StatusOK {
		t.Fatalf("reject: want 200, got %d", code)
	}
}

func TestEvolutionHTTP_SkillAddClaimReleaseAndApply(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	ss := pg.NewPGEvolutionSuggestionStore(db)
	dir := t.TempDir()
	skillStore := pg.NewPGSkillStore(db, dir)
	h := httppkg.NewEvolutionHandler(pg.NewPGEvolutionMetricsStore(db), ss,
		httppkg.WithAgentStore(pg.NewPGAgentStore(db)),
		httppkg.WithSkillCreation(skillStore, skills.NewLoader(dir, "", ""), dir))
	e := evoHTTP{h: h, tenantID: tenantID, agentID: agentID}
	ctx := tenantCtx(tenantID)

	slug := "evo-skill-" + uuid.NewString()[:8]
	sg := store.EvolutionSuggestion{
		ID: uuid.New(), AgentID: agentID, SuggestionType: store.SuggestSkillAdd,
		Suggestion: "add skill", Rationale: "r", Parameters: json.RawMessage(`{"tool":"exec"}`),
		Status: store.SuggestionPending,
	}
	if err := ss.CreateSuggestion(ctx, sg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM skills WHERE slug = $1`, slug) })

	// No draft → validation fails after claim; claim is released to pending.
	if code, _ := e.patch(t, sg.ID, "alice", map[string]any{"status": "approved"}); code != http.StatusBadRequest {
		t.Fatalf("missing draft: want 400, got %d", code)
	}
	stored, _ := ss.GetSuggestion(ctx, sg.ID)
	if stored.Status != store.SuggestionPending {
		t.Fatalf("claim not released: %s", stored.Status)
	}
	ev := e.events(t, sg.ID)
	if len(ev) != 2 || ev[0].Action != "claim" || ev[1].Action != "apply_failed" {
		t.Fatalf("audit after failed apply: %+v", ev)
	}

	draft := "---\nname: " + slug + "\ndescription: test skill\n---\n# Steps\nRun the checks.\n"
	code, body := e.patch(t, sg.ID, "alice", map[string]any{"status": "approved", "skill_draft": draft})
	if code != http.StatusOK || body["action"] != "skill_created" {
		t.Fatalf("apply: %d %v", code, body)
	}
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM skills WHERE slug = $1`, slug).Scan(&n)
	if n != 1 {
		t.Fatalf("skill rows = %d, want 1", n)
	}
	stored, _ = ss.GetSuggestion(ctx, sg.ID)
	if stored.Status != store.SuggestionApplied || stored.AppliedBy != "alice" {
		t.Fatalf("after apply: %+v", stored)
	}
	// Second approval cannot create a duplicate skill.
	if code, _ := e.patch(t, sg.ID, "alice", map[string]any{"status": "approved", "skill_draft": draft}); code != http.StatusConflict {
		t.Fatalf("repeat approve: want 409, got %d", code)
	}
	_ = db.QueryRow(`SELECT count(*) FROM skills WHERE slug = $1`, slug).Scan(&n)
	if n != 1 {
		t.Fatalf("duplicate skill created: %d", n)
	}
	// Rollback of skill_add is explicitly unsupported (409), never status-only.
	if code, _ := e.patch(t, sg.ID, "alice", map[string]any{"status": "rolled_back"}); code != http.StatusConflict {
		t.Fatalf("skill_add rollback: want 409, got %d", code)
	}
}
