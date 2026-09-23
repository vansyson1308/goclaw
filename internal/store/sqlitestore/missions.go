//go:build sqlite || sqliteonly

package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// SQLiteMissionStore implements store.MissionStore for the Lite edition.
type SQLiteMissionStore struct{ db *sql.DB }

func NewSQLiteMissionStore(db *sql.DB) *SQLiteMissionStore { return &SQLiteMissionStore{db: db} }

const missionCols = `id, tenant_id, owner_id, agent_key, title, contract, contract_digest, status,
	status_reason, executor, workspace_path, base_revision, verification, diff, diff_truncated,
	changed_files, summary, input_tokens, output_tokens, cost_usd, iterations, state_version,
	created_at, updated_at, started_at, finished_at`

func scanMission(row interface{ Scan(...any) error }) (*store.Mission, error) {
	var m store.Mission
	var id, tenant string
	var contract, verification, changed sql.NullString
	var reason, executor, wsPath, baseRev, diff, summary sql.NullString
	var cost sql.NullFloat64
	var created, updated sqliteTime
	var started, finished nullSqliteTime
	if err := row.Scan(&id, &tenant, &m.OwnerID, &m.AgentKey, &m.Title, &contract, &m.ContractDigest, &m.Status,
		&reason, &executor, &wsPath, &baseRev, &verification, &diff, &m.DiffTruncated,
		&changed, &summary, &m.InputTokens, &m.OutputTokens, &cost, &m.Iterations, &m.StateVersion,
		&created, &updated, &started, &finished); err != nil {
		return nil, err
	}
	m.ID, _ = uuid.Parse(id)
	m.TenantID, _ = uuid.Parse(tenant)
	m.Contract = json.RawMessage(contract.String)
	m.StatusReason, m.Executor, m.WorkspacePath, m.BaseRevision = reason.String, executor.String, wsPath.String, baseRev.String
	m.Diff, m.Summary = diff.String, summary.String
	if verification.Valid && verification.String != "" {
		m.Verification = json.RawMessage(verification.String)
	}
	if changed.Valid && changed.String != "" {
		_ = json.Unmarshal([]byte(changed.String), &m.ChangedFiles)
	}
	if cost.Valid {
		v := cost.Float64
		m.CostUSD = &v
	}
	m.CreatedAt, m.UpdatedAt = created.Time, updated.Time
	m.StartedAt, m.FinishedAt = started.ptr(), finished.ptr()
	return &m, nil
}

func nowText() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *SQLiteMissionStore) CreateMission(ctx context.Context, m *store.Mission, actor string) error {
	tenantID := tenantIDForInsert(ctx)
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
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO missions (id, tenant_id, owner_id, agent_key, title, contract, contract_digest, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID.String(), tenantID.String(), m.OwnerID, m.AgentKey, m.Title, string(m.Contract), m.ContractDigest, m.Status,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO mission_events (id, tenant_id, mission_id, kind, to_status, actor, message, created_at) VALUES (?, ?, ?, 'transition', ?, ?, 'created', ?)`,
		uuid.New().String(), tenantID.String(), m.ID.String(), m.Status, actor, now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	m.CreatedAt, m.UpdatedAt = now, now
	return tx.Commit()
}

func (s *SQLiteMissionStore) GetMission(ctx context.Context, id uuid.UUID) (*store.Mission, error) {
	m, err := scanMission(s.db.QueryRowContext(ctx,
		`SELECT `+missionCols+` FROM missions WHERE id = ? AND tenant_id = ?`, id.String(), tenantIDForInsert(ctx).String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

func (s *SQLiteMissionStore) ListMissions(ctx context.Context, limit int) ([]store.Mission, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.list(ctx, `SELECT `+missionCols+` FROM missions WHERE tenant_id = ? ORDER BY created_at DESC LIMIT ?`,
		true, tenantIDForInsert(ctx).String(), limit)
}

func (s *SQLiteMissionStore) ListActiveMissionsAllTenants(ctx context.Context) ([]store.Mission, error) {
	return s.list(ctx, `SELECT `+missionCols+` FROM missions WHERE status IN ('planned','preparing','running','verifying') ORDER BY created_at`, false)
}

func (s *SQLiteMissionStore) list(ctx context.Context, q string, omitDiff bool, args ...any) ([]store.Mission, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
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
		if omitDiff {
			m.Diff = ""
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *SQLiteMissionStore) TransitionMission(ctx context.Context, id uuid.UUID, from []string, to, actor, message string, u store.MissionUpdate) (*store.Mission, error) {
	tenantID := tenantIDForInsert(ctx)
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("missions: tenant_id required in context")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	var current string
	err = tx.QueryRowContext(ctx, `SELECT status FROM missions WHERE id = ? AND tenant_id = ?`, id.String(), tenantID.String()).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrMissionNotFound
	}
	if err != nil {
		return nil, err
	}
	if !slices.Contains(from, current) {
		return nil, fmt.Errorf("%w: status is %q, expected one of %v", store.ErrMissionStateConflict, current, from)
	}
	sets := []string{"status = ?", "updated_at = ?", "state_version = state_version + 1"}
	args := []any{to, nowText()}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
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
		add("verification", string(u.Verification))
	}
	if u.Diff != nil {
		add("diff", *u.Diff)
	}
	if u.DiffTruncated != nil {
		add("diff_truncated", *u.DiffTruncated)
	}
	if u.ChangedFiles != nil {
		b, _ := json.Marshal(u.ChangedFiles)
		add("changed_files", string(b))
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
		add("started_at", u.StartedAt.UTC().Format(time.RFC3339Nano))
	}
	if u.FinishedAt != nil {
		add("finished_at", u.FinishedAt.UTC().Format(time.RFC3339Nano))
	}
	args = append(args, id.String(), tenantID.String())
	if _, err := tx.ExecContext(ctx, `UPDATE missions SET `+strings.Join(sets, ", ")+` WHERE id = ? AND tenant_id = ?`, args...); err != nil {
		return nil, fmt.Errorf("update mission: %w", err)
	}
	var msg any
	if message != "" {
		msg = message
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO mission_events (id, tenant_id, mission_id, kind, from_status, to_status, actor, message, created_at)
		 VALUES (?, ?, ?, 'transition', ?, ?, ?, ?, ?)`,
		uuid.New().String(), tenantID.String(), id.String(), current, to, actor, msg, nowText()); err != nil {
		return nil, err
	}
	m, err := scanMission(tx.QueryRowContext(ctx, `SELECT `+missionCols+` FROM missions WHERE id = ?`, id.String()))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *SQLiteMissionStore) AppendMissionEvent(ctx context.Context, ev store.MissionEvent) error {
	tenantID := tenantIDForInsert(ctx)
	if tenantID == uuid.Nil {
		return fmt.Errorf("missions: tenant_id required in context")
	}
	var detail, msg any
	if len(ev.Detail) > 0 {
		detail = string(ev.Detail)
	}
	if ev.Message != "" {
		msg = ev.Message
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO mission_events (id, tenant_id, mission_id, kind, actor, message, detail, created_at)
		 SELECT ?, tenant_id, id, ?, ?, ?, ?, ? FROM missions WHERE id = ? AND tenant_id = ?`,
		uuid.New().String(), ev.Kind, ev.Actor, msg, detail, nowText(), ev.MissionID.String(), tenantID.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrMissionNotFound
	}
	return nil
}

func (s *SQLiteMissionStore) ListMissionEvents(ctx context.Context, missionID uuid.UUID) ([]store.MissionEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, mission_id, kind, COALESCE(from_status,''), COALESCE(to_status,''), actor, COALESCE(message,''), detail, created_at
		 FROM mission_events WHERE mission_id = ? AND tenant_id = ? ORDER BY created_at, rowid`,
		missionID.String(), tenantIDForInsert(ctx).String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.MissionEvent
	for rows.Next() {
		var e store.MissionEvent
		var id, mid string
		var detail sql.NullString
		var created sqliteTime
		if err := rows.Scan(&id, &mid, &e.Kind, &e.FromStatus, &e.ToStatus, &e.Actor, &e.Message, &detail, &created); err != nil {
			return nil, err
		}
		e.ID, _ = uuid.Parse(id)
		e.MissionID, _ = uuid.Parse(mid)
		if detail.Valid && detail.String != "" {
			e.Detail = json.RawMessage(detail.String)
		}
		e.CreatedAt = created.Time
		out = append(out, e)
	}
	return out, rows.Err()
}

var _ store.MissionStore = (*SQLiteMissionStore)(nil)
