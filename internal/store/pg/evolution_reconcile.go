package pg

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ReconcileFinding is one inconsistent or legacy evolution record. The report
// is read-only: it never repairs data, it tells an operator what to review.
type ReconcileFinding struct {
	Kind         string     `json:"kind"`
	TenantID     uuid.UUID  `json:"tenant_id"`
	AgentID      *uuid.UUID `json:"agent_id,omitempty"`
	SkillID      *uuid.UUID `json:"skill_id,omitempty"`
	SuggestionID *uuid.UUID `json:"suggestion_id,omitempty"`
	Detail       string     `json:"detail"`
	Action       string     `json:"action"`
}

// staleApplyingAfter is how long a claim may stay in "applying" before it is
// reported as stuck (a crash between claim and commit).
const staleApplyingAfter = 15 * time.Minute

type reconcileQuery struct {
	kind, action, sql string
	scan              func(*sql.Rows) (ReconcileFinding, error)
}

// EvolutionReconcile scans all tenants (server-internal, no tenant scoping)
// for evolution records left inconsistent by earlier code or partial failures.
func EvolutionReconcile(ctx context.Context, db *sql.DB) ([]ReconcileFinding, error) {
	suggestionScan := func(detail string) func(*sql.Rows) (ReconcileFinding, error) {
		return func(rows *sql.Rows) (ReconcileFinding, error) {
			var f ReconcileFinding
			var agentID, sgID uuid.UUID
			var extra sql.NullString
			if err := rows.Scan(&f.TenantID, &agentID, &sgID, &extra); err != nil {
				return f, err
			}
			f.AgentID, f.SuggestionID = &agentID, &sgID
			f.Detail = detail
			if extra.Valid && extra.String != "" {
				f.Detail += ": " + extra.String
			}
			return f, nil
		}
	}
	skillScan := func(detail string) func(*sql.Rows) (ReconcileFinding, error) {
		return func(rows *sql.Rows) (ReconcileFinding, error) {
			var f ReconcileFinding
			var skillID uuid.UUID
			var sgID uuid.NullUUID
			var extra sql.NullString
			if err := rows.Scan(&f.TenantID, &skillID, &sgID, &extra); err != nil {
				return f, err
			}
			f.SkillID = &skillID
			if sgID.Valid {
				id := sgID.UUID
				f.SuggestionID = &id
			}
			f.Detail = detail
			if extra.Valid && extra.String != "" {
				f.Detail += ": " + extra.String
			}
			return f, nil
		}
	}

	queries := []reconcileQuery{
		{
			kind:   "legacy_threshold_applied",
			action: "Roll back via PATCH .../evolution/suggestions/{id} {\"status\":\"rolled_back\"}; the key it wrote is not read at runtime",
			sql: `SELECT s.tenant_id, s.agent_id, s.id, a.other_config->>'retrieval_threshold'
			      FROM agent_evolution_suggestions s JOIN agents a ON a.id = s.agent_id
			      WHERE s.status = 'applied' AND s.suggestion_type = 'threshold' AND s.applied_change IS NULL`,
			scan: suggestionScan("threshold applied by legacy code wrote other_config.retrieval_threshold"),
		},
		{
			kind:   "legacy_tenant_wide_tool_disable",
			action: "Review Builtin tools settings for the tenant; re-enable the tool if the disable was meant for one agent only",
			sql: `SELECT s.tenant_id, s.agent_id, s.id, s.parameters->>'tool'
			      FROM agent_evolution_suggestions s
			      JOIN builtin_tool_tenant_configs c ON c.tenant_id = s.tenant_id AND c.tool_name = s.parameters->>'tool'
			      WHERE s.status = 'applied' AND s.suggestion_type = 'tool_order' AND s.applied_change IS NULL
			        AND c.enabled = false`,
			scan: suggestionScan("tool_order applied by legacy code disabled the tool for the whole tenant"),
		},
		{
			kind:   "stuck_applying",
			action: "Check whether the side effect (e.g. skill creation) completed; then resolve the suggestion manually",
			sql: `SELECT s.tenant_id, s.agent_id, s.id, s.suggestion_type
			      FROM agent_evolution_suggestions s
			      WHERE s.status = 'applying' AND COALESCE(
			        (SELECT max(e.created_at) FROM agent_evolution_events e WHERE e.suggestion_id = s.id), s.created_at) < $1`,
			scan: suggestionScan("suggestion claimed for apply but never committed"),
		},
		{
			kind:   "skill_suggestion_half_applied",
			action: "Retry apply for the suggestion; it resumes from the recorded version",
			sql: `SELECT sg.tenant_id, sg.skill_id, sg.id, 'version ' || v.version
			      FROM skill_improvement_suggestions sg
			      JOIN skill_versions v ON v.created_from_suggestion_id = sg.id
			      WHERE sg.status <> 'applied'`,
			scan: skillScan("skill version recorded but suggestion not marked applied"),
		},
		{
			kind:   "skill_active_version_without_history",
			action: "Inspect the skill directory; record or restore the version before further evolution",
			sql: `SELECT s.tenant_id, s.id, NULL::uuid, 'version ' || s.version
			      FROM skills s
			      WHERE s.is_system = false AND s.status = 'active'
			        AND EXISTS (SELECT 1 FROM skill_versions v WHERE v.skill_id = s.id)
			        AND NOT EXISTS (SELECT 1 FROM skill_versions v WHERE v.skill_id = s.id AND v.version = s.version)`,
			scan: skillScan("active skill version has no version-history row"),
		},
	}

	var out []ReconcileFinding
	for _, q := range queries {
		var args []any
		if q.kind == "stuck_applying" {
			args = append(args, time.Now().Add(-staleApplyingAfter))
		}
		rows, err := db.QueryContext(ctx, q.sql, args...)
		if err != nil {
			return nil, fmt.Errorf("reconcile %s: %w", q.kind, err)
		}
		for rows.Next() {
			f, err := q.scan(rows)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("reconcile %s: %w", q.kind, err)
			}
			f.Kind, f.Action = q.kind, q.action
			out = append(out, f)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("reconcile %s: %w", q.kind, err)
		}
	}
	return out, nil
}
