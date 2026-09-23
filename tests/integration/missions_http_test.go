//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	httpapi "github.com/nextlevelbuilder/goclaw/internal/http"
	"github.com/nextlevelbuilder/goclaw/internal/mission"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

type noopRunner struct{}

func (noopRunner) RunMission(context.Context, mission.RunInput) (*mission.RunOutput, error) {
	return nil, context.Canceled
}

// Tenant isolation of every missions endpoint against real PostgreSQL:
// another tenant sees nothing and cannot cancel.
func TestMissionsHTTPTenantIsolation(t *testing.T) {
	db := testDB(t)
	tenantA, _ := seedTenantAgent(t, db)
	tenantB, _ := seedTenantAgent(t, db)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM missions WHERE tenant_id IN ($1, $2)`, tenantA, tenantB) })
	ms := pg.NewPGMissionStore(db)
	ctxA, ctxB := tenantCtx(tenantA), tenantCtx(tenantB)

	m := &store.Mission{OwnerID: "alice", AgentKey: "coder", Title: "secret work", Contract: []byte(`{"version":1}`), ContractDigest: "d", MaxAttempts: 1}
	if err := ms.CreateMission(ctxA, m, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPlanned}, store.MissionRunning, "sys", "",
		store.MissionUpdate{Claim: &store.MissionClaim{Owner: "w", TTL: time.Minute}}); err != nil {
		t.Fatal(err)
	}
	if err := ms.BeginMissionReceipt(ctxA, store.MissionReceipt{MissionID: m.ID, Attempt: 1, Seq: 1, Tool: "exec", ActionClass: "exec", Status: store.ReceiptStarted, ArgsDigest: "x"},
		store.MissionFence{Owner: "w", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	svc, err := mission.NewService(mission.Config{SourceRoot: filepath.Join(root, "src"), DataRoot: filepath.Join(root, "data")}, ms, noopRunner{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.NewMissionsHandler(ms, svc)

	call := func(ctx context.Context, fn http.HandlerFunc, method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.SetPathValue("id", m.ID.String())
		rec := httptest.NewRecorder()
		fn(rec, req.WithContext(store.WithUserID(ctx, "someone")))
		return rec
	}
	base := "/v1/missions/" + m.ID.String()
	for name, fn := range map[string]http.HandlerFunc{"get": h.HandleGetForTest, "events": h.HandleEventsForTest, "receipts": h.HandleReceiptsForTest} {
		if rec := call(ctxB, fn, http.MethodGet, base); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "secret work") {
			t.Errorf("tenant B %s: %d %s", name, rec.Code, rec.Body.String())
		}
		if rec := call(ctxA, fn, http.MethodGet, base); rec.Code != http.StatusOK {
			t.Errorf("tenant A %s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	var listB struct {
		Missions []store.Mission `json:"missions"`
	}
	rec := call(ctxB, h.HandleListForTest, http.MethodGet, "/v1/missions")
	_ = json.Unmarshal(rec.Body.Bytes(), &listB)
	for _, x := range listB.Missions {
		if x.ID == m.ID {
			t.Fatal("tenant B lists tenant A's mission")
		}
	}
	if rec := call(ctxB, h.HandleCancelForTest, http.MethodPost, base+"/cancel"); rec.Code != http.StatusNotFound {
		t.Fatalf("tenant B cancel: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := ms.GetMission(ctxA, m.ID); got.Status != store.MissionRunning {
		t.Fatalf("tenant B changed the mission: %s", got.Status)
	}
	if rec := call(ctxA, h.HandleCancelForTest, http.MethodPost, base+"/cancel"); rec.Code != http.StatusOK {
		t.Fatalf("tenant A cancel: %d %s", rec.Code, rec.Body.String())
	}
	_ = uuid.Nil
}
