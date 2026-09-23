//go:build integration

package integration

import (
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

// Rows written by old installs (or by hand) may carry NULL display_name,
// api_base and api_key; the schema has always allowed it. One such row must
// not break listing — before the fix the whole scan failed and the gateway
// started with zero providers.
func TestLegacyProviderNullColumnsDoNotBreakListing(t *testing.T) {
	db := testDB(t)
	tenantID, _ := seedTenantAgent(t, db)
	pstore := pg.NewPGProviderStore(db, "")
	ctx := tenantCtx(tenantID)

	name := "legacy-null-" + uuid.NewString()[:8]
	id := uuid.New()
	if _, err := db.Exec(`INSERT INTO llm_providers (id, name, provider_type, tenant_id) VALUES ($1, $2, 'openai_compat', $3)`,
		id, name, tenantID); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM llm_providers WHERE id = $1`, id) })

	list, err := pstore.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders with a NULL-column row: %v", err)
	}
	found := false
	for _, p := range list {
		if p.ID == id {
			found = true
			if p.DisplayName != "" || p.APIBase != "" || p.APIKey != "" {
				t.Errorf("NULL columns should read as empty strings, got %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("legacy provider %s missing from list", name)
	}
	if _, err := pstore.ListAllProviders(ctx); err != nil {
		t.Fatalf("ListAllProviders: %v", err)
	}
	if _, err := pstore.GetProviderByName(ctx, name); err != nil {
		t.Fatalf("GetProviderByName: %v", err)
	}
	if _, err := pstore.GetProvider(ctx, id); err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
}

// Agents with NULL display_name/status (legacy or hand-inserted rows; the
// schema allows both) used to be skipped silently by list scans, so upgraded
// installs showed an empty agent list.
func TestLegacyAgentNullColumnsStillListed(t *testing.T) {
	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db) // seeds display_name = NULL
	if _, err := db.Exec(`UPDATE agents SET status = NULL WHERE id = $1`, agentID); err != nil {
		t.Fatalf("null status: %v", err)
	}
	agents := pg.NewPGAgentStore(db)
	ctx := tenantCtx(tenantID)

	list, err := agents.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, a := range list {
		if a.ID == agentID {
			found = true
			if a.DisplayName != "" || a.Status != "active" {
				t.Errorf("want display_name '' and status 'active' (column default), got %q/%q", a.DisplayName, a.Status)
			}
		}
	}
	if !found {
		t.Fatalf("agent with NULL display_name/status missing from List (got %d agents)", len(list))
	}
	if _, err := agents.GetByID(ctx, agentID); err != nil {
		t.Fatalf("GetByID: %v", err)
	}
}
