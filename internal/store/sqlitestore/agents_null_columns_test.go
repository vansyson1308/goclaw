//go:build sqlite || sqliteonly

package sqlitestore

import "testing"

// Agents with NULL display_name/status (allowed by the schema) must still be
// listed; List used to drop them silently because the scan failed.
func TestSQLiteAgentStoreList_NullDisplayNameAndStatus(t *testing.T) {
	db := newHookTestDB(t)
	tenantID, agentID := seedHookTenantAgent(t, db) // display_name stays NULL
	if _, err := db.Exec(`UPDATE agents SET status = NULL WHERE id = ?`, agentID.String()); err != nil {
		t.Fatalf("null status: %v", err)
	}
	list, err := NewSQLiteAgentStore(db).List(sqliteTenantCtx(tenantID), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != agentID {
		t.Fatalf("want the seeded agent, got %d agents", len(list))
	}
	if list[0].DisplayName != "" || list[0].Status != "active" {
		t.Fatalf("want '' / 'active', got %q / %q", list[0].DisplayName, list[0].Status)
	}
}
