//go:build sqlite || sqliteonly

package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// SQLiteEvolutionSuggestionStore implements store.EvolutionSuggestionStore backed by SQLite.
type SQLiteEvolutionSuggestionStore struct {
	db *sql.DB
}

// NewSQLiteEvolutionSuggestionStore creates a new SQLite-backed evolution suggestion store.
func NewSQLiteEvolutionSuggestionStore(db *sql.DB) *SQLiteEvolutionSuggestionStore {
	return &SQLiteEvolutionSuggestionStore{db: db}
}

func (s *SQLiteEvolutionSuggestionStore) CreateSuggestion(ctx context.Context, sg store.EvolutionSuggestion) error {
	tenantID := tenantIDForInsert(ctx)
	if tenantID == uuid.Nil {
		return fmt.Errorf("evolution.CreateSuggestion: tenant_id required in context")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_evolution_suggestions
		 (id, tenant_id, agent_id, suggestion_type, suggestion, rationale, parameters, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sg.ID.String(), tenantID.String(), sg.AgentID.String(),
		string(sg.SuggestionType), sg.Suggestion, sg.Rationale,
		string(sg.Parameters), sg.Status)
	return err
}

const evoSuggestionCols = `id, tenant_id, agent_id, suggestion_type, suggestion, rationale,
	parameters, status, reviewed_by, reviewed_at, created_at,
	applied_at, applied_by, rolled_back_at, rolled_back_by, applied_change, state_version`

type evoRowScanner interface{ Scan(dest ...any) error }

func scanEvoSuggestion(row evoRowScanner) (*store.EvolutionSuggestion, error) {
	var sg store.EvolutionSuggestion
	var idStr, tenantStr, agentStr string
	var paramsBytes, changeBytes []byte
	var reviewedBy, appliedBy, rolledBackBy sql.NullString
	var reviewedAt, appliedAt, rolledBackAt nullSqliteTime
	var createdAt sqliteTime
	if err := row.Scan(&idStr, &tenantStr, &agentStr, &sg.SuggestionType,
		&sg.Suggestion, &sg.Rationale, &paramsBytes, &sg.Status,
		&reviewedBy, &reviewedAt, &createdAt,
		&appliedAt, &appliedBy, &rolledBackAt, &rolledBackBy, &changeBytes, &sg.StateVersion); err != nil {
		return nil, err
	}
	sg.ID, _ = uuid.Parse(idStr)
	sg.TenantID, _ = uuid.Parse(tenantStr)
	sg.AgentID, _ = uuid.Parse(agentStr)
	sg.Parameters = paramsBytes
	sg.ReviewedBy, sg.AppliedBy, sg.RolledBackBy = reviewedBy.String, appliedBy.String, rolledBackBy.String
	sg.ReviewedAt, sg.AppliedAt, sg.RolledBackAt = reviewedAt.ptr(), appliedAt.ptr(), rolledBackAt.ptr()
	sg.CreatedAt = createdAt.Time
	if len(changeBytes) > 0 {
		var c store.AgentConfigChange
		if err := json.Unmarshal(changeBytes, &c); err != nil {
			return nil, fmt.Errorf("decode applied_change: %w", err)
		}
		sg.AppliedChange = &c
	}
	return &sg, nil
}

func (s *SQLiteEvolutionSuggestionStore) ListSuggestions(ctx context.Context, agentID uuid.UUID, status string, limit int) ([]store.EvolutionSuggestion, error) {
	tenantID := tenantIDForInsert(ctx)
	if limit <= 0 {
		limit = 50
	}

	query := `SELECT ` + evoSuggestionCols + `
	          FROM agent_evolution_suggestions
	          WHERE agent_id = ? AND tenant_id = ?`
	args := []any{agentID.String(), tenantID.String()}
	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var suggestions []store.EvolutionSuggestion
	for rows.Next() {
		sg, err := scanEvoSuggestion(rows)
		if err != nil {
			return nil, err
		}
		suggestions = append(suggestions, *sg)
	}
	return suggestions, rows.Err()
}

func (s *SQLiteEvolutionSuggestionStore) UpdateSuggestionStatus(ctx context.Context, id uuid.UUID, status, reviewedBy string) error {
	tenantID := tenantIDForInsert(ctx)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_evolution_suggestions
		 SET status = ?, reviewed_by = ?, reviewed_at = ?
		 WHERE id = ? AND tenant_id = ?`,
		status, reviewedBy, now, id.String(), tenantID.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("suggestion not found or access denied")
	}
	return nil
}

func (s *SQLiteEvolutionSuggestionStore) UpdateSuggestionParameters(ctx context.Context, id uuid.UUID, params json.RawMessage) error {
	tenantID := tenantIDForInsert(ctx)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_evolution_suggestions SET parameters = ?
		 WHERE id = ? AND tenant_id = ?`,
		string(params), id.String(), tenantID.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("suggestion not found or access denied")
	}
	return nil
}

func (s *SQLiteEvolutionSuggestionStore) GetSuggestion(ctx context.Context, id uuid.UUID) (*store.EvolutionSuggestion, error) {
	tenantID := tenantIDForInsert(ctx)
	sg, err := scanEvoSuggestion(s.db.QueryRowContext(ctx,
		`SELECT `+evoSuggestionCols+` FROM agent_evolution_suggestions WHERE id = ? AND tenant_id = ?`,
		id.String(), tenantID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sg, nil
}

// TransitionSuggestion mirrors the PG implementation. SQLite has no row locks,
// so the transaction takes the database write lock up front with a no-op
// UPDATE; that serializes concurrent transitions and avoids deferred-lock
// upgrade failures.
func (s *SQLiteEvolutionSuggestionStore) TransitionSuggestion(ctx context.Context, t store.SuggestionTransition) (*store.EvolutionSuggestion, error) {
	tenantID := tenantIDForInsert(ctx)
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("evolution.TransitionSuggestion: tenant_id required in context")
	}
	if t.Actor == "" {
		return nil, fmt.Errorf("evolution.TransitionSuggestion: actor required")
	}
	if t.Mutate != nil && !store.EvolutionConfigColumns[t.Column] {
		return nil, fmt.Errorf("evolution.TransitionSuggestion: column %q not allowed", t.Column)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_evolution_suggestions SET state_version = state_version WHERE id = ? AND tenant_id = ?`,
		t.ID.String(), tenantID.String()); err != nil {
		return nil, err
	}
	sg, err := scanEvoSuggestion(tx.QueryRowContext(ctx,
		`SELECT `+evoSuggestionCols+` FROM agent_evolution_suggestions WHERE id = ? AND tenant_id = ?`,
		t.ID.String(), tenantID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("suggestion not found or access denied")
	}
	if err != nil {
		return nil, err
	}
	fromStatus := sg.Status
	if err := store.CheckTransitionFrom(sg.Status, t.From); err != nil {
		return nil, err
	}

	var change *store.AgentConfigChange
	if t.Mutate != nil {
		var current []byte
		// Column is allowlisted above; safe to interpolate.
		err := tx.QueryRowContext(ctx,
			`SELECT `+t.Column+` FROM agents WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL`,
			sg.AgentID.String(), tenantID.String()).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("agent not found")
		}
		if err != nil {
			return nil, err
		}
		next, c, err := t.Mutate(sg, current)
		if err != nil {
			return nil, err
		}
		change = c
		if _, err := tx.ExecContext(ctx,
			`UPDATE agents SET `+t.Column+` = ?, updated_at = ? WHERE id = ? AND tenant_id = ?`,
			string(next), time.Now().UTC().Format(time.RFC3339Nano), sg.AgentID.String(), tenantID.String()); err != nil {
			return nil, fmt.Errorf("update agent %s: %w", t.Column, err)
		}
	}

	now := time.Now().UTC()
	store.ApplyTransitionFields(sg, t, change, now)
	var changeJSON any
	if sg.AppliedChange != nil {
		b, _ := json.Marshal(sg.AppliedChange)
		changeJSON = string(b)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_evolution_suggestions
		 SET status = ?, reviewed_by = ?, reviewed_at = ?, applied_at = ?, applied_by = ?,
		     rolled_back_at = ?, rolled_back_by = ?, applied_change = ?, state_version = ?
		 WHERE id = ? AND tenant_id = ?`,
		sg.Status, nilStr(sg.ReviewedBy), timePtrStr(sg.ReviewedAt), timePtrStr(sg.AppliedAt), nilStr(sg.AppliedBy),
		timePtrStr(sg.RolledBackAt), nilStr(sg.RolledBackBy), changeJSON, sg.StateVersion,
		sg.ID.String(), tenantID.String()); err != nil {
		return nil, err
	}
	var detail any
	if d := store.TransitionEventDetail(t, change); d != nil {
		detail = string(d)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_evolution_events (id, tenant_id, suggestion_id, agent_id, action, from_status, to_status, actor, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.New().String(), tenantID.String(), sg.ID.String(), sg.AgentID.String(), t.Action,
		fromStatus, sg.Status, t.Actor, detail, now.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("record evolution event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sg, nil
}

func (s *SQLiteEvolutionSuggestionStore) ListSuggestionEvents(ctx context.Context, suggestionID uuid.UUID) ([]store.EvolutionEvent, error) {
	tenantID := tenantIDForInsert(ctx)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, suggestion_id, agent_id, action, from_status, to_status, actor, detail, created_at
		 FROM agent_evolution_events WHERE suggestion_id = ? AND tenant_id = ?
		 ORDER BY created_at, rowid`, suggestionID.String(), tenantID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.EvolutionEvent
	for rows.Next() {
		var e store.EvolutionEvent
		var id, sid, aid string
		var detail sql.NullString
		var createdAt sqliteTime
		if err := rows.Scan(&id, &sid, &aid, &e.Action, &e.FromStatus, &e.ToStatus, &e.Actor, &detail, &createdAt); err != nil {
			return nil, err
		}
		e.ID, _ = uuid.Parse(id)
		e.SuggestionID, _ = uuid.Parse(sid)
		e.AgentID, _ = uuid.Parse(aid)
		if detail.Valid {
			e.Detail = json.RawMessage(detail.String)
		}
		e.CreatedAt = createdAt.Time
		out = append(out, e)
	}
	return out, rows.Err()
}

func timePtrStr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// Ensure SQLiteEvolutionSuggestionStore implements store.EvolutionSuggestionStore.
var _ store.EvolutionSuggestionStore = (*SQLiteEvolutionSuggestionStore)(nil)
