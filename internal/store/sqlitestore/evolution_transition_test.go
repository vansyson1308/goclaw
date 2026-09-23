//go:build sqlite || sqliteonly

package sqlitestore

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func sqliteToolOrder(t *testing.T, ss *SQLiteEvolutionSuggestionStore, tenantID, agentID uuid.UUID, tool string) store.EvolutionSuggestion {
	t.Helper()
	sg := store.EvolutionSuggestion{
		ID: uuid.New(), AgentID: agentID, SuggestionType: store.SuggestToolOrder,
		Suggestion: "x", Rationale: "y", Parameters: json.RawMessage(`{"tool":"` + tool + `"}`),
		Status: store.SuggestionPending,
	}
	if err := ss.CreateSuggestion(sqliteTenantCtx(tenantID), sg); err != nil {
		t.Fatal(err)
	}
	got, err := ss.GetSuggestion(sqliteTenantCtx(tenantID), sg.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	return *got
}

func sqliteToolsConfig(t *testing.T, ss *SQLiteEvolutionSuggestionStore, agentID uuid.UUID) json.RawMessage {
	t.Helper()
	var raw []byte
	if err := ss.db.QueryRow(`SELECT COALESCE(tools_config, 'null') FROM agents WHERE id = ?`, agentID.String()).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSQLiteEvolutionApplyRollbackConflictAndConcurrency(t *testing.T) {
	db := newHookTestDB(t)
	tenantID, agentID := seedHookTenantAgent(t, db)
	ss := NewSQLiteEvolutionSuggestionStore(db)
	ctx := sqliteTenantCtx(tenantID)
	if _, err := db.Exec(`UPDATE agents SET tools_config = '{"profile":"coding"}' WHERE id = ?`, agentID.String()); err != nil {
		t.Fatal(err)
	}
	original := sqliteToolsConfig(t, ss, agentID)
	sg := sqliteToolOrder(t, ss, tenantID, agentID, "exec")

	// Concurrent apply: exactly one wins.
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := agent.ApplyToolOrder(ctx, ss, sg, "alice")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if !errors.Is(err, store.ErrSuggestionStateConflict) {
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("want exactly one apply, got %d", ok)
	}
	applied, _ := ss.GetSuggestion(ctx, sg.ID)
	if applied.Status != store.SuggestionApplied || applied.AppliedAt == nil || applied.AppliedChange == nil || applied.AppliedChange.Before.Present {
		t.Fatalf("applied state: %+v", applied)
	}

	// Newer unrelated edit → rollback refuses.
	if _, err := db.Exec(`UPDATE agents SET tools_config = '{"profile":"coding","deny":["exec","cron"]}' WHERE id = ?`, agentID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.RollbackSuggestion(ctx, ss, *applied, "bob", ""); !errors.Is(err, agent.ErrRollbackConflict) {
		t.Fatalf("want rollback conflict, got %v", err)
	}
	// Operator reverts their edit → rollback succeeds and restores absence.
	if _, err := db.Exec(`UPDATE agents SET tools_config = '{"profile":"coding","deny":["exec"]}' WHERE id = ?`, agentID.String()); err != nil {
		t.Fatal(err)
	}
	rolled, err := agent.RollbackSuggestion(ctx, ss, *applied, "bob", "")
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Status != store.SuggestionRolledBack || rolled.RolledBackBy != "bob" {
		t.Fatalf("rolled: %+v", rolled)
	}
	got := sqliteToolsConfig(t, ss, agentID)
	if !store.ConfigValuesEqual(store.ConfigValue{Present: true, Value: got}, store.ConfigValue{Present: true, Value: original}) {
		t.Fatalf("restored %s, want %s", got, original)
	}
	ev, err := ss.ListSuggestionEvents(ctx, sg.ID)
	if err != nil || len(ev) != 2 || ev[0].Action != "apply" || ev[1].Action != "rollback" || ev[1].Actor != "bob" {
		t.Fatalf("events: %+v %v", ev, err)
	}

	// Cross-tenant: a fresh pending suggestion in tenant A cannot be applied
	// or read from tenant B (would succeed if the tenant filter were missing).
	otherTenant, _ := seedHookTenantAgent(t, db)
	fresh := sqliteToolOrder(t, ss, tenantID, agentID, "browser")
	if _, err := agent.ApplyToolOrder(sqliteTenantCtx(otherTenant), ss, fresh, "x"); err == nil {
		t.Fatal("cross-tenant apply must fail")
	}
	if got, _ := ss.GetSuggestion(sqliteTenantCtx(otherTenant), fresh.ID); got != nil {
		t.Fatal("cross-tenant read must not see the suggestion")
	}
	if stored, _ := ss.GetSuggestion(ctx, fresh.ID); stored.Status != store.SuggestionPending {
		t.Fatalf("status changed by other tenant: %s", stored.Status)
	}
}

func TestSQLiteEvolutionFailedMutateWritesNothing(t *testing.T) {
	db := newHookTestDB(t)
	tenantID, agentID := seedHookTenantAgent(t, db)
	ss := NewSQLiteEvolutionSuggestionStore(db)
	ctx := sqliteTenantCtx(tenantID)
	sg := sqliteToolOrder(t, ss, tenantID, agentID, "exec")
	before := sqliteToolsConfig(t, ss, agentID)
	boom := errors.New("injected")
	_, err := ss.TransitionSuggestion(ctx, store.SuggestionTransition{
		ID: sg.ID, From: []string{store.SuggestionPending}, To: store.SuggestionApplied,
		Actor: "u", Action: "apply", Column: "tools_config",
		Mutate: func(*store.EvolutionSuggestion, json.RawMessage) (json.RawMessage, *store.AgentConfigChange, error) {
			return nil, nil, boom
		},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want injected, got %v", err)
	}
	stored, _ := ss.GetSuggestion(ctx, sg.ID)
	if stored.Status != store.SuggestionPending || stored.StateVersion != 0 {
		t.Fatalf("state changed: %+v", stored)
	}
	if got := sqliteToolsConfig(t, ss, agentID); string(got) != string(before) {
		t.Fatalf("config changed: %s", got)
	}
	if ev, _ := ss.ListSuggestionEvents(ctx, sg.ID); len(ev) != 0 {
		t.Fatalf("events written: %d", len(ev))
	}
}

// Existing Lite databases at v60 must upgrade in place.
func TestSQLiteEvolutionSchemaUpgradeFrom60(t *testing.T) {
	db := newHookTestDB(t)
	if _, err := db.Exec(`DROP TABLE agent_evolution_events`); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"applied_at", "applied_by", "rolled_back_at", "rolled_back_by", "applied_change", "state_version"} {
		if _, err := db.Exec(`ALTER TABLE agent_evolution_suggestions DROP COLUMN ` + col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	if _, err := db.Exec(`UPDATE schema_version SET version = 60`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatalf("upgrade 60→61: %v", err)
	}
	var v int
	_ = db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if v != SchemaVersion {
		t.Fatalf("version = %d, want %d", v, SchemaVersion)
	}
	tenantID, agentID := seedHookTenantAgent(t, db)
	ss := NewSQLiteEvolutionSuggestionStore(db)
	sg := sqliteToolOrder(t, ss, tenantID, agentID, "exec")
	if _, err := agent.ApplyToolOrder(sqliteTenantCtx(tenantID), ss, sg, "u"); err != nil {
		t.Fatalf("apply after upgrade: %v", err)
	}
}
