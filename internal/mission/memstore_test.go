package mission

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/storetest"
)

// memStore is an in-memory store.MissionStore for service tests. It passes
// the same conformance checks as the PostgreSQL and SQLite stores.
type memStore struct {
	mu       sync.Mutex
	missions map[uuid.UUID]*store.Mission
	events   []store.MissionEvent
	receipts []store.MissionReceipt
	// fail, when set, is consulted before every call; a non-nil error is
	// returned as if the database were unreachable (fault injection).
	fail func(op string) error
}

func newMemStore() *memStore { return &memStore{missions: map[uuid.UUID]*store.Mission{}} }

func TestMemStoreConformsToMissionStore(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	storetest.MissionLeases(t, newMemStore(), store.WithTenantID(context.Background(), a), store.WithTenantID(context.Background(), b))
}

var errDBDown = errors.New("injected: database unavailable")

func (s *memStore) injected(op string) error {
	if s.fail != nil {
		return s.fail(op)
	}
	return nil
}

func (s *memStore) get(ctx context.Context, id uuid.UUID) (*store.Mission, bool) {
	m, ok := s.missions[id]
	if !ok || m.TenantID != store.TenantIDFromContext(ctx) {
		return nil, false
	}
	return m, true
}

func (s *memStore) CreateMission(ctx context.Context, m *store.Mission, actor string) error {
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

func (s *memStore) GetMission(ctx context.Context, id uuid.UUID) (*store.Mission, error) {
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

func (s *memStore) ListMissions(context.Context, int) ([]store.Mission, error) { return nil, nil }

func (s *memStore) ListActiveMissionsAllTenants(context.Context) ([]store.Mission, error) {
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

func (s *memStore) TransitionMission(ctx context.Context, id uuid.UUID, from []string, to, actor, msg string, u store.MissionUpdate) (*store.Mission, error) {
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
	if u.Claim != nil && m.Attempt >= m.MaxAttempts {
		return nil, fmt.Errorf("%w: no attempts left", store.ErrMissionStateConflict)
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
		until := u.Claim.Until
		m.LeaseOwner, m.LeaseExpiresAt = u.Claim.Owner, &until
		m.Attempt++
	case u.ClearLease:
		m.LeaseOwner, m.LeaseExpiresAt = "", nil
	}
	cp := *m
	return &cp, nil
}

func (s *memStore) AppendMissionEvent(ctx context.Context, ev store.MissionEvent) error {
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

func (s *memStore) ListMissionEvents(ctx context.Context, id uuid.UUID) ([]store.MissionEvent, error) {
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

func (s *memStore) RenewMissionLease(ctx context.Context, id uuid.UUID, f store.MissionFence, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("renew"); err != nil {
		return err
	}
	m, ok := s.get(ctx, id)
	if !ok || m.LeaseOwner != f.Owner || m.Attempt != f.Attempt || !slices.Contains(store.MissionActiveStatuses, m.Status) {
		return store.ErrMissionLeaseLost
	}
	m.LeaseExpiresAt = &until
	return nil
}

func (s *memStore) BeginMissionReceipt(ctx context.Context, r store.MissionReceipt, f store.MissionFence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("receipt"); err != nil {
		return err
	}
	for _, x := range s.receipts {
		if x.MissionID == r.MissionID && x.Attempt == r.Attempt && x.Seq == r.Seq {
			return nil
		}
	}
	m, ok := s.get(ctx, r.MissionID)
	if !ok || m.LeaseOwner != f.Owner || m.Attempt != r.Attempt ||
		!slices.Contains([]string{store.MissionPreparing, store.MissionRunning, store.MissionVerifying}, m.Status) {
		return store.ErrMissionLeaseLost
	}
	r.CreatedAt = time.Now()
	s.receipts = append(s.receipts, r)
	return nil
}

func (s *memStore) CompleteMissionReceipt(ctx context.Context, id uuid.UUID, attempt, seq int, status string, ms int64) error {
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

func (s *memStore) ListMissionReceipts(ctx context.Context, id uuid.UUID) ([]store.MissionReceipt, error) {
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

// eventsFor returns the recorded events for one mission (test helper).
func (s *memStore) eventsFor(id uuid.UUID) []store.MissionEvent {
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
