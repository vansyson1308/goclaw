package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

type fakeMissionSvc struct{ created int }

func (f *fakeMissionSvc) Create(ctx context.Context, raw []byte, owner string) (*store.Mission, error) {
	f.created++
	return &store.Mission{ID: uuid.New(), TenantID: store.TenantIDFromContext(ctx), Status: store.MissionPlanned,
		WorkspacePath: "/var/lib/goclaw/missions/x/workspace", LeaseOwner: "host-1"}, nil
}

func (f *fakeMissionSvc) Cancel(context.Context, uuid.UUID, string, string) (*store.Mission, error) {
	return nil, store.ErrMissionNotFound
}

type fakeMissionStore struct {
	store.MissionStore
	m *store.Mission
}

func (f fakeMissionStore) GetMission(context.Context, uuid.UUID) (*store.Mission, error) { return f.m, nil }

// Mission verifiers run on the gateway host, so a tenant-scoped operator
// must not be able to create one; the master scope can.
func TestMissionCreateRequiresMasterScope(t *testing.T) {
	svc := &fakeMissionSvc{}
	h := NewMissionsHandler(nil, svc)
	body := `{"version":1}`

	tenantCtx := store.WithTenantID(context.Background(), uuid.New())
	rec := httptest.NewRecorder()
	h.handleCreate(rec, httptest.NewRequest(http.MethodPost, "/v1/missions", strings.NewReader(body)).WithContext(tenantCtx))
	if rec.Code != http.StatusForbidden || svc.created != 0 {
		t.Fatalf("tenant-scoped create: code %d, created %d", rec.Code, svc.created)
	}

	masterCtx := store.WithTenantID(context.Background(), store.MasterTenantID)
	rec = httptest.NewRecorder()
	h.handleCreate(rec, httptest.NewRequest(http.MethodPost, "/v1/missions", strings.NewReader(body)).WithContext(masterCtx))
	if rec.Code != http.StatusAccepted || svc.created != 1 {
		t.Fatalf("master create: code %d, created %d", rec.Code, svc.created)
	}
	var got store.Mission
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.WorkspacePath == "" {
		t.Error("master scope should see the workspace path")
	}
}

// Host paths are not shown to tenant-scoped viewers.
func TestMissionGetRedactsHostPathsForTenants(t *testing.T) {
	m := &store.Mission{ID: uuid.New(), Status: store.MissionRunning, WorkspacePath: "/srv/missions/t/m/workspace",
		LeaseOwner: "host-1", CreatedAt: time.Now()}
	h := NewMissionsHandler(fakeMissionStore{m: m}, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/missions/"+m.ID.String(), nil)
	req.SetPathValue("id", m.ID.String())
	rec := httptest.NewRecorder()
	h.handleGet(rec, req.WithContext(store.WithTenantID(context.Background(), uuid.New())))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "/srv/missions") || strings.Contains(rec.Body.String(), "host-1") {
		t.Fatalf("tenant view leaks host details: %d %s", rec.Code, rec.Body.String())
	}
	if m.WorkspacePath == "" {
		t.Fatal("redaction must not mutate the stored record")
	}
}
