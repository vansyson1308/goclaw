package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/crypto"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	authzOperatorKey = "authz-operator-key"
	authzAdminKey    = "authz-admin-key"
)

func setupAuthzKeys(t *testing.T) {
	t.Helper()
	setupTestToken(t, "")
	setupTestCache(t, map[string]*store.APIKeyData{
		crypto.HashAPIKey(authzOperatorKey): {ID: uuid.New(), Scopes: []string{"operator.write"}},
		crypto.HashAPIKey(authzAdminKey):    {ID: uuid.New(), Scopes: []string{"operator.admin"}},
	})
}

func serve(mux *http.ServeMux, method, path, key, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

// Creating a mission runs code on the gateway's executor: admin only (and
// master scope, checked in the handler). Cancelling stays open to operators.
func TestMissionCreateRequiresAdminCancelDoesNot(t *testing.T) {
	setupAuthzKeys(t)
	svc := &fakeMissionSvc{}
	mux := http.NewServeMux()
	NewMissionsHandler(nil, svc).RegisterRoutes(mux)

	if rec := serve(mux, http.MethodPost, "/v1/missions", authzOperatorKey, `{"version":1}`, nil); rec.Code != http.StatusForbidden || svc.created != 0 {
		t.Fatalf("operator create: %d, created %d", rec.Code, svc.created)
	}
	if rec := serve(mux, http.MethodPost, "/v1/missions", authzAdminKey, `{"version":1}`, nil); rec.Code != http.StatusAccepted || svc.created != 1 {
		t.Fatalf("admin create: %d %s", rec.Code, rec.Body.String())
	}
	// The fake service reports "not found": the operator got past the role gate.
	if rec := serve(mux, http.MethodPost, "/v1/missions/"+uuid.NewString()+"/cancel", authzOperatorKey, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("operator cancel should pass the role gate: %d %s", rec.Code, rec.Body.String())
	}
}

// Applying or rolling back an evolution suggestion changes the agent's
// configuration or creates tenant skills, like the direct agent/skill writes:
// admin role, and a tenant owner/admin when tenant-scoped.
func TestEvolutionSuggestionPatchRequiresTenantAdmin(t *testing.T) {
	setupAuthzKeys(t)
	ts := newMockTenantStore()
	ts.addTenant(uuid.New(), "acme")
	setupTestTenantStore(t, ts)
	mux := http.NewServeMux()
	NewEvolutionHandler(nil, nil, WithTenantStore(ts)).RegisterRoutes(mux)
	// An invalid agent ID is rejected with 400 by the handler itself, after
	// the permission gate: 400 means allowed, 403 means refused.
	path := "/v1/agents/not-a-uuid/evolution/suggestions/" + uuid.NewString()

	if rec := serve(mux, http.MethodPatch, path, authzOperatorKey, `{}`, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("operator patch: want 403, got %d", rec.Code)
	}
	if rec := serve(mux, http.MethodPatch, path, authzAdminKey, `{}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("master-scope admin patch: want to pass the gate (400), got %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(mux, http.MethodPatch, path, authzAdminKey, `{}`, map[string]string{"X-GoClaw-Tenant-Id": "acme"}); rec.Code != http.StatusForbidden {
		t.Fatalf("tenant-scoped admin without tenant admin membership: want 403, got %d", rec.Code)
	}
	// Reads stay available to viewers and operators.
	if rec := serve(mux, http.MethodGet, "/v1/agents/not-a-uuid/evolution/suggestions", authzOperatorKey, "", nil); rec.Code == http.StatusForbidden {
		t.Fatalf("operator read must not be forbidden")
	}
}
