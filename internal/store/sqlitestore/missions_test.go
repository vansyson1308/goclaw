//go:build sqlite || sqliteonly

package sqlitestore

import (
	"errors"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func TestSQLiteMissionStoreLifecycle(t *testing.T) {
	db := newHookTestDB(t)
	tenantA, _ := seedHookTenantAgent(t, db)
	tenantB, _ := seedHookTenantAgent(t, db)
	ms := NewSQLiteMissionStore(db)
	ctxA, ctxB := sqliteTenantCtx(tenantA), sqliteTenantCtx(tenantB)

	m := &store.Mission{OwnerID: "alice", AgentKey: "coder", Title: "t", Contract: []byte(`{"version":1}`), ContractDigest: "d"}
	if err := ms.CreateMission(ctxA, m, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPlanned}, store.MissionPreparing, "s", "", store.MissionUpdate{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPlanned}, store.MissionPreparing, "s", "", store.MissionUpdate{}); !errors.Is(err, store.ErrMissionStateConflict) {
		t.Fatalf("repeat transition: want conflict, got %v", err)
	}
	cost := 0.5
	now := time.Now().UTC()
	got, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPreparing}, store.MissionFailed, "s", "x", store.MissionUpdate{
		CostUSD: &cost, FinishedAt: &now, ChangedFiles: []string{"a.go"}, Verification: []byte(`[]`),
	})
	if err != nil || got.Status != store.MissionFailed || got.CostUSD == nil || *got.CostUSD != 0.5 || got.FinishedAt == nil || len(got.ChangedFiles) != 1 {
		t.Fatalf("transition result: %+v %v", got, err)
	}
	if g, _ := ms.GetMission(ctxB, m.ID); g != nil {
		t.Fatal("cross-tenant read")
	}
	if err := ms.AppendMissionEvent(ctxB, store.MissionEvent{MissionID: m.ID, Kind: "note", Actor: "x"}); err == nil {
		t.Fatal("cross-tenant event append")
	}
	ev, err := ms.ListMissionEvents(ctxA, m.ID)
	if err != nil || len(ev) != 3 {
		t.Fatalf("events: %d %v", len(ev), err)
	}
	active, _ := ms.ListActiveMissionsAllTenants(ctxA)
	if len(active) != 0 {
		t.Fatalf("no active missions expected, got %d", len(active))
	}
}
