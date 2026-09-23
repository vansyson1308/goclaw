package pg

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

// PGMissionStore implements store.MissionStore.
type PGMissionStore struct{ db *sql.DB }

func NewPGMissionStore(db *sql.DB) *PGMissionStore { return &PGMissionStore{db: db} }

const missionCols = `id, tenant_id, owner_id, agent_key, title, contract, contract_digest, status,
	status_reason, executor, workspace_path, base_revision, verification, diff, diff_truncated,
	changed_files, summary, input_tokens, output_tokens, cost_usd, iterations, state_version,
	created_at, updated_at, started_at, finished_at,
	usage_incomplete, attempt, max_attempts, lease_owner, lease_expires_at, pins`

func scanMission(row interface{ Scan(...any) error }) (*store.Mission, error) {
	var m store.Mission
	var reason, executor, wsPath, baseRev, diff, summary sql.NullString
	var verification, changed []byte
	var cost sql.NullFloat64
	var leaseOwner sql.NullString
	var pins []byte
	if err := row.Scan(&m.ID, &m.TenantID, &m.OwnerID, &m.AgentKey, &m.Title, &m.Contract, &m.ContractDigest, &m.Status,
		&reason, &executor, &wsPath, &baseRev, &verification, &diff, &m.DiffTruncated,
		&changed, &summary, &m.InputTokens, &m.OutputTokens, &cost, &m.Iterations, &m.StateVersion,
		&m.CreatedAt, &m.UpdatedAt, &m.StartedAt, &m.FinishedAt,
		&m.UsageIncomplete, &m.Attempt, &m.MaxAttempts, &leaseOwner, &m.LeaseExpiresAt, &pins); err != nil {
		return nil, err
	}
	m.LeaseOwner = leaseOwner.String
	if len(pins) > 0 {
		m.Pins = pins
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
	if m.MaxAttempts <= 0 {
		m.MaxAttempts = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO missions (id, tenant_id, owner_id, agent_key, title, contract, contract_digest, status, max_attempts, pins)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING created_at, updated_at`,
		m.ID, tenantID, m.OwnerID, m.AgentKey, m.Title, []byte(m.Contract), m.ContractDigest, m.Status, m.MaxAttempts, nilJSON(m.Pins),
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
	var cur missionLockState
	err = tx.QueryRowContext(ctx,
		`SELECT status, attempt, max_attempts, COALESCE(lease_owner, '') FROM missions WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		id, tenantID).Scan(&cur.status, &cur.attempt, &cur.maxAttempts, &cur.leaseOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrMissionNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := cur.check(from, u); err != nil {
		return nil, err
	}
	current := cur.status
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
	if u.UsageIncomplete != nil {
		add("usage_incomplete", *u.UsageIncomplete)
	}
	switch {
	case u.Claim != nil:
		add("lease_owner", u.Claim.Owner)
		add("lease_expires_at", u.Claim.Until)
		sets = append(sets, "attempt = attempt + 1")
	case u.ClearLease:
		sets = append(sets, "lease_owner = NULL", "lease_expires_at = NULL")
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

// missionLockState is the row state read under lock by TransitionMission.
type missionLockState struct {
	status, leaseOwner   string
	attempt, maxAttempts int
}

// check enforces the status compare-and-set plus the optional fence/claim.
func (c missionLockState) check(from []string, u store.MissionUpdate) error {
	if !slices.Contains(from, c.status) {
		return fmt.Errorf("%w: status is %q, expected one of %v", store.ErrMissionStateConflict, c.status, from)
	}
	if f := u.Fence; f != nil && (c.leaseOwner != f.Owner || c.attempt != f.Attempt) {
		return fmt.Errorf("%w: %w (held by %q attempt %d)", store.ErrMissionStateConflict, store.ErrMissionLeaseLost, c.leaseOwner, c.attempt)
	}
	if u.Claim != nil && c.attempt >= c.maxAttempts {
		return fmt.Errorf("%w: %w (%d/%d)", store.ErrMissionStateConflict, store.ErrMissionNoAttempts, c.attempt, c.maxAttempts)
	}
	return nil
}

func (s *PGMissionStore) RenewMissionLease(ctx context.Context, id uuid.UUID, fence store.MissionFence, until time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE missions SET lease_expires_at = $1
		 WHERE id = $2 AND tenant_id = $3 AND lease_owner = $4 AND attempt = $5
		   AND status IN ('planned','preparing','running','verifying')`,
		until, id, store.TenantIDFromContext(ctx), fence.Owner, fence.Attempt)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrMissionLeaseLost
	}
	return nil
}

func (s *PGMissionStore) BeginMissionReceipt(ctx context.Context, r store.MissionReceipt, fence store.MissionFence) error {
	tenantID := store.TenantIDFromContext(ctx)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO mission_receipts (tenant_id, mission_id, attempt, seq, tool, action_class, status, reason, args_digest)
		 SELECT tenant_id, id, $3, $4, $5, $6, $7, $8, $9 FROM missions
		 WHERE id = $2 AND tenant_id = $1 AND lease_owner = $10 AND attempt = $3
		   AND status IN ('preparing','running','verifying')
		 ON CONFLICT (mission_id, attempt, seq) DO NOTHING`,
		tenantID, r.MissionID, r.Attempt, r.Seq, r.Tool, r.ActionClass, r.Status, nilStr(r.Reason), r.ArgsDigest, fence.Owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either a duplicate (idempotent) or the lease is gone.
		var exists bool
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM mission_receipts WHERE mission_id = $1 AND tenant_id = $2 AND attempt = $3 AND seq = $4)`,
			r.MissionID, tenantID, r.Attempt, r.Seq).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return store.ErrMissionLeaseLost
		}
	}
	return nil
}

func (s *PGMissionStore) CompleteMissionReceipt(ctx context.Context, missionID uuid.UUID, attempt, seq int, status string, durationMS int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE mission_receipts SET status = $1, duration_ms = $2, updated_at = clock_timestamp()
		 WHERE mission_id = $3 AND tenant_id = $4 AND attempt = $5 AND seq = $6 AND status = 'started'`,
		status, durationMS, missionID, store.TenantIDFromContext(ctx), attempt, seq)
	return err
}

func (s *PGMissionStore) ListMissionReceipts(ctx context.Context, missionID uuid.UUID) ([]store.MissionReceipt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT mission_id, attempt, seq, tool, action_class, status, COALESCE(reason, ''), args_digest, duration_ms, created_at
		 FROM mission_receipts WHERE mission_id = $1 AND tenant_id = $2 ORDER BY attempt, seq`,
		missionID, store.TenantIDFromContext(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.MissionReceipt{}
	for rows.Next() {
		var r store.MissionReceipt
		if err := rows.Scan(&r.MissionID, &r.Attempt, &r.Seq, &r.Tool, &r.ActionClass, &r.Status, &r.Reason, &r.ArgsDigest, &r.DurationMS, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

var _ store.MissionStore = (*PGMissionStore)(nil)

// nilJSON stores an empty raw JSON value as SQL NULL.
func nilJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}
