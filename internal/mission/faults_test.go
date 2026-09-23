package mission

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Deterministic fault tests for durable execution (Phase D). A "worker" is a
// Service; several share one store like several gateway processes share one
// database. A crash or network partition is simulated by cutting one
// worker's access to the store; lease expiry is simulated with a clock
// shifted forward, so no test waits for a real TTL to pass.

// partitionable cuts one worker off from the shared store.
type partitionable struct {
	store.MissionStore
	down atomic.Bool
}

func (p *partitionable) check() error {
	if p.down.Load() {
		return errDBDown
	}
	return nil
}

func (p *partitionable) GetMission(ctx context.Context, id uuid.UUID) (*store.Mission, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	return p.MissionStore.GetMission(ctx, id)
}

func (p *partitionable) TransitionMission(ctx context.Context, id uuid.UUID, from []string, to, actor, msg string, u store.MissionUpdate) (*store.Mission, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	return p.MissionStore.TransitionMission(ctx, id, from, to, actor, msg, u)
}

func (p *partitionable) AppendMissionEvent(ctx context.Context, ev store.MissionEvent) error {
	if err := p.check(); err != nil {
		return err
	}
	return p.MissionStore.AppendMissionEvent(ctx, ev)
}

func (p *partitionable) RenewMissionLease(ctx context.Context, id uuid.UUID, f store.MissionFence, ttl time.Duration) error {
	if err := p.check(); err != nil {
		return err
	}
	return p.MissionStore.RenewMissionLease(ctx, id, f, ttl)
}

func (p *partitionable) ListActiveMissionsAllTenants(ctx context.Context) ([]store.Mission, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	return p.MissionStore.ListActiveMissionsAllTenants(ctx)
}

// hangRunner stands in for an agent that is mid-run when its worker dies:
// it optionally leaves a side effect in the workspace, then blocks until
// its context ends and records why it was stopped.
type hangRunner struct {
	sideEffect bool
	started    chan struct{}
	once       sync.Once
	mu         sync.Mutex
	cause      error
	inputs     []RunInput
}

func newHangRunner(sideEffect bool) *hangRunner {
	return &hangRunner{sideEffect: sideEffect, started: make(chan struct{})}
}

func (h *hangRunner) RunMission(ctx context.Context, in RunInput) (*RunOutput, error) {
	h.mu.Lock()
	h.inputs = append(h.inputs, in)
	h.mu.Unlock()
	if h.sideEffect {
		_ = os.WriteFile(filepath.Join(in.Workspace, "partial.txt"), []byte("half-done work from a crashed attempt\n"), 0o644)
	}
	h.once.Do(func() { close(h.started) })
	<-ctx.Done()
	h.mu.Lock()
	h.cause = context.Cause(ctx)
	h.mu.Unlock()
	return nil, ctx.Err()
}

func (h *hangRunner) stopCause() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cause
}

type cluster struct {
	t      *testing.T
	shared *memStore
	root   string
	ctx    context.Context
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	newTestService(t, &fakeRunner{}) // skips when go/git are missing
	root := t.TempDir()
	seedRepo(t, filepath.Join(root, "sources"))
	return &cluster{t: t, shared: newMemStore(), root: root, ctx: store.WithTenantID(context.Background(), uuid.New())}
}

// worker starts a service that shares the cluster's store. skew shifts the
// worker's own clock only; lease expiry is always judged by the store.
func (c *cluster) worker(name string, r AgentRunner, ttl, skew time.Duration) (*Service, *partitionable) {
	c.t.Helper()
	p := &partitionable{MissionStore: c.shared}
	svc, err := NewService(Config{
		SourceRoot: filepath.Join(c.root, "sources"), DataRoot: filepath.Join(c.root, "data"),
		WorkerID: name, LeaseTTL: ttl, Now: func() time.Time { return time.Now().Add(skew) },
	}, p, r, HostExecutor{})
	must(c.t, err)
	return svc, p
}

func (c *cluster) get(id uuid.UUID) *store.Mission {
	m, _ := c.shared.GetMission(c.ctx, id)
	return m
}

func waitDone(t *testing.T, svc *Service, within time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { svc.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("worker did not stop within %s", within)
	}
}

func eventMessages(st *memStore, id uuid.UUID) string {
	var b strings.Builder
	for _, e := range st.eventsFor(id) {
		b.WriteString(e.FromStatus + ">" + e.ToStatus + ": " + e.Message + "\n")
	}
	return b.String()
}

const expired = time.Hour // store clock advance that expires every lease

// A worker dies mid-run (before its provider call returns). Another worker
// retries the mission in a fresh workspace; the result is honest about the
// lost attempt.
func TestCrashedWorkerAttemptIsRetried(t *testing.T) {
	c := newCluster(t)
	hang := newHangRunner(false)
	a, aStore := c.worker("worker-a", hang, 600*time.Millisecond, 0)
	m, err := a.Create(c.ctx, contractJSON(), "alice")
	must(t, err)
	<-hang.started
	aStore.down.Store(true) // worker A crashes / loses the database
	waitDone(t, a, 5*time.Second)
	if !errors.Is(hang.stopCause(), errLeaseUnrenewable) {
		t.Fatalf("A should stop itself once it cannot renew its lease, cause: %v", hang.stopCause())
	}
	if got := c.get(m.ID); got.Status != StatusRunning || got.LeaseOwner != "worker-a" {
		t.Fatalf("A must not have written anything after losing the store: %s %q", got.Status, got.LeaseOwner)
	}

	c.shared.advance(expired) // every existing lease has now run out
	b, _ := c.worker("worker-b", &fakeRunner{reply: "fixed", edit: honestFix}, time.Minute, 0)
	rep, err := b.Recover(context.Background())
	must(t, err)
	if rep.Retried != 1 {
		t.Fatalf("recovery report %+v", rep)
	}
	b.Wait()
	got := c.get(m.ID)
	if got.Status != StatusSucceeded || got.Attempt != 2 || !got.UsageIncomplete || got.LeaseOwner != "" {
		t.Fatalf("after retry: status=%s attempt=%d incomplete=%v lease=%q (%s)", got.Status, got.Attempt, got.UsageIncomplete, got.LeaseOwner, got.StatusReason)
	}
	if !strings.Contains(got.WorkspacePath, "attempt-2") || !strings.Contains(got.StatusReason, "attempt 2/2") {
		t.Fatalf("attempt 2 not visible: %s / %s", got.WorkspacePath, got.StatusReason)
	}
	if ev := eventMessages(c.shared, m.ID); !strings.Contains(ev, "attempt 1/2 was interrupted while running") {
		t.Fatalf("lost attempt not in the audit trail:\n%s", ev)
	}
	// The retry ran as a new conversation, not a continuation of the dead one.
	if hang.inputs[0].SessionKey == "" || strings.HasSuffix(hang.inputs[0].SessionKey, ":a2") {
		t.Fatalf("attempt 1 session key %q", hang.inputs[0].SessionKey)
	}
}

// Work a crashed attempt did in its workspace (a side effect performed but
// never acknowledged) cannot leak into the retried attempt's evidence.
func TestSideEffectsOfCrashedAttemptDoNotLeak(t *testing.T) {
	c := newCluster(t)
	hang := newHangRunner(true)
	a, aStore := c.worker("worker-a", hang, 600*time.Millisecond, 0)
	m, err := a.Create(c.ctx, contractJSON(), "alice")
	must(t, err)
	<-hang.started
	aStore.down.Store(true)
	waitDone(t, a, 5*time.Second)

	c.shared.advance(expired) // every existing lease has now run out
	b, _ := c.worker("worker-b", &fakeRunner{reply: "fixed", edit: honestFix}, time.Minute, 0)
	_, err = b.Recover(context.Background())
	must(t, err)
	b.Wait()
	got := c.get(m.ID)
	if got.Status != StatusSucceeded || slices.Contains(got.ChangedFiles, "partial.txt") || strings.Contains(got.Diff, "half-done") {
		t.Fatalf("crashed attempt leaked: %s %v", got.Status, got.ChangedFiles)
	}
	// Attempt 1's workspace is kept separately for inspection.
	old := filepath.Join(filepath.Dir(filepath.Dir(got.WorkspacePath)), "attempt-1", "workspace", "partial.txt")
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("attempt 1 workspace not preserved: %v", err)
	}
}

// A partitioned worker whose lease was taken over must stop and must not
// write anything when the partition heals.
func TestStaleWorkerIsFencedAfterTakeover(t *testing.T) {
	c := newCluster(t)
	hangA := newHangRunner(false)
	a, aStore := c.worker("worker-a", hangA, 900*time.Millisecond, 0)
	m, err := a.Create(c.ctx, contractJSON(), "alice")
	must(t, err)
	<-hangA.started

	aStore.down.Store(true) // partition: A still runs, but cannot reach the store
	gate := make(chan struct{})
	bRunner := &fakeRunner{reply: "fixed", edit: honestFix, block: gate, started: make(chan struct{})}
	c.shared.advance(expired) // every existing lease has now run out
	b, _ := c.worker("worker-b", bRunner, time.Minute, 0)
	rep, err := b.Recover(context.Background())
	must(t, err)
	if rep.Retried != 1 {
		t.Fatalf("takeover: %+v", rep)
	}
	aStore.down.Store(false) // partition heals (well within A's TTL)

	waitDone(t, a, 5*time.Second) // A's next heartbeat finds the lease gone
	<-bRunner.started             // B holds attempt 2
	if !errors.Is(hangA.stopCause(), errLeaseLost) {
		t.Fatalf("stale worker stopped for the wrong reason: %v", hangA.stopCause())
	}
	// A's fence no longer works for anything.
	if _, err := c.shared.TransitionMission(c.ctx, m.ID, []string{StatusRunning}, StatusVerifying, "x", "",
		store.MissionUpdate{Fence: &store.MissionFence{Owner: "worker-a", Attempt: 1}}); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("stale fence accepted: %v", err)
	}
	close(gate)
	b.Wait()
	got := c.get(m.ID)
	if got.Status != StatusSucceeded || got.Attempt != 2 {
		t.Fatalf("final: %s attempt %d (%s)", got.Status, got.Attempt, got.StatusReason)
	}
	verifying := 0
	for _, e := range c.shared.eventsFor(m.ID) {
		if e.ToStatus == StatusVerifying {
			verifying++
		}
	}
	if verifying != 1 {
		t.Fatalf("expected exactly one verification (by B), got %d:\n%s", verifying, eventMessages(c.shared, m.ID))
	}
}

// A cancel issued through another gateway reaches the worker running the
// mission (via its heartbeat), and recovery never resurrects it.
func TestCancelReachesOtherWorkerAndIsNotRetried(t *testing.T) {
	c := newCluster(t)
	hang := newHangRunner(false)
	a, _ := c.worker("worker-a", hang, 600*time.Millisecond, 0)
	m, err := a.Create(c.ctx, contractJSON(), "alice")
	must(t, err)
	<-hang.started
	c.shared.advance(expired) // every existing lease has now run out
	b, _ := c.worker("worker-b", &fakeRunner{reply: "fixed", edit: honestFix}, time.Minute, 0)
	start := time.Now()
	if _, err := b.Cancel(c.ctx, m.ID, "bob", ""); err != nil {
		t.Fatal(err)
	}
	waitDone(t, a, 3*time.Second)
	if !errors.Is(hang.stopCause(), errLeaseLost) || time.Since(start) > 2*time.Second {
		t.Fatalf("remote cancel not propagated promptly: %v after %s", hang.stopCause(), time.Since(start))
	}
	rep, err := b.Recover(context.Background())
	must(t, err)
	b.Wait()
	if got := c.get(m.ID); got.Status != StatusCancelled || rep != (RecoveryReport{}) {
		t.Fatalf("cancelled mission touched by recovery: %s %+v", got.Status, rep)
	}
}

// Attempts are bounded: the last lost attempt fails the mission.
func TestAttemptsAreBounded(t *testing.T) {
	c := newCluster(t)
	hang := newHangRunner(false)
	a, aStore := c.worker("worker-a", hang, 600*time.Millisecond, 0)
	m, err := a.Create(c.ctx, contractWith(func(ct *Contract) { ct.Limits.MaxAttempts = 1 }), "alice")
	must(t, err)
	<-hang.started
	aStore.down.Store(true)
	waitDone(t, a, 5*time.Second)
	c.shared.advance(expired) // every existing lease has now run out
	b, _ := c.worker("worker-b", &fakeRunner{reply: "fixed", edit: honestFix}, time.Minute, 0)
	rep, err := b.Recover(context.Background())
	must(t, err)
	b.Wait()
	got := c.get(m.ID)
	if rep.Failed != 1 || got.Status != StatusFailed || !strings.Contains(got.StatusReason, "no attempts left") || !got.UsageIncomplete {
		t.Fatalf("exhausted: %+v %s (%s)", rep, got.Status, got.StatusReason)
	}
}

// A live worker's lease is respected: recovery on another gateway with an
// accurate clock does nothing.
func TestRecoveryLeavesLiveLeaseAlone(t *testing.T) {
	c := newCluster(t)
	hang := newHangRunner(false)
	a, _ := c.worker("worker-a", hang, 3*time.Second, 0)
	m, err := a.Create(c.ctx, contractJSON(), "alice")
	must(t, err)
	<-hang.started
	b, _ := c.worker("worker-b", &fakeRunner{}, time.Minute, 0)
	rep, err := b.Recover(context.Background())
	must(t, err)
	if rep != (RecoveryReport{}) || c.get(m.ID).Attempt != 1 {
		t.Fatalf("recovery interfered with a live lease: %+v", rep)
	}
	_, _ = a.Cancel(c.ctx, m.ID, "alice", "")
	a.Wait()
}

// A mission queued in a process that died (planned, never claimed) is
// started by the next recovery pass.
func TestRecoveryResumesQueuedMission(t *testing.T) {
	c := newCluster(t)
	b, _ := c.worker("worker-b", &fakeRunner{reply: "fixed", edit: honestFix}, time.Minute, 0)
	ct, err := ParseContract(contractJSON())
	must(t, err)
	pins, err := b.pinInputs(ct)
	must(t, err)
	m := &store.Mission{OwnerID: "alice", AgentKey: ct.Agent, Title: ct.Title, Contract: ct.Canonical(),
		ContractDigest: ct.Digest(), MaxAttempts: 2, Pins: mustJSON(t, pins)}
	must(t, c.shared.CreateMission(c.ctx, m, "alice")) // queued by a dead process
	rep, err := b.Recover(context.Background())
	must(t, err)
	b.Wait()
	if got := c.get(m.ID); rep.Resumed != 1 || got.Status != StatusSucceeded || got.Attempt != 1 {
		t.Fatalf("queued mission: %+v %s attempt %d", rep, got.Status, got.Attempt)
	}
	// A mission already queued or running here is never started twice
	// (recovery racing with Create).
	b.mu.Lock()
	b.running[m.ID] = func(error) {}
	b.mu.Unlock()
	if b.start(c.ctx, m.ID, ct) {
		t.Fatal("duplicate local start")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return b
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The agent run hitting the mission timeout (e.g. a hung provider call) is
// a failed run, never a success.
func TestProviderTimeoutIsFailure(t *testing.T) {
	c := newCluster(t)
	hang := newHangRunner(false)
	a, _ := c.worker("worker-a", hang, time.Minute, 0)
	m, err := a.Create(c.ctx, contractWith(func(ct *Contract) { ct.Limits.TimeoutSeconds = 1 }), "alice")
	must(t, err)
	a.Wait()
	got := c.get(m.ID)
	if got.Status != StatusFailed || !strings.Contains(got.StatusReason, "agent run failed: context deadline exceeded") || !got.UsageIncomplete {
		t.Fatalf("timeout: %s (%s)", got.Status, got.StatusReason)
	}
}

// A repeated completion (a duplicate callback after the mission finished)
// changes nothing and records nothing.
func TestDuplicateCompletionIsIgnored(t *testing.T) {
	c := newCluster(t)
	a, _ := c.worker("worker-a", &fakeRunner{reply: "fixed", edit: honestFix}, time.Minute, 0)
	m, err := a.Create(c.ctx, contractJSON(), "alice")
	must(t, err)
	a.Wait()
	before := len(c.shared.eventsFor(m.ID))
	dup := &attempt{s: a, id: m.ID, fence: store.MissionFence{Owner: "worker-a", Attempt: 1}, log: slogDiscard()}
	dup.finish(c.ctx, StatusVerifying, StatusFailed, "late duplicate", store.MissionUpdate{})
	got := c.get(m.ID)
	if got.Status != StatusSucceeded || len(c.shared.eventsFor(m.ID)) != before {
		t.Fatalf("duplicate completion changed the mission: %s, events %d→%d", got.Status, before, len(c.shared.eventsFor(m.ID)))
	}
}

// A brief database error is retried; the attempt still completes.
func TestTransientStoreErrorIsRetried(t *testing.T) {
	c := newCluster(t)
	var failed atomic.Int32
	c.shared.fail = func(op string) error {
		if op == "transition:"+StatusVerifying && failed.Add(1) == 1 {
			return errDBDown
		}
		return nil
	}
	a, _ := c.worker("worker-a", &fakeRunner{reply: "fixed", edit: honestFix}, time.Minute, 0)
	m, err := a.Create(c.ctx, contractJSON(), "alice")
	must(t, err)
	a.Wait()
	if got := c.get(m.ID); got.Status != StatusSucceeded || got.Attempt != 1 || failed.Load() < 2 {
		t.Fatalf("transient error: %s attempt %d, calls %d", got.Status, got.Attempt, failed.Load())
	}
}

// A legacy mission (created before leases existed) left active is retried
// under the new rules instead of being killed, and ends honestly.
func TestLegacyActiveMissionIsRecovered(t *testing.T) {
	c := newCluster(t)
	stale := &store.Mission{Title: "t", Status: StatusRunning, MaxAttempts: 1}
	must(t, c.shared.CreateMission(c.ctx, stale, "alice"))
	b, _ := c.worker("worker-b", &fakeRunner{}, time.Minute, 0)
	rep, err := b.Recover(context.Background())
	must(t, err)
	b.Wait()
	got := c.get(stale.ID)
	if rep.Retried != 1 || got.Status != StatusBlocked || !strings.Contains(got.StatusReason, "contract") {
		t.Fatalf("legacy: %+v %s (%s)", rep, got.Status, got.StatusReason)
	}
}

func TestAggregateUsageNeverTreatsUnknownAsZero(t *testing.T) {
	one, two := 0.5, 0.25
	cases := []struct {
		name       string
		prev       store.Mission
		out        *RunOutput
		wantCost   *float64
		wantOK     bool
		incomplete bool
	}{
		{"first attempt priced", store.Mission{}, &RunOutput{CostUSD: &one, InputTokens: 10}, &one, true, false},
		{"first attempt unpriced", store.Mission{}, &RunOutput{InputTokens: 10}, nil, false, false},
		{"run returned nothing", store.Mission{}, nil, nil, false, true},
		{"after lost attempt", store.Mission{UsageIncomplete: true}, &RunOutput{CostUSD: &two}, &two, false, true},
		{"prior unpriced usage", store.Mission{InputTokens: 5}, &RunOutput{CostUSD: &two}, &two, false, true},
		{"this attempt unpriced", store.Mission{CostUSD: &one, InputTokens: 5}, &RunOutput{InputTokens: 1}, nil, false, true},
	}
	for _, tc := range cases {
		u, ok := aggregateUsage(&tc.prev, tc.out)
		if ok != tc.wantOK || (u.UsageIncomplete != nil && *u.UsageIncomplete) != tc.incomplete ||
			(tc.wantCost == nil) != (u.CostUSD == nil) || (tc.wantCost != nil && *u.CostUSD != *tc.wantCost) {
			t.Errorf("%s: ok=%v incomplete=%v cost=%v", tc.name, ok, u.UsageIncomplete, u.CostUSD)
		}
	}
	u, _ := aggregateUsage(&store.Mission{CostUSD: &one, InputTokens: 7, Iterations: 1}, &RunOutput{CostUSD: &two, InputTokens: 3, Iterations: 2})
	if *u.CostUSD != 0.75 || *u.InputTokens != 10 || *u.Iterations != 3 {
		t.Errorf("totals not summed across attempts: %+v", u)
	}
}

// Review D/M1: a peer whose clock runs far ahead must not take over a
// mission whose worker is still renewing: expiry is the store's decision.
func TestClockSkewedPeerCannotStealLiveLease(t *testing.T) {
	c := newCluster(t)
	hang := newHangRunner(false)
	a, _ := c.worker("worker-a", hang, 3*time.Second, 0)
	m, err := a.Create(c.ctx, contractWith(func(ct *Contract) { ct.Limits.MaxAttempts = 1 }), "alice")
	must(t, err)
	<-hang.started
	b, _ := c.worker("worker-b", &fakeRunner{}, time.Minute, 2*time.Hour) // B's clock is 2h ahead
	rep, err := b.Recover(context.Background())
	must(t, err)
	if got := c.get(m.ID); rep != (RecoveryReport{}) || got.Status != StatusRunning || got.Attempt != 1 {
		t.Fatalf("skewed peer interfered: %+v %s attempt %d", rep, got.Status, got.Attempt)
	}
	_, _ = a.Cancel(c.ctx, m.ID, "alice", "")
	a.Wait()
}
