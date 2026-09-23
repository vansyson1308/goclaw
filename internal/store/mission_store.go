package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Mission statuses (see docs/mission-control/MISSIONS.md).
const (
	MissionPlanned   = "planned"
	MissionPreparing = "preparing"
	MissionRunning   = "running"
	MissionVerifying = "verifying"
	MissionSucceeded = "succeeded"
	MissionPartial   = "partial"
	MissionFailed    = "failed"
	MissionBlocked   = "blocked"
	MissionCancelled = "cancelled"
)

// MissionActiveStatuses are non-terminal.
var MissionActiveStatuses = []string{MissionPlanned, MissionPreparing, MissionRunning, MissionVerifying}

// ErrMissionStateConflict means the mission was not in an allowed status.
var ErrMissionStateConflict = errors.New("mission state conflict")

// ErrMissionNotFound means no mission with that ID exists in the tenant.
var ErrMissionNotFound = errors.New("mission not found")

// Mission is a durable, verifiable unit of agent work.
type Mission struct {
	ID             uuid.UUID       `json:"id"`
	TenantID       uuid.UUID       `json:"tenant_id"`
	OwnerID        string          `json:"owner_id"`
	AgentKey       string          `json:"agent_key"`
	Title          string          `json:"title"`
	Contract       json.RawMessage `json:"contract"`
	ContractDigest string          `json:"contract_digest"`
	Status         string          `json:"status"`
	StatusReason   string          `json:"status_reason,omitempty"`
	Executor       string          `json:"executor,omitempty"`
	WorkspacePath  string          `json:"workspace_path,omitempty"`
	BaseRevision   string          `json:"base_revision,omitempty"`
	Verification   json.RawMessage `json:"verification,omitempty"` // []mission.CriterionResult
	Diff           string          `json:"diff,omitempty"`
	DiffTruncated  bool            `json:"diff_truncated,omitempty"`
	ChangedFiles   []string        `json:"changed_files,omitempty"`
	Summary        string          `json:"summary,omitempty"` // agent's final message (narrative, not evidence)
	InputTokens    int64           `json:"input_tokens"`
	OutputTokens   int64           `json:"output_tokens"`
	CostUSD        *float64        `json:"cost_usd,omitempty"` // nil = unknown, never assumed zero
	Iterations     int             `json:"iterations"`
	StateVersion   int             `json:"state_version"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
}

// MissionUpdate carries optional field updates applied with a transition.
type MissionUpdate struct {
	StatusReason  *string
	Executor      *string
	WorkspacePath *string
	BaseRevision  *string
	Verification  json.RawMessage
	Diff          *string
	DiffTruncated *bool
	ChangedFiles  []string
	Summary       *string
	InputTokens   *int64
	OutputTokens  *int64
	CostUSD       *float64
	Iterations    *int
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

// MissionEvent is an append-only audit/progress record.
type MissionEvent struct {
	ID         uuid.UUID       `json:"id"`
	MissionID  uuid.UUID       `json:"mission_id"`
	Kind       string          `json:"kind"` // transition | note
	FromStatus string          `json:"from_status,omitempty"`
	ToStatus   string          `json:"to_status,omitempty"`
	Actor      string          `json:"actor"`
	Message    string          `json:"message,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// MissionStore persists missions. All methods are tenant-scoped from ctx
// except ListActiveMissionsAllTenants (server-internal recovery).
type MissionStore interface {
	CreateMission(ctx context.Context, m *Mission, actor string) error
	GetMission(ctx context.Context, id uuid.UUID) (*Mission, error)
	ListMissions(ctx context.Context, limit int) ([]Mission, error)
	// TransitionMission moves a mission from one of from to to, applying u
	// and appending a transition event atomically (compare-and-set).
	TransitionMission(ctx context.Context, id uuid.UUID, from []string, to, actor, message string, u MissionUpdate) (*Mission, error)
	AppendMissionEvent(ctx context.Context, ev MissionEvent) error
	ListMissionEvents(ctx context.Context, missionID uuid.UUID) ([]MissionEvent, error)
	ListActiveMissionsAllTenants(ctx context.Context) ([]Mission, error)
}
