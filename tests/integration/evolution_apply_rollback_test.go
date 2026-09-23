//go:build integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

func evoToolOrder(t *testing.T, ctx context.Context, ss store.EvolutionSuggestionStore, agentID uuid.UUID, tool string) store.EvolutionSuggestion {
	t.Helper()
	sg := store.EvolutionSuggestion{
		ID: uuid.New(), AgentID: agentID, SuggestionType: store.SuggestToolOrder,
		Suggestion: "Disable " + tool, Rationale: "test",
		Parameters: json.RawMessage(`{"tool":"` + tool + `","success_rate":0.05}`),
		Status:     store.SuggestionPending,
	}
	if err := ss.CreateSuggestion(ctx, sg); err != nil {
		t.Fatalf("CreateSuggestion: %v", err)
	}
	got, err := ss.GetSuggestion(ctx, sg.ID)
	if err != nil || got == nil {
		t.Fatalf("GetSuggestion: %v", err)
	}
	return *got
}

func agentColumn(t *testing.T, db *sql.DB, agentID uuid.UUID, col string) json.RawMessage {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(`SELECT COALESCE(`+col+`::text, 'null') FROM agents WHERE id = $1`, agentID).Scan(&raw); err != nil {
		t.Fatalf("read %s: %v", col, err)
	}
	return raw
}

func sameJSON(a, b json.RawMessage) bool {
	return store.ConfigValuesEqual(store.ConfigValue{Present: true, Value: a}, store.ConfigValue{Present: true, Value: b})
}

func TestEvolutionToolOrder_ApplyIsAgentScopedAndRollbackRestoresAbsence(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	ctx := tenantCtx(tenantID)
	ss := pg.NewPGEvolutionSuggestionStore(db)

	// A second agent in the same tenant must be untouched.
	otherID := uuid.New()
	if _, err := db.Exec(`INSERT INTO agents (id, tenant_id, agent_key, agent_type, status, provider, model, owner_id, tools_config)
		VALUES ($1, $2, $3, 'predefined', 'active', 'test', 'm', 'o', '{"profile":"coding"}')`,
		otherID, tenantID, "other-"+otherID.String()[:8]); err != nil {
		t.Fatalf("seed other agent: %v", err)
	}
	if _, err := db.Exec(`UPDATE agents SET tools_config = '{"profile":"coding"}' WHERE id = $1`, agentID); err != nil {
		t.Fatal(err)
	}
	original := agentColumn(t, db, agentID, "tools_config")

	sg := evoToolOrder(t, ctx, ss, agentID, "web_fetch")
	applied, err := agent.ApplyToolOrder(ctx, ss, sg, "user-alice")
	if err != nil {
		t.Fatalf("ApplyToolOrder: %v", err)
	}
	if applied.Status != store.SuggestionApplied || applied.AppliedAt == nil || applied.AppliedBy != "user-alice" {
		t.Fatalf("applied bookkeeping wrong: %+v", applied)
	}
	if c := applied.AppliedChange; c == nil || c.Before.Present || !c.After.Present {
		t.Fatalf("change must record absent→present: %+v", c)
	}
	if got := agentColumn(t, db, agentID, "tools_config"); !sameJSON(got, json.RawMessage(`{"profile":"coding","deny":["web_fetch"]}`)) {
		t.Fatalf("tools_config after apply = %s", got)
	}
	if got := agentColumn(t, db, otherID, "tools_config"); !sameJSON(got, json.RawMessage(`{"profile":"coding"}`)) {
		t.Fatalf("other agent modified: %s", got)
	}
	var tenantDisables int
	_ = db.QueryRow(`SELECT count(*) FROM builtin_tool_tenant_configs WHERE tenant_id = $1`, tenantID).Scan(&tenantDisables)
	if tenantDisables != 0 {
		t.Fatalf("tenant-wide tool config must not change, got %d rows", tenantDisables)
	}

	// Stored state agrees with the returned state.
	stored, _ := ss.GetSuggestion(ctx, sg.ID)
	if stored.Status != store.SuggestionApplied || stored.AppliedChange == nil || stored.StateVersion != applied.StateVersion {
		t.Fatalf("stored state disagrees: %+v", stored)
	}

	rolled, err := agent.RollbackSuggestion(ctx, ss, *stored, "user-bob", "regression")
	if err != nil {
		t.Fatalf("RollbackSuggestion: %v", err)
	}
	if rolled.Status != store.SuggestionRolledBack || rolled.RolledBackBy != "user-bob" || rolled.RolledBackAt == nil {
		t.Fatalf("rollback bookkeeping wrong: %+v", rolled)
	}
	if got := agentColumn(t, db, agentID, "tools_config"); !sameJSON(got, original) {
		t.Fatalf("rollback must restore exact original (deny absent): got %s want %s", got, original)
	}

	events, err := ss.ListSuggestionEvents(ctx, sg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Action != "apply" || events[0].Actor != "user-alice" ||
		events[1].Action != "rollback" || events[1].Actor != "user-bob" || events[1].FromStatus != "applied" {
		t.Fatalf("audit trail wrong: %+v", events)
	}
}

func TestEvolutionToolOrder_RollbackRestoresPreexistingList(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	ctx := tenantCtx(tenantID)
	ss := pg.NewPGEvolutionSuggestionStore(db)
	if _, err := db.Exec(`UPDATE agents SET tools_config = '{"deny":["exec"]}' WHERE id = $1`, agentID); err != nil {
		t.Fatal(err)
	}
	sg := evoToolOrder(t, ctx, ss, agentID, "browser")
	applied, err := agent.ApplyToolOrder(ctx, ss, sg, "u")
	if err != nil {
		t.Fatal(err)
	}
	if got := agentColumn(t, db, agentID, "tools_config"); !sameJSON(got, json.RawMessage(`{"deny":["exec","browser"]}`)) {
		t.Fatalf("after apply = %s", got)
	}
	if _, err := agent.RollbackSuggestion(ctx, ss, *applied, "u", ""); err != nil {
		t.Fatal(err)
	}
	if got := agentColumn(t, db, agentID, "tools_config"); !sameJSON(got, json.RawMessage(`{"deny":["exec"]}`)) {
		t.Fatalf("after rollback = %s", got)
	}
}

func TestEvolutionToolOrder_ConcurrentApplyExactlyOnce(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	ctx := tenantCtx(tenantID)
	ss := pg.NewPGEvolutionSuggestionStore(db)
	sg := evoToolOrder(t, ctx, ss, agentID, "exec")

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, conflicts int
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := agent.ApplyToolOrder(ctx, ss, sg, "u")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, store.ErrSuggestionStateConflict):
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || conflicts != n-1 {
		t.Fatalf("want exactly one apply, got ok=%d conflicts=%d", ok, conflicts)
	}
	if got := agentColumn(t, db, agentID, "tools_config"); !sameJSON(got, json.RawMessage(`{"deny":["exec"]}`)) {
		t.Fatalf("deny list must contain the tool once: %s", got)
	}
	events, _ := ss.ListSuggestionEvents(ctx, sg.ID)
	if len(events) != 1 {
		t.Fatalf("want one audit event, got %d", len(events))
	}
}

func TestEvolutionToolOrder_RepeatApplyAndRollbackAreRejected(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	ctx := tenantCtx(tenantID)
	ss := pg.NewPGEvolutionSuggestionStore(db)
	sg := evoToolOrder(t, ctx, ss, agentID, "exec")

	// Rollback before apply: nothing to roll back.
	if _, err := agent.RollbackSuggestion(ctx, ss, sg, "u", ""); !errors.Is(err, agent.ErrNotApplicable) {
		t.Fatalf("rollback of pending: want ErrNotApplicable, got %v", err)
	}
	applied, err := agent.ApplyToolOrder(ctx, ss, sg, "u")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ApplyToolOrder(ctx, ss, sg, "u"); !errors.Is(err, store.ErrSuggestionStateConflict) {
		t.Fatalf("repeat apply: want conflict, got %v", err)
	}
	if _, err := agent.RollbackSuggestion(ctx, ss, *applied, "u", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.RollbackSuggestion(ctx, ss, *applied, "u", ""); !errors.Is(err, store.ErrSuggestionStateConflict) {
		t.Fatalf("repeat rollback: want conflict, got %v", err)
	}
	if got := agentColumn(t, db, agentID, "tools_config"); !sameJSON(got, json.RawMessage(`{}`)) {
		t.Fatalf("config after repeat attempts = %s", got)
	}
}

// A newer, unrelated edit to the same key must not be overwritten by rollback.
func TestEvolutionToolOrder_RollbackRefusesToOverwriteNewerChange(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	ctx := tenantCtx(tenantID)
	ss := pg.NewPGEvolutionSuggestionStore(db)
	sg := evoToolOrder(t, ctx, ss, agentID, "exec")
	applied, err := agent.ApplyToolOrder(ctx, ss, sg, "u")
	if err != nil {
		t.Fatal(err)
	}
	// Operator later adds another tool to the same deny list.
	if _, err := db.Exec(`UPDATE agents SET tools_config = '{"deny":["exec","cron"]}' WHERE id = $1`, agentID); err != nil {
		t.Fatal(err)
	}
	_, err = agent.RollbackSuggestion(ctx, ss, *applied, "u", "")
	if !errors.Is(err, agent.ErrRollbackConflict) {
		t.Fatalf("want ErrRollbackConflict, got %v", err)
	}
	if got := agentColumn(t, db, agentID, "tools_config"); !sameJSON(got, json.RawMessage(`{"deny":["exec","cron"]}`)) {
		t.Fatalf("operator edit overwritten: %s", got)
	}
	stored, _ := ss.GetSuggestion(ctx, sg.ID)
	if stored.Status != store.SuggestionApplied {
		t.Fatalf("status must stay applied after refused rollback, got %s", stored.Status)
	}
	if events, _ := ss.ListSuggestionEvents(ctx, sg.ID); len(events) != 1 {
		t.Fatalf("refused rollback must not write an event, got %d", len(events))
	}
}

// Injected failure inside the transaction: nothing is written.
func TestEvolutionTransition_FailureWritesNothing(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	ctx := tenantCtx(tenantID)
	ss := pg.NewPGEvolutionSuggestionStore(db)
	sg := evoToolOrder(t, ctx, ss, agentID, "exec")
	before := agentColumn(t, db, agentID, "tools_config")

	boom := errors.New("injected")
	_, err := ss.TransitionSuggestion(ctx, store.SuggestionTransition{
		ID: sg.ID, From: []string{store.SuggestionPending}, To: store.SuggestionApplied,
		Actor: "u", Action: "apply", Column: "tools_config",
		Mutate: func(_ *store.EvolutionSuggestion, cur json.RawMessage) (json.RawMessage, *store.AgentConfigChange, error) {
			return nil, nil, boom
		},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want injected error, got %v", err)
	}
	// Failure after the agent row was written (bad audit actor is rejected
	// up front, so use an unknown column to fail inside the tx instead).
	_, err = ss.TransitionSuggestion(ctx, store.SuggestionTransition{
		ID: sg.ID, From: []string{store.SuggestionPending}, To: store.SuggestionApplied,
		Actor: "u", Action: "apply", Column: "tools_config",
		Mutate: func(_ *store.EvolutionSuggestion, cur json.RawMessage) (json.RawMessage, *store.AgentConfigChange, error) {
			return json.RawMessage(`not json`), &store.AgentConfigChange{}, nil
		},
	})
	if err == nil {
		t.Fatal("invalid JSON write must fail")
	}
	stored, _ := ss.GetSuggestion(ctx, sg.ID)
	if stored.Status != store.SuggestionPending || stored.StateVersion != 0 {
		t.Fatalf("suggestion changed after failed transitions: %+v", stored)
	}
	if got := agentColumn(t, db, agentID, "tools_config"); !sameJSON(got, before) {
		t.Fatalf("agent config changed after failed transitions: %s", got)
	}
	if events, _ := ss.ListSuggestionEvents(ctx, sg.ID); len(events) != 0 {
		t.Fatalf("failed transitions must not write events, got %d", len(events))
	}
	// Disallowed column is rejected before any write.
	if _, err := ss.TransitionSuggestion(ctx, store.SuggestionTransition{
		ID: sg.ID, From: []string{store.SuggestionPending}, To: store.SuggestionApplied,
		Actor: "u", Column: "owner_id",
		Mutate: func(*store.EvolutionSuggestion, json.RawMessage) (json.RawMessage, *store.AgentConfigChange, error) {
			return json.RawMessage(`"x"`), nil, nil
		},
	}); err == nil {
		t.Fatal("non-allowlisted column must be rejected")
	}
}

func TestEvolutionTransition_TenantIsolation(t *testing.T) {
	db := testDB(t)
	tenantA, agentA := seedTenantAgent(t, db)
	tenantB, _ := seedTenantAgent(t, db)
	ss := pg.NewPGEvolutionSuggestionStore(db)
	sg := evoToolOrder(t, tenantCtx(tenantA), ss, agentA, "exec")

	if _, err := agent.ApplyToolOrder(tenantCtx(tenantB), ss, sg, "attacker"); err == nil {
		t.Fatal("cross-tenant apply must fail")
	}
	if got, _ := ss.GetSuggestion(tenantCtx(tenantB), sg.ID); got != nil {
		t.Fatal("cross-tenant read must not see the suggestion")
	}
	if ev, _ := ss.ListSuggestionEvents(tenantCtx(tenantB), sg.ID); len(ev) != 0 {
		t.Fatal("cross-tenant events leak")
	}
	stored, _ := ss.GetSuggestion(tenantCtx(tenantA), sg.ID)
	if stored.Status != store.SuggestionPending {
		t.Fatalf("status changed by other tenant: %s", stored.Status)
	}
}

// Rows applied by the old code kept a presence-less _baseline. Absent key →
// rollback deletes; explicit zero → rollback restores zero (not "missing").
func TestEvolutionLegacyThresholdRollback(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	ctx := tenantCtx(tenantID)
	ss := pg.NewPGEvolutionSuggestionStore(db)

	cases := []struct {
		name, baseline, want string
	}{
		{"absent", `{}`, `{"keep":1}`},
		{"explicit-zero", `{"retrieval_threshold":0}`, `{"keep":1,"retrieval_threshold":0}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Exec(`UPDATE agents SET other_config = '{"keep":1,"retrieval_threshold":0.4}' WHERE id = $1`, agentID); err != nil {
				t.Fatal(err)
			}
			sg := store.EvolutionSuggestion{
				ID: uuid.New(), AgentID: agentID, SuggestionType: store.SuggestThreshold,
				Suggestion: "legacy", Rationale: "legacy",
				Parameters: json.RawMessage(`{"source":"memory_search","_baseline":` + tc.baseline + `}`),
				Status:     store.SuggestionApplied,
			}
			if err := ss.CreateSuggestion(ctx, sg); err != nil {
				t.Fatal(err)
			}
			got, _ := ss.GetSuggestion(ctx, sg.ID)
			if _, err := agent.RollbackSuggestion(ctx, ss, *got, "u", "legacy"); err != nil {
				t.Fatalf("legacy rollback: %v", err)
			}
			if oc := agentColumn(t, db, agentID, "other_config"); !sameJSON(oc, json.RawMessage(tc.want)) {
				t.Fatalf("other_config = %s, want %s", oc, tc.want)
			}
		})
	}
}

func TestEvolutionAdvisoryThresholdChangesNoConfig(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	ctx := tenantCtx(tenantID)
	ss := pg.NewPGEvolutionSuggestionStore(db)
	before := agentColumn(t, db, agentID, "other_config")
	sg := store.EvolutionSuggestion{
		ID: uuid.New(), AgentID: agentID, SuggestionType: store.SuggestThreshold,
		Suggestion: "raise", Rationale: "r", Parameters: json.RawMessage(`{"source":"memory_search"}`),
		Status: store.SuggestionPending,
	}
	if err := ss.CreateSuggestion(ctx, sg); err != nil {
		t.Fatal(err)
	}
	got, err := agent.AcknowledgeAdvisory(ctx, ss, sg, "u", "advisory")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.SuggestionApproved || got.AppliedAt != nil || got.ReviewedBy != "u" {
		t.Fatalf("advisory ack wrong: %+v", got)
	}
	if oc := agentColumn(t, db, agentID, "other_config"); !sameJSON(oc, before) {
		t.Fatalf("advisory approval changed config: %s", oc)
	}
}

// Regression: the cron listed agents with a bare context, which fails closed.
func TestListEvolutionAgentsIteratesTenants(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	if _, err := db.Exec(`UPDATE agents SET other_config = '{"self_evolution_metrics":true,"self_evolution_suggestions":true}' WHERE id = $1`, agentID); err != nil {
		t.Fatal(err)
	}
	_ = tenantID
	agents := pg.NewPGAgentStore(db)
	if bare, _ := agents.List(context.Background(), ""); len(bare) != 0 {
		t.Fatalf("precondition: bare-context List is expected to fail closed, got %d", len(bare))
	}
	list, err := agent.ListEvolutionAgents(context.Background(), pg.NewPGTenantStore(db), agents)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range list {
		if a.ID == agentID {
			found = true
		}
	}
	if !found {
		t.Fatalf("evolution-enabled agent not listed (got %d agents)", len(list))
	}
}
