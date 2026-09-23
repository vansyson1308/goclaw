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

// ErrMissionLeaseLost means the caller no longer holds the mission's lease
// (another worker claimed it, it was cancelled, or the attempt moved on).
// It is also an ErrMissionStateConflict.
var ErrMissionLeaseLost = errors.New("mission lease lost")

// Receipt statuses. A "started" receipt without a later ok/error means the
// tool may have run but its outcome was never acknowledged (crash).
const (
	ReceiptDenied  = "denied"
	ReceiptStarted = "started"
	ReceiptOK      = "ok"
	ReceiptError   = "error"
)

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
	// UsageIncomplete is set when an attempt ended without reporting usage
	// (e.g. the process died); totals then undercount and cost is unknown.
	UsageIncomplete bool       `json:"usage_incomplete,omitempty"`
	Attempt         int        `json:"attempt"`
	MaxAttempts     int        `json:"max_attempts"`
	LeaseOwner      string     `json:"lease_owner,omitempty"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`
	StateVersion    int        `json:"state_version"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
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

	UsageIncomplete *bool
	// Fence makes the transition conditional on the caller still holding
	// the lease for that attempt (fencing token = owner + attempt).
	Fence *MissionFence
	// Claim takes the lease and starts the next attempt (attempt+1). It is
	// refused with ErrMissionStateConflict when no attempts are left.
	Claim *MissionClaim
	// ClearLease releases the lease (terminal transitions, requeue).
	ClearLease bool
}

// MissionFence identifies one worker's hold on one attempt.
type MissionFence struct {
	Owner   string
	Attempt int
}

// MissionClaim starts an attempt held by Owner until Until.
type MissionClaim struct {
	Owner string
	Until time.Time
}

// MissionReceipt records one tool call made during a mission attempt. It is
// written before the tool runs (write-ahead) so a side effect is never
// performed without a durable record, and completed afterwards.
type MissionReceipt struct {
	MissionID   uuid.UUID `json:"mission_id"`
	Attempt     int       `json:"attempt"`
	Seq         int       `json:"seq"`
	Tool        string    `json:"tool"`
	ActionClass string    `json:"action_class"`
	Status      string    `json:"status"` // denied | started | ok | error
	Reason      string    `json:"reason,omitempty"`
	ArgsDigest  string    `json:"args_digest"`
	DurationMS  int64     `json:"duration_ms"`
	CreatedAt   time.Time `json:"created_at"`
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
	// RenewMissionLease extends the lease held by fence while the mission is
	// active. It returns ErrMissionLeaseLost when the lease is not held.
	RenewMissionLease(ctx context.Context, id uuid.UUID, fence MissionFence, until time.Time) error
	// BeginMissionReceipt inserts a receipt, fenced by the lease. A duplicate
	// (mission, attempt, seq) is ignored. Denied receipts use the same path.
	BeginMissionReceipt(ctx context.Context, r MissionReceipt, fence MissionFence) error
	// CompleteMissionReceipt records the outcome of a started receipt.
	CompleteMissionReceipt(ctx context.Context, missionID uuid.UUID, attempt, seq int, status string, durationMS int64) error
	ListMissionReceipts(ctx context.Context, missionID uuid.UUID) ([]MissionReceipt, error)
}
