//go:build integration

package integration

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

func TestMissionStoreLifecycleCASAndIsolation(t *testing.T) {
	db := testDB(t)
	tenantA, _ := seedTenantAgent(t, db)
	tenantB, _ := seedTenantAgent(t, db)
	ms := pg.NewPGMissionStore(db)
	ctxA, ctxB := tenantCtx(tenantA), tenantCtx(tenantB)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM missions WHERE tenant_id IN ($1, $2)`, tenantA, tenantB) })

	m := &store.Mission{OwnerID: "alice", AgentKey: "coder", Title: "t", Contract: []byte(`{"version":1}`), ContractDigest: "d"}
	if err := ms.CreateMission(ctxA, m, "alice"); err != nil {
		t.Fatal(err)
	}
	if m.Status != store.MissionPlanned {
		t.Fatalf("status %s", m.Status)
	}
	// Concurrent planned→preparing: exactly one wins.
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPlanned}, store.MissionPreparing, "sys", "prep", store.MissionUpdate{})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if !errors.Is(err, store.ErrMissionStateConflict) {
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("wins = %d, want 1", wins)
	}
	cost := 0.0125
	now := time.Now().UTC()
	reason := "1/1 passed"
	diff := "diff --git a/x b/x"
	got, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPreparing}, store.MissionSucceeded, "sys", "done", store.MissionUpdate{
		StatusReason: &reason, Diff: &diff, ChangedFiles: []string{"x"}, CostUSD: &cost, FinishedAt: &now,
		Verification: []byte(`[{"id":"a","status":"pass"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.MissionSucceeded || got.CostUSD == nil || *got.CostUSD != cost || got.Diff != diff ||
		len(got.ChangedFiles) != 1 || got.FinishedAt == nil || got.StateVersion != 2 {
		t.Fatalf("stored state wrong: %+v", got)
	}
	// Unknown cost stays nil (not zero) on a fresh mission.
	m2 := &store.Mission{OwnerID: "alice", AgentKey: "coder", Title: "t2", Contract: []byte(`{}`), ContractDigest: "d"}
	if err := ms.CreateMission(ctxA, m2, "alice"); err != nil {
		t.Fatal(err)
	}
	if g, _ := ms.GetMission(ctxA, m2.ID); g.CostUSD != nil {
		t.Fatalf("unknown cost must be nil, got %v", *g.CostUSD)
	}
	// Tenant isolation.
	if g, _ := ms.GetMission(ctxB, m.ID); g != nil {
		t.Fatal("cross-tenant read")
	}
	if _, err := ms.TransitionMission(ctxB, m2.ID, store.MissionActiveStatuses, store.MissionCancelled, "x", "", store.MissionUpdate{}); err == nil {
		t.Fatal("cross-tenant transition")
	}
	if list, _ := ms.ListMissions(ctxB, 10); len(list) != 0 {
		t.Fatal("cross-tenant list")
	}
	if err := ms.AppendMissionEvent(ctxB, store.MissionEvent{MissionID: m.ID, Kind: "note", Actor: "x"}); err == nil {
		t.Fatal("cross-tenant event append")
	}
	if ev, _ := ms.ListMissionEvents(ctxB, m.ID); len(ev) != 0 {
		t.Fatal("cross-tenant events")
	}
	// Events: created + prepared + succeeded, ordered.
	ev, err := ms.ListMissionEvents(ctxA, m.ID)
	if err != nil || len(ev) != 3 || ev[0].ToStatus != "planned" || ev[1].ToStatus != "preparing" || ev[2].ToStatus != "succeeded" || ev[2].FromStatus != "preparing" {
		t.Fatalf("events: %+v %v", ev, err)
	}
	// Active listing across tenants sees m2 (planned) but not m (terminal).
	active, _ := ms.ListActiveMissionsAllTenants(ctxA)
	var sawM2, sawM bool
	for _, a := range active {
		sawM2 = sawM2 || a.ID == m2.ID
		sawM = sawM || a.ID == m.ID
	}
	if !sawM2 || sawM {
		t.Fatalf("active listing wrong: m2=%v m=%v", sawM2, sawM)
	}
	// List omits the diff payload.
	list, _ := ms.ListMissions(ctxA, 10)
	for _, l := range list {
		if l.Diff != "" {
			t.Fatal("list must omit diff")
		}
	}
}
