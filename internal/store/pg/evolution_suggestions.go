package pg

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

// PGEvolutionSuggestionStore implements store.EvolutionSuggestionStore backed by PostgreSQL.
type PGEvolutionSuggestionStore struct {
	db *sql.DB
}

// NewPGEvolutionSuggestionStore creates a new PG-backed evolution suggestion store.
func NewPGEvolutionSuggestionStore(db *sql.DB) *PGEvolutionSuggestionStore {
	return &PGEvolutionSuggestionStore{db: db}
}

const evoSuggestionCols = `id, tenant_id, agent_id, suggestion_type, suggestion, rationale,
	parameters, status, reviewed_by, reviewed_at, created_at,
	applied_at, applied_by, rolled_back_at, rolled_back_by, applied_change, state_version`

type evoRowScanner interface{ Scan(dest ...any) error }

func scanEvoSuggestion(row evoRowScanner) (*store.EvolutionSuggestion, error) {
	var sg store.EvolutionSuggestion
	var reviewedBy, appliedBy, rolledBackBy sql.NullString
	var change []byte
	if err := row.Scan(&sg.ID, &sg.TenantID, &sg.AgentID, &sg.SuggestionType,
		&sg.Suggestion, &sg.Rationale, &sg.Parameters, &sg.Status,
		&reviewedBy, &sg.ReviewedAt, &sg.CreatedAt,
		&sg.AppliedAt, &appliedBy, &sg.RolledBackAt, &rolledBackBy, &change, &sg.StateVersion); err != nil {
		return nil, err
	}
	sg.ReviewedBy, sg.AppliedBy, sg.RolledBackBy = reviewedBy.String, appliedBy.String, rolledBackBy.String
	if len(change) > 0 {
		var c store.AgentConfigChange
		if err := json.Unmarshal(change, &c); err != nil {
			return nil, fmt.Errorf("decode applied_change: %w", err)
		}
		sg.AppliedChange = &c
	}
	return &sg, nil
}

func (s *PGEvolutionSuggestionStore) CreateSuggestion(ctx context.Context, sg store.EvolutionSuggestion) error {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return fmt.Errorf("evolution.CreateSuggestion: tenant_id required in context")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_evolution_suggestions
		 (id, tenant_id, agent_id, suggestion_type, suggestion, rationale, parameters, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		sg.ID, tenantID, sg.AgentID, sg.SuggestionType, sg.Suggestion,
		sg.Rationale, sg.Parameters, sg.Status)
	return err
}

func (s *PGEvolutionSuggestionStore) ListSuggestions(ctx context.Context, agentID uuid.UUID, status string, limit int) ([]store.EvolutionSuggestion, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if limit <= 0 {
		limit = 50
	}

	query := `SELECT ` + evoSuggestionCols + `
	          FROM agent_evolution_suggestions
	          WHERE agent_id = $1 AND tenant_id = $2`
	args := []any{agentID, tenantID}
	if status != "" {
		query += " AND status = $3"
		args = append(args, status)
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args)+1)
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

func (s *PGEvolutionSuggestionStore) UpdateSuggestionStatus(ctx context.Context, id uuid.UUID, status, reviewedBy string) error {
	tenantID := store.TenantIDFromContext(ctx)
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_evolution_suggestions
		 SET status = $1, reviewed_by = $2, reviewed_at = $3, state_version = state_version + 1
		 WHERE id = $4 AND tenant_id = $5`,
		status, reviewedBy, now, id, tenantID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("suggestion not found or access denied")
	}
	return nil
}

func (s *PGEvolutionSuggestionStore) UpdateSuggestionParameters(ctx context.Context, id uuid.UUID, params json.RawMessage) error {
	tenantID := store.TenantIDFromContext(ctx)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_evolution_suggestions SET parameters = $1
		 WHERE id = $2 AND tenant_id = $3`,
		params, id, tenantID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("suggestion not found or access denied")
	}
	return nil
}

func (s *PGEvolutionSuggestionStore) GetSuggestion(ctx context.Context, id uuid.UUID) (*store.EvolutionSuggestion, error) {
	tenantID := store.TenantIDFromContext(ctx)
	sg, err := scanEvoSuggestion(s.db.QueryRowContext(ctx,
		`SELECT `+evoSuggestionCols+` FROM agent_evolution_suggestions WHERE id = $1 AND tenant_id = $2`,
		id, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sg, nil
}

// TransitionSuggestion locks the suggestion (and the agent row when a config
// mutation is requested), validates the current status, applies the mutation,
// and records the audit event — all in one transaction.
func (s *PGEvolutionSuggestionStore) TransitionSuggestion(ctx context.Context, t store.SuggestionTransition) (*store.EvolutionSuggestion, error) {
	tenantID := store.TenantIDFromContext(ctx)
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

	sg, err := scanEvoSuggestion(tx.QueryRowContext(ctx,
		`SELECT `+evoSuggestionCols+` FROM agent_evolution_suggestions
		 WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, t.ID, tenantID))
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
			`SELECT `+t.Column+` FROM agents WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL FOR NO KEY UPDATE`,
			sg.AgentID, tenantID).Scan(&current)
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
			`UPDATE agents SET `+t.Column+` = $1, updated_at = NOW() WHERE id = $2 AND tenant_id = $3`,
			[]byte(next), sg.AgentID, tenantID); err != nil {
			return nil, fmt.Errorf("update agent %s: %w", t.Column, err)
		}
	}

	now := time.Now().UTC()
	store.ApplyTransitionFields(sg, t, change, now)
	var changeJSON []byte
	if sg.AppliedChange != nil {
		changeJSON, _ = json.Marshal(sg.AppliedChange)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_evolution_suggestions
		 SET status = $1, reviewed_by = $2, reviewed_at = $3, applied_at = $4, applied_by = $5,
		     rolled_back_at = $6, rolled_back_by = $7, applied_change = $8, state_version = $9
		 WHERE id = $10 AND tenant_id = $11`,
		sg.Status, nilStr(sg.ReviewedBy), sg.ReviewedAt, sg.AppliedAt, nilStr(sg.AppliedBy),
		sg.RolledBackAt, nilStr(sg.RolledBackBy), changeJSON, sg.StateVersion, sg.ID, tenantID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_evolution_events (tenant_id, suggestion_id, agent_id, action, from_status, to_status, actor, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		tenantID, sg.ID, sg.AgentID, t.Action, fromStatus, sg.Status, t.Actor,
		[]byte(store.TransitionEventDetail(t, change))); err != nil {
		return nil, fmt.Errorf("record evolution event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sg, nil
}

func (s *PGEvolutionSuggestionStore) ListSuggestionEvents(ctx context.Context, suggestionID uuid.UUID) ([]store.EvolutionEvent, error) {
	tenantID := store.TenantIDFromContext(ctx)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, suggestion_id, agent_id, action, from_status, to_status, actor, detail, created_at
		 FROM agent_evolution_events WHERE suggestion_id = $1 AND tenant_id = $2
		 ORDER BY created_at, id`, suggestionID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.EvolutionEvent
	for rows.Next() {
		var e store.EvolutionEvent
		var detail []byte
		if err := rows.Scan(&e.ID, &e.SuggestionID, &e.AgentID, &e.Action, &e.FromStatus, &e.ToStatus, &e.Actor, &detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Detail = detail
		out = append(out, e)
	}
	return out, rows.Err()
}
