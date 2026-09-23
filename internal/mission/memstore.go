package mission

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// MemoryStore is an in-memory store.MissionStore, used by tests and the
// offline evaluation suite. It passes the same conformance checks
// (internal/store/storetest) as the PostgreSQL and SQLite stores.
type MemoryStore struct {
	mu       sync.Mutex
	missions map[uuid.UUID]*store.Mission
	events   []store.MissionEvent
	receipts []store.MissionReceipt
	// fail, when set, is consulted before every call; a non-nil error is
	// returned as if the database were unreachable (fault injection).
	fail func(op string) error
	// skew shifts the store's clock (lease expiry is judged by it, like the
	// database clock), so tests can expire leases without waiting.
	skew time.Duration
}

// NewMemoryStore returns an empty in-memory mission store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{missions: map[uuid.UUID]*store.Mission{}}
}

func (s *MemoryStore) now() time.Time { return time.Now().Add(s.skew) }

// advance moves the store clock forward.
func (s *MemoryStore) advance(d time.Duration) {
	s.mu.Lock()
	s.skew += d
	s.mu.Unlock()
}

func (s *MemoryStore) injected(op string) error {
	if s.fail != nil {
		return s.fail(op)
	}
	return nil
}

func (s *MemoryStore) get(ctx context.Context, id uuid.UUID) (*store.Mission, bool) {
	m, ok := s.missions[id]
	if !ok || m.TenantID != store.TenantIDFromContext(ctx) {
		return nil, false
	}
	return m, true
}

func (s *MemoryStore) CreateMission(ctx context.Context, m *store.Mission, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("create"); err != nil {
		return err
	}
	m.ID, m.TenantID, m.CreatedAt = uuid.New(), store.TenantIDFromContext(ctx), time.Now()
	if m.MaxAttempts <= 0 {
		m.MaxAttempts = 1
	}
	if m.Status == "" {
		m.Status = store.MissionPlanned
	}
	cp := *m
	s.missions[m.ID] = &cp
	s.events = append(s.events, store.MissionEvent{MissionID: m.ID, Kind: "transition", ToStatus: m.Status, Actor: actor})
	return nil
}

func (s *MemoryStore) GetMission(ctx context.Context, id uuid.UUID) (*store.Mission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("get"); err != nil {
		return nil, err
	}
	m, ok := s.get(ctx, id)
	if !ok {
		return nil, nil
	}
	cp := *m
	return &cp, nil
}

func (s *MemoryStore) ListMissions(context.Context, int) ([]store.Mission, error) { return nil, nil }

func (s *MemoryStore) ListActiveMissionsAllTenants(context.Context) ([]store.Mission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("list_active"); err != nil {
		return nil, err
	}
	var out []store.Mission
	for _, m := range s.missions {
		if slices.Contains(store.MissionActiveStatuses, m.Status) {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (s *MemoryStore) TransitionMission(ctx context.Context, id uuid.UUID, from []string, to, actor, msg string, u store.MissionUpdate) (*store.Mission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("transition:" + to); err != nil {
		return nil, err
	}
	m, ok := s.get(ctx, id)
	if !ok {
		return nil, store.ErrMissionNotFound
	}
	if !slices.Contains(from, m.Status) {
		return nil, fmt.Errorf("%w: %s", store.ErrMissionStateConflict, m.Status)
	}
	if f := u.Fence; f != nil && (m.LeaseOwner != f.Owner || m.Attempt != f.Attempt) {
		return nil, fmt.Errorf("%w: %w", store.ErrMissionStateConflict, store.ErrMissionLeaseLost)
	}
	if u.RequireLeaseExpired && m.LeaseExpiresAt != nil && !m.LeaseExpiresAt.Before(s.now()) {
		return nil, fmt.Errorf("%w: lease has not expired", store.ErrMissionStateConflict)
	}
	if u.Claim != nil && m.Attempt >= m.MaxAttempts {
		return nil, fmt.Errorf("%w: %w", store.ErrMissionStateConflict, store.ErrMissionNoAttempts)
	}
	s.events = append(s.events, store.MissionEvent{MissionID: id, Kind: "transition", FromStatus: m.Status, ToStatus: to, Actor: actor, Message: msg})
	m.Status = to
	m.StateVersion++
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&m.StatusReason, u.StatusReason)
	set(&m.WorkspacePath, u.WorkspacePath)
	set(&m.BaseRevision, u.BaseRevision)
	set(&m.Diff, u.Diff)
	set(&m.Summary, u.Summary)
	set(&m.Executor, u.Executor)
	if u.Verification != nil {
		m.Verification = u.Verification
	}
	if u.DiffTruncated != nil {
		m.DiffTruncated = *u.DiffTruncated
	}
	if u.ChangedFiles != nil {
		m.ChangedFiles = u.ChangedFiles
	}
	if u.InputTokens != nil {
		m.InputTokens = *u.InputTokens
	}
	if u.OutputTokens != nil {
		m.OutputTokens = *u.OutputTokens
	}
	if u.CostUSD != nil {
		m.CostUSD = u.CostUSD
	}
	if u.Iterations != nil {
		m.Iterations = *u.Iterations
	}
	if u.StartedAt != nil {
		m.StartedAt = u.StartedAt
	}
	if u.FinishedAt != nil {
		m.FinishedAt = u.FinishedAt
	}
	if u.UsageIncomplete != nil {
		m.UsageIncomplete = *u.UsageIncomplete
	}
	switch {
	case u.Claim != nil:
		until := s.now().Add(u.Claim.TTL)
		m.LeaseOwner, m.LeaseExpiresAt = u.Claim.Owner, &until
		m.Attempt++
	case u.ClearLease:
		m.LeaseOwner, m.LeaseExpiresAt = "", nil
	}
	cp := *m
	return &cp, nil
}

func (s *MemoryStore) AppendMissionEvent(ctx context.Context, ev store.MissionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("event"); err != nil {
		return err
	}
	if _, ok := s.get(ctx, ev.MissionID); !ok {
		return store.ErrMissionNotFound
	}
	s.events = append(s.events, ev)
	return nil
}

func (s *MemoryStore) ListMissionEvents(ctx context.Context, id uuid.UUID) ([]store.MissionEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.get(ctx, id); !ok {
		return nil, nil
	}
	var out []store.MissionEvent
	for _, e := range s.events {
		if e.MissionID == id {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *MemoryStore) RenewMissionLease(ctx context.Context, id uuid.UUID, f store.MissionFence, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("renew"); err != nil {
		return err
	}
	m, ok := s.get(ctx, id)
	if !ok || m.LeaseOwner != f.Owner || m.Attempt != f.Attempt || !slices.Contains(store.MissionActiveStatuses, m.Status) {
		return store.ErrMissionLeaseLost
	}
	until := s.now().Add(ttl)
	m.LeaseExpiresAt = &until
	return nil
}

func (s *MemoryStore) BeginMissionReceipt(ctx context.Context, r store.MissionReceipt, f store.MissionFence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("receipt"); err != nil {
		return err
	}
	m, ok := s.get(ctx, r.MissionID)
	if !ok || r.Attempt != f.Attempt || m.LeaseOwner != f.Owner || m.Attempt != r.Attempt || m.Status != store.MissionRunning {
		return store.ErrMissionLeaseLost
	}
	for _, x := range s.receipts {
		if x.MissionID == r.MissionID && x.Attempt == r.Attempt && x.Seq == r.Seq {
			return nil
		}
	}
	r.CreatedAt = time.Now()
	s.receipts = append(s.receipts, r)
	return nil
}

func (s *MemoryStore) CompleteMissionReceipt(ctx context.Context, id uuid.UUID, attempt, seq int, status string, ms int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("receipt_complete"); err != nil {
		return err
	}
	if _, ok := s.get(ctx, id); !ok {
		return nil
	}
	for i := range s.receipts {
		r := &s.receipts[i]
		if r.MissionID == id && r.Attempt == attempt && r.Seq == seq && r.Status == store.ReceiptStarted {
			r.Status, r.DurationMS = status, ms
		}
	}
	return nil
}

func (s *MemoryStore) ListMissionReceipts(ctx context.Context, id uuid.UUID) ([]store.MissionReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []store.MissionReceipt{}
	if _, ok := s.get(ctx, id); !ok {
		return out, nil
	}
	for _, r := range s.receipts {
		if r.MissionID == id {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b store.MissionReceipt) int {
		if a.Attempt != b.Attempt {
			return a.Attempt - b.Attempt
		}
		return a.Seq - b.Seq
	})
	return out, nil
}

// eventsFor returns the recorded events for one mission.
func (s *MemoryStore) eventsFor(id uuid.UUID) []store.MissionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.MissionEvent
	for _, e := range s.events {
		if e.MissionID == id {
			out = append(out, e)
		}
	}
	return out
}

var _ store.MissionStore = (*MemoryStore)(nil)
