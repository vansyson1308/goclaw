package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// PGMissionStore implements store.MissionStore.
type PGMissionStore struct{ db *sql.DB }

func NewPGMissionStore(db *sql.DB) *PGMissionStore { return &PGMissionStore{db: db} }

const missionCols = `id, tenant_id, owner_id, agent_key, title, contract, contract_digest, status,
	status_reason, executor, workspace_path, base_revision, verification, diff, diff_truncated,
	changed_files, summary, input_tokens, output_tokens, cost_usd, iterations, state_version,
	created_at, updated_at, started_at, finished_at`

func scanMission(row interface{ Scan(...any) error }) (*store.Mission, error) {
	var m store.Mission
	var reason, executor, wsPath, baseRev, diff, summary sql.NullString
	var verification, changed []byte
	var cost sql.NullFloat64
	if err := row.Scan(&m.ID, &m.TenantID, &m.OwnerID, &m.AgentKey, &m.Title, &m.Contract, &m.ContractDigest, &m.Status,
		&reason, &executor, &wsPath, &baseRev, &verification, &diff, &m.DiffTruncated,
		&changed, &summary, &m.InputTokens, &m.OutputTokens, &cost, &m.Iterations, &m.StateVersion,
		&m.CreatedAt, &m.UpdatedAt, &m.StartedAt, &m.FinishedAt); err != nil {
		return nil, err
	}
	m.StatusReason, m.Executor, m.WorkspacePath, m.BaseRevision = reason.String, executor.String, wsPath.String, baseRev.String
	m.Diff, m.Summary = diff.String, summary.String
	if len(verification) > 0 {
		m.Verification = verification
	}
	if len(changed) > 0 {
		_ = json.Unmarshal(changed, &m.ChangedFiles)
	}
	if cost.Valid {
		v := cost.Float64
		m.CostUSD = &v
	}
	return &m, nil
}

func (s *PGMissionStore) CreateMission(ctx context.Context, m *store.Mission, actor string) error {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return fmt.Errorf("missions: tenant_id required in context")
	}
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	m.TenantID = tenantID
	if m.Status == "" {
		m.Status = store.MissionPlanned
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO missions (id, tenant_id, owner_id, agent_key, title, contract, contract_digest, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at, updated_at`,
		m.ID, tenantID, m.OwnerID, m.AgentKey, m.Title, []byte(m.Contract), m.ContractDigest, m.Status,
	).Scan(&m.CreatedAt, &m.UpdatedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO mission_events (tenant_id, mission_id, kind, to_status, actor, message) VALUES ($1, $2, 'transition', $3, $4, 'created')`,
		tenantID, m.ID, m.Status, actor); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PGMissionStore) GetMission(ctx context.Context, id uuid.UUID) (*store.Mission, error) {
	m, err := scanMission(s.db.QueryRowContext(ctx,
		`SELECT `+missionCols+` FROM missions WHERE id = $1 AND tenant_id = $2`, id, store.TenantIDFromContext(ctx)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

func (s *PGMissionStore) ListMissions(ctx context.Context, limit int) ([]store.Mission, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+missionCols+` FROM missions WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT $2`,
		store.TenantIDFromContext(ctx), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Mission
	for rows.Next() {
		m, err := scanMission(rows)
		if err != nil {
			return nil, err
		}
		m.Diff = "" // list view: omit large payloads
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *PGMissionStore) ListActiveMissionsAllTenants(ctx context.Context) ([]store.Mission, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+missionCols+` FROM missions WHERE status IN ('planned','preparing','running','verifying') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Mission
	for rows.Next() {
		m, err := scanMission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *PGMissionStore) TransitionMission(ctx context.Context, id uuid.UUID, from []string, to, actor, message string, u store.MissionUpdate) (*store.Mission, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("missions: tenant_id required in context")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	var current string
	err = tx.QueryRowContext(ctx, `SELECT status FROM missions WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, id, tenantID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrMissionNotFound
	}
	if err != nil {
		return nil, err
	}
	if !slices.Contains(from, current) {
		return nil, fmt.Errorf("%w: status is %q, expected one of %v", store.ErrMissionStateConflict, current, from)
	}
	sets := []string{"status = $1", "updated_at = NOW()", "state_version = state_version + 1"}
	args := []any{to}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if u.StatusReason != nil {
		add("status_reason", *u.StatusReason)
	}
	if u.Executor != nil {
		add("executor", *u.Executor)
	}
	if u.WorkspacePath != nil {
		add("workspace_path", *u.WorkspacePath)
	}
	if u.BaseRevision != nil {
		add("base_revision", *u.BaseRevision)
	}
	if u.Verification != nil {
		add("verification", []byte(u.Verification))
	}
	if u.Diff != nil {
		add("diff", *u.Diff)
	}
	if u.DiffTruncated != nil {
		add("diff_truncated", *u.DiffTruncated)
	}
	if u.ChangedFiles != nil {
		b, _ := json.Marshal(u.ChangedFiles)
		add("changed_files", b)
	}
	if u.Summary != nil {
		add("summary", *u.Summary)
	}
	if u.InputTokens != nil {
		add("input_tokens", *u.InputTokens)
	}
	if u.OutputTokens != nil {
		add("output_tokens", *u.OutputTokens)
	}
	if u.CostUSD != nil {
		add("cost_usd", *u.CostUSD)
	}
	if u.Iterations != nil {
		add("iterations", *u.Iterations)
	}
	if u.StartedAt != nil {
		add("started_at", *u.StartedAt)
	}
	if u.FinishedAt != nil {
		add("finished_at", *u.FinishedAt)
	}
	args = append(args, id, tenantID)
	q := fmt.Sprintf(`UPDATE missions SET %s WHERE id = $%d AND tenant_id = $%d`, strings.Join(sets, ", "), len(args)-1, len(args))
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return nil, fmt.Errorf("update mission: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO mission_events (tenant_id, mission_id, kind, from_status, to_status, actor, message)
		 VALUES ($1, $2, 'transition', $3, $4, $5, $6)`, tenantID, id, current, to, actor, nilStr(message)); err != nil {
		return nil, err
	}
	m, err := scanMission(tx.QueryRowContext(ctx, `SELECT `+missionCols+` FROM missions WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *PGMissionStore) AppendMissionEvent(ctx context.Context, ev store.MissionEvent) error {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return fmt.Errorf("missions: tenant_id required in context")
	}
	var detail []byte
	if len(ev.Detail) > 0 {
		detail = ev.Detail
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO mission_events (tenant_id, mission_id, kind, actor, message, detail)
		 SELECT $1, id, $3, $4, $5, $6 FROM missions WHERE id = $2 AND tenant_id = $1`,
		tenantID, ev.MissionID, ev.Kind, ev.Actor, nilStr(ev.Message), detail)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrMissionNotFound
	}
	return nil
}

func (s *PGMissionStore) ListMissionEvents(ctx context.Context, missionID uuid.UUID) ([]store.MissionEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, mission_id, kind, COALESCE(from_status,''), COALESCE(to_status,''), actor, COALESCE(message,''), detail, created_at
		 FROM mission_events WHERE mission_id = $1 AND tenant_id = $2 ORDER BY created_at, id`,
		missionID, store.TenantIDFromContext(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.MissionEvent
	for rows.Next() {
		var e store.MissionEvent
		var detail []byte
		if err := rows.Scan(&e.ID, &e.MissionID, &e.Kind, &e.FromStatus, &e.ToStatus, &e.Actor, &e.Message, &detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			e.Detail = detail
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
