package http

import (
	"context"
	"encoding/json"
	"errors"
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

func (f fakeMissionStore) GetMission(context.Context, uuid.UUID) (*store.Mission, error) {
	return f.m, nil
}

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

type fakeMissionEventStore struct {
	fakeMissionStore
	events []store.MissionEvent
	err    error
}

func (f fakeMissionEventStore) ListMissionEvents(context.Context, uuid.UUID) ([]store.MissionEvent, error) {
	return f.events, f.err
}

// Error texts in the status reason, check details and events can carry the
// gateway's mission directory; tenant-scoped viewers see it redacted, the
// master scope sees it as is.
func TestMissionHostPathsRedactedInReasonsDetailsAndEvents(t *testing.T) {
	dir := "/srv/goclaw/data/missions/t1/m1"
	m := &store.Mission{ID: uuid.New(), Status: store.MissionBlocked, WorkspacePath: dir + "/attempt-1/evidence-abc",
		StatusReason: "prepare workspace: mkdir " + dir + "/attempt-2/workspace: no space left on device",
		Verification: json.RawMessage(`[{"id":"a","detail":"copy workspace for check: open ` + dir + `/attempt-1/scratch/x: denied"}]`)}
	ev := store.MissionEvent{ID: uuid.New(), MissionID: m.ID, Message: "attempt 1: failed at " + dir + "/attempt-1/workspace",
		Detail: json.RawMessage(`{"path":"` + dir + `/attempt-1/workspace/f"}`)}
	h := NewMissionsHandler(fakeMissionEventStore{fakeMissionStore: fakeMissionStore{m: m}, events: []store.MissionEvent{ev}}, nil)

	get := func(ctx context.Context, path string, fn http.HandlerFunc) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetPathValue("id", m.ID.String())
		rec := httptest.NewRecorder()
		fn(rec, req.WithContext(ctx))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	tenant := store.WithTenantID(context.Background(), uuid.New())
	for _, body := range []string{get(tenant, "/v1/missions/x", h.handleGet), get(tenant, "/v1/missions/x/events", h.handleEvents)} {
		if strings.Contains(body, "/srv/goclaw") {
			t.Fatalf("tenant view leaks the mission directory: %s", body)
		}
	}
	master := store.WithTenantID(context.Background(), store.MasterTenantID)
	if body := get(master, "/v1/missions/x/events", h.handleEvents); !strings.Contains(body, dir) {
		t.Fatalf("master scope should see paths: %s", body)
	}
}

// Store errors (SQL text, driver details) are logged, not returned.
func TestMissionInternalErrorsAreNotReturnedVerbatim(t *testing.T) {
	m := &store.Mission{ID: uuid.New()}
	h := NewMissionsHandler(fakeMissionEventStore{fakeMissionStore: fakeMissionStore{m: m},
		err: errors.New(`pq: relation "mission_events" does not exist`)}, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/missions/x/events", nil)
	req.SetPathValue("id", m.ID.String())
	rec := httptest.NewRecorder()
	h.handleEvents(rec, req.WithContext(store.WithTenantID(context.Background(), store.MasterTenantID)))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "pq:") {
		t.Fatalf("internal error exposed: %d %s", rec.Code, rec.Body.String())
	}
}
