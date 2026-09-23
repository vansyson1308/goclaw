package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// MetricType identifies the category of evolution metric.
type MetricType string

const (
	MetricRetrieval MetricType = "retrieval"
	MetricTool      MetricType = "tool"
)

// EvolutionMetric is a single recorded metric data point.
type EvolutionMetric struct {
	ID         uuid.UUID       `json:"id" db:"id"`
	TenantID   uuid.UUID       `json:"tenant_id" db:"tenant_id"`
	AgentID    uuid.UUID       `json:"agent_id" db:"agent_id"`
	SessionKey string          `json:"session_key" db:"session_key"`
	MetricType MetricType      `json:"metric_type" db:"metric_type"`
	MetricKey  string          `json:"metric_key" db:"metric_key"`
	Value      json.RawMessage `json:"value" db:"value"`
	CreatedAt  time.Time       `json:"created_at" db:"created_at"`
}

// ToolAggregate summarizes per-tool metrics over a period.
type ToolAggregate struct {
	ToolName      string  `json:"tool_name"`
	CallCount     int     `json:"call_count"`
	SuccessRate   float64 `json:"success_rate"`
	AvgDurationMs float64 `json:"avg_duration_ms"` // milliseconds (not time.Duration — JSON-friendly)
}

// RetrievalAggregate summarizes per-source retrieval metrics.
type RetrievalAggregate struct {
	Source     string  `json:"source"`
	QueryCount int     `json:"query_count"`
	UsageRate  float64 `json:"usage_rate"` // fraction of results used in reply
	AvgScore   float64 `json:"avg_score"`
}

// EvolutionMetricsStore manages self-evolution metrics (Stage 1).
type EvolutionMetricsStore interface {
	RecordMetric(ctx context.Context, metric EvolutionMetric) error
	QueryMetrics(ctx context.Context, agentID uuid.UUID, metricType MetricType, since time.Time, limit int) ([]EvolutionMetric, error)
	AggregateToolMetrics(ctx context.Context, agentID uuid.UUID, since time.Time) ([]ToolAggregate, error)
	AggregateRetrievalMetrics(ctx context.Context, agentID uuid.UUID, since time.Time) ([]RetrievalAggregate, error)
	Cleanup(ctx context.Context, olderThan time.Time) (int64, error)
}

// SuggestionType identifies the kind of evolution suggestion.
type SuggestionType string

const (
	SuggestThreshold SuggestionType = "threshold"
	SuggestToolOrder SuggestionType = "tool_order"
	SuggestSkillAdd  SuggestionType = "skill_add"
)

// Suggestion lifecycle statuses.
const (
	SuggestionPending    = "pending"
	SuggestionApproved   = "approved" // reviewed; advisory types stop here
	SuggestionRejected   = "rejected"
	SuggestionApplying   = "applying" // claimed; side effects in progress (skill_add)
	SuggestionApplied    = "applied"
	SuggestionRolledBack = "rolled_back"
)

// EvolutionSuggestion is a data-driven suggestion for agent improvement.
type EvolutionSuggestion struct {
	ID             uuid.UUID       `json:"id" db:"id"`
	TenantID       uuid.UUID       `json:"tenant_id" db:"tenant_id"`
	AgentID        uuid.UUID       `json:"agent_id" db:"agent_id"`
	SuggestionType SuggestionType  `json:"suggestion_type" db:"suggestion_type"`
	Suggestion     string          `json:"suggestion" db:"suggestion"`
	Rationale      string          `json:"rationale" db:"rationale"`
	Parameters     json.RawMessage `json:"parameters,omitempty" db:"parameters"`
	Status         string          `json:"status" db:"status"` // see Suggestion* constants
	ReviewedBy     string          `json:"reviewed_by,omitempty" db:"reviewed_by"`
	ReviewedAt     *time.Time      `json:"reviewed_at,omitempty" db:"reviewed_at"`
	CreatedAt      time.Time       `json:"created_at" db:"created_at"`

	// Apply/rollback bookkeeping (migration 000098 / SQLite v61).
	AppliedAt     *time.Time         `json:"applied_at,omitempty" db:"applied_at"`
	AppliedBy     string             `json:"applied_by,omitempty" db:"applied_by"`
	RolledBackAt  *time.Time         `json:"rolled_back_at,omitempty" db:"rolled_back_at"`
	RolledBackBy  string             `json:"rolled_back_by,omitempty" db:"rolled_back_by"`
	AppliedChange *AgentConfigChange `json:"applied_change,omitempty" db:"applied_change"`
	StateVersion  int                `json:"state_version" db:"state_version"`
}

// ConfigValue is a JSON value together with whether it was present at all.
// Presence matters for rollback: restoring "absent" deletes the key.
type ConfigValue struct {
	Present bool            `json:"present"`
	Value   json.RawMessage `json:"value,omitempty"`
}

// AgentConfigChange records one mutation of an agent JSONB config column at
// a key path, with the exact before/after state.
type AgentConfigChange struct {
	Column string      `json:"column"` // tools_config | other_config | memory_config
	Path   []string    `json:"path"`
	Before ConfigValue `json:"before"`
	After  ConfigValue `json:"after"`
}

// EvolutionEvent is an append-only audit record of a suggestion transition.
type EvolutionEvent struct {
	ID           uuid.UUID       `json:"id"`
	SuggestionID uuid.UUID       `json:"suggestion_id"`
	AgentID      uuid.UUID       `json:"agent_id"`
	Action       string          `json:"action"`
	FromStatus   string          `json:"from_status"`
	ToStatus     string          `json:"to_status"`
	Actor        string          `json:"actor"`
	Detail       json.RawMessage `json:"detail,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// SuggestionTransition describes an atomic status change, optionally with an
// agent config mutation performed under the same transaction and row locks.
type SuggestionTransition struct {
	ID     uuid.UUID
	From   []string // allowed current statuses; mismatch → ErrSuggestionStateConflict
	To     string
	Actor  string // authenticated identity; never client-supplied labels
	Action string // audit action name (approve, apply, rollback, ...)
	// Column names the agent JSONB column Mutate edits ("" = status only).
	Column string
	// Mutate receives the locked suggestion and the column's current value and
	// returns the new value plus the change record. Returning an error aborts
	// the whole transition (nothing is written).
	Mutate func(sg *EvolutionSuggestion, current json.RawMessage) (json.RawMessage, *AgentConfigChange, error)
	Detail map[string]any // extra audit detail
}

// ErrSuggestionStateConflict means the suggestion was not in an allowed
// status (stale UI, concurrent reviewer, or repeated request).
var ErrSuggestionStateConflict = errors.New("suggestion state conflict")

// EvolutionSuggestionStore manages suggestions (Stage 2).
type EvolutionSuggestionStore interface {
	CreateSuggestion(ctx context.Context, s EvolutionSuggestion) error
	ListSuggestions(ctx context.Context, agentID uuid.UUID, status string, limit int) ([]EvolutionSuggestion, error)
	UpdateSuggestionStatus(ctx context.Context, id uuid.UUID, status, reviewedBy string) error
	UpdateSuggestionParameters(ctx context.Context, id uuid.UUID, params json.RawMessage) error
	GetSuggestion(ctx context.Context, id uuid.UUID) (*EvolutionSuggestion, error)
	// TransitionSuggestion applies t atomically (tenant-scoped) and returns the
	// updated suggestion.
	TransitionSuggestion(ctx context.Context, t SuggestionTransition) (*EvolutionSuggestion, error)
	// ListSuggestionEvents returns the audit trail, oldest first.
	ListSuggestionEvents(ctx context.Context, suggestionID uuid.UUID) ([]EvolutionEvent, error)
}

// EvolutionConfigColumns are the only agent columns a transition may mutate.
var EvolutionConfigColumns = map[string]bool{"tools_config": true, "other_config": true, "memory_config": true}
