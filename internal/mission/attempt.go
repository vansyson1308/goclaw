package mission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// attempt is one claimed execution of a mission by this worker. Every
// write it makes is fenced by (worker, attempt): once another worker takes
// over or the mission is cancelled, its writes are refused.
type attempt struct {
	s     *Service
	id    uuid.UUID
	fence store.MissionFence
	dir   string // <DataRoot>/<tenant>/<mission>/attempt-<n>
	log   *slog.Logger
}

// transientRetry is how long fenced writes are retried on store errors
// (not conflicts) before the attempt gives up and leaves recovery to retry.
var transientRetry = []time.Duration{0, 200 * time.Millisecond, time.Second, 3 * time.Second}

func (a *attempt) transition(ctx context.Context, from, to, msg string, u store.MissionUpdate) (*store.Mission, error) {
	u.Fence = &a.fence
	var m *store.Mission
	var err error
	for _, d := range transientRetry {
		if d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		m, err = a.s.store.TransitionMission(ctx, a.id, []string{from}, to, ActorSystem, msg, u)
		if err == nil || errors.Is(err, store.ErrMissionStateConflict) || errors.Is(err, store.ErrMissionNotFound) {
			return m, err
		}
		a.log.Warn("mission.transition failed; retrying", "from", from, "to", to, "error", err)
	}
	return m, err
}

// finish moves the attempt to a terminal status and releases the lease.
func (a *attempt) finish(ctx context.Context, from, to, reason string, u store.MissionUpdate) {
	now := time.Now().UTC()
	reason = cleanText(reason)
	u.FinishedAt, u.StatusReason, u.ClearLease = &now, &reason, true
	// Persist the terminal state even if the run context expired.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, err := a.transition(fctx, from, to, reason, u)
	if err == nil || errors.Is(err, store.ErrMissionStateConflict) || errors.Is(err, store.ErrMissionNotFound) {
		return
	}
	// The evidence itself could not be stored (e.g. rejected content): do
	// not leave the mission active forever. Record that it could not be
	// judged, without the payload. If the store is down entirely, the lease
	// expires and recovery takes over.
	msg := cleanText("could not persist verification evidence: " + err.Error())
	_, _ = a.transition(fctx, from, StatusBlocked, msg, store.MissionUpdate{StatusReason: &msg, FinishedAt: &now, ClearLease: true})
}

// execute claims the mission (next attempt) and runs it to a terminal
// status. A conflict at any fenced step (cancel, lost lease) stops quietly.
func (s *Service) execute(ctx context.Context, id uuid.UUID, c *Contract) {
	log := slog.With("mission", id, "worker", s.cfg.WorkerID)
	// Bookkeeping uses a context that outlives run timeouts so the mission
	// can always reach a terminal state.
	bctx := context.WithoutCancel(ctx)
	m, err := s.store.GetMission(bctx, id)
	if err != nil || m == nil {
		log.Warn("mission.execute: load failed", "error", err)
		return
	}
	execName := s.exec.Name()
	upd := store.MissionUpdate{Executor: &execName, Claim: &store.MissionClaim{Owner: s.cfg.WorkerID, Until: s.cfg.Now().Add(s.cfg.LeaseTTL)}}
	if m.StartedAt == nil {
		now := time.Now().UTC()
		upd.StartedAt = &now
	}
	msg := fmt.Sprintf("attempt %d/%d claimed by %s; preparing workspace", m.Attempt+1, m.MaxAttempts, s.cfg.WorkerID)
	claimed, err := s.store.TransitionMission(bctx, id, []string{StatusPlanned}, StatusPreparing, ActorSystem, msg, upd)
	switch {
	case errors.Is(err, store.ErrMissionNoAttempts):
		incomplete := m.Attempt > 0
		s.finishUnleased(bctx, id, StatusPlanned, StatusFailed,
			fmt.Sprintf("no attempts left (%d/%d)", m.Attempt, m.MaxAttempts), store.MissionUpdate{UsageIncomplete: &incomplete})
		return
	case err != nil:
		// Cancelled, or another worker claimed it first.
		if !errors.Is(err, store.ErrMissionStateConflict) {
			log.Warn("mission.claim failed", "error", err)
		}
		return
	}
	a := &attempt{
		s: s, id: id,
		fence: store.MissionFence{Owner: s.cfg.WorkerID, Attempt: claimed.Attempt},
		dir:   filepath.Join(s.cfg.DataRoot, claimed.TenantID.String(), id.String(), fmt.Sprintf("attempt-%d", claimed.Attempt)),
		log:   log.With("attempt", claimed.Attempt),
	}
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := s.heartbeat(bctx, id, a.fence, cancel)
	defer stop()
	a.run(actx, bctx, claimed, c)
}

// heartbeat renews the lease every TTL/3. It stops the attempt when the
// lease is lost (cancelled elsewhere, or another worker took over after a
// partition) or cannot be renewed for a whole TTL (store unreachable), so
// a worker never keeps acting on a mission it may no longer own.
func (s *Service) heartbeat(ctx context.Context, id uuid.UUID, fence store.MissionFence, cancel context.CancelCauseFunc) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(s.cfg.LeaseTTL / 3)
		defer t.Stop()
		lastOK := s.cfg.Now()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			err := s.store.RenewMissionLease(ctx, id, fence, s.cfg.Now().Add(s.cfg.LeaseTTL))
			switch {
			case err == nil:
				lastOK = s.cfg.Now()
			case errors.Is(err, store.ErrMissionLeaseLost):
				cancel(errLeaseLost)
				return
			case s.cfg.Now().Sub(lastOK) >= s.cfg.LeaseTTL:
				slog.Warn("mission.lease_unrenewable", "mission", id, "error", err)
				cancel(errLeaseUnrenewable)
				return
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// stopped reports whether the attempt lost ownership; its remaining work
// must not be recorded.
func stopped(ctx context.Context) error {
	if cause := context.Cause(ctx); errors.Is(cause, errLeaseLost) || errors.Is(cause, errLeaseUnrenewable) || errors.Is(cause, errCancelled) {
		return cause
	}
	return nil
}

// run prepares the attempt's own workspace, runs the agent, verifies and
// records the outcome. ctx carries the attempt's cancellation; bctx is for
// bookkeeping writes.
func (a *attempt) run(ctx, bctx context.Context, m *store.Mission, c *Contract) {
	s, id := a.s, a.id
	var pins Pins
	if err := json.Unmarshal(m.Pins, &pins); err != nil || pins.Source == "" {
		a.finish(bctx, StatusPreparing, StatusBlocked, "mission inputs were not pinned at creation", store.MissionUpdate{})
		return
	}
	// Each attempt starts from a fresh copy of the pinned source, so work a
	// crashed attempt left behind can never leak into the evidence.
	src, err := s.resolveSource(c.Workspace.SourceDir)
	var pw *PreparedWorkspace
	if err == nil {
		pw, err = PrepareWorkspace(ctx, src, filepath.Join(a.dir, "workspace"), filepath.Join(a.dir, "base.git"))
	}
	if err != nil {
		a.finish(bctx, StatusPreparing, StatusBlocked, "workspace preparation failed: "+err.Error(), store.MissionUpdate{})
		return
	}
	if pw.SourceDigest != pins.Source {
		a.finish(bctx, StatusPreparing, StatusBlocked,
			fmt.Sprintf("source %q changed since the mission was created (%s, pinned %s)", c.Workspace.SourceDir, pw.SourceDigest, pins.Source), store.MissionUpdate{})
		return
	}
	if len(pw.Skipped) > 0 {
		s.note(bctx, id, fmt.Sprintf("skipped %d non-regular entries (symlinks/special files)", len(pw.Skipped)), pw.Skipped)
	}
	overlays, err := s.resolveOverlays(c)
	if err != nil {
		a.finish(bctx, StatusPreparing, StatusBlocked, "acceptance overlay unavailable: "+err.Error(), store.MissionUpdate{})
		return
	}
	scratch := filepath.Join(a.dir, "scratch")
	// The scratch dir (caches, throwaway check copies) is not evidence.
	defer os.RemoveAll(scratch)
	env := VerifyEnv{Workspace: pw.Path, Scratch: scratch, Overlays: overlays, OverlayPins: pins.Overlays, Exec: s.exec}
	// Checks that must prove the change are run on the untouched baseline first.
	baseline := Baseline(ctx, c, env)
	if len(baseline) > 0 {
		s.note(bctx, id, fmt.Sprintf("baseline evaluated for %d must_change check(s)", len(baseline)), baselineSummary(baseline))
	}
	if _, err = a.transition(bctx, StatusPreparing, StatusRunning, "agent run started",
		store.MissionUpdate{WorkspacePath: &pw.Path, BaseRevision: &pw.BaseRevision}); err != nil {
		return
	}

	runCtx, cancelRun := context.WithTimeout(ctx, time.Duration(c.Limits.TimeoutSeconds)*time.Second)
	out, runErr := s.runner.RunMission(runCtx, RunInput{
		TenantID:      m.TenantID,
		AgentKey:      c.Agent,
		SessionKey:    fmt.Sprintf("agent:%s:mission:%s:a%d", c.Agent, id, m.Attempt),
		RunID:         fmt.Sprintf("mission:%s:a%d", id, m.Attempt),
		UserID:        m.OwnerID,
		Message:       BuildPrompt(c),
		Workspace:     pw.Path,
		MaxIterations: c.Limits.MaxIterations,
		Guard:         newToolGuard(bctx, s.store, id, a.fence, c),
		TokenBudget:   c.Limits.MaxTokens,
	})
	cancelRun()
	if cause := stopped(ctx); cause != nil {
		// Cancelled, or another worker now owns the mission: nothing this
		// attempt observed may be recorded.
		a.log.Info("mission attempt stopped", "cause", cause)
		return
	}
	usage, costOK := aggregateUsage(m, out)
	var runReason string
	if out != nil {
		if c.Limits.MaxCostUSD > 0 && out.CostUSD != nil && *out.CostUSD > c.Limits.MaxCostUSD {
			runReason = fmt.Sprintf("cost limit exceeded: $%.4f > $%.4f", *out.CostUSD, c.Limits.MaxCostUSD)
		}
		if out.LoopKilled {
			runReason = "agent run stopped by the loop detector"
		}
	}
	if runErr != nil {
		runReason = "agent run failed: " + runErr.Error()
	}
	if _, err = a.transition(bctx, StatusRunning, StatusVerifying, "verifying acceptance criteria", usage); err != nil {
		return
	}

	// Evidence is collected even when the run failed, so users see real state.
	diff, diffErr := Diff(bctx, pw)
	upd := store.MissionUpdate{}
	if diffErr == nil {
		env.Changed = diff.Present
		upd.Diff, upd.DiffTruncated, upd.ChangedFiles = &diff.Patch, &diff.Truncated, orEmpty(diff.ChangedFiles)
	}
	results := Verify(ctx, c, env, baseline)
	if cause := stopped(ctx); cause != nil {
		a.log.Info("mission attempt stopped during verification", "cause", cause)
		return
	}
	if diffErr != nil {
		results = append(results, CriterionResult{ID: "_diff", Kind: "internal", Status: ResultError,
			Detail: "could not compute diff: " + cleanText(diffErr.Error()), Executor: s.exec.Name(), ContractDigest: c.Digest()})
	} else if findings := IntegrityFindings(diff, diff.Full); len(findings) > 0 {
		// Verifier commands execute agent-written code; changes that can
		// subvert them need a human to confirm the checks are meaningful.
		results = append(results, CriterionResult{ID: "_integrity", Kind: "internal", Status: ResultError,
			Detail: "needs human review: " + strings.Join(findings, "; "), Executor: s.exec.Name(), ContractDigest: c.Digest()})
	}
	verification, _ := json.Marshal(results)
	upd.Verification = verification

	final := Outcome(results)
	reason := summarizeOutcome(results)
	if c.Limits.MaxCostUSD > 0 && !costOK && (final == StatusSucceeded || final == StatusPartial) {
		// Unknown is not zero: the limit cannot be shown to hold.
		final = StatusBlocked
		reason = "cost limit is set but the mission's cost is unknown, so the limit cannot be verified; " + reason
	}
	if runReason != "" {
		// A failed or over-budget run cannot be reported as success even if
		// the checks happen to pass.
		if final == StatusSucceeded || final == StatusPartial {
			final = StatusFailed
		}
		reason = runReason + "; " + reason
	}
	if m.Attempt > 1 {
		reason = fmt.Sprintf("attempt %d/%d: %s", m.Attempt, m.MaxAttempts, reason)
	}
	a.finish(bctx, StatusVerifying, final, reason, upd)
}

// aggregateUsage adds this attempt's usage to the mission totals. costOK is
// false when the total cost is not fully known: this attempt reported no
// cost, or an earlier attempt's usage was lost or unpriced. Unknown cost is
// never summed as zero; usage_incomplete marks totals as a lower bound.
func aggregateUsage(m *store.Mission, out *RunOutput) (store.MissionUpdate, bool) {
	u := store.MissionUpdate{}
	if out == nil {
		incomplete := true
		u.UsageIncomplete = &incomplete
		return u, false
	}
	summary := cleanText(out.Content)
	in, outTok, iters := m.InputTokens+out.InputTokens, m.OutputTokens+out.OutputTokens, m.Iterations+out.Iterations
	u.Summary, u.InputTokens, u.OutputTokens, u.Iterations = &summary, &in, &outTok, &iters
	priorUsage := m.InputTokens+m.OutputTokens > 0 || m.Iterations > 0
	incomplete := m.UsageIncomplete
	costOK := !incomplete && out.CostUSD != nil
	switch {
	case out.CostUSD != nil && m.CostUSD != nil:
		total := *m.CostUSD + *out.CostUSD
		u.CostUSD = &total
	case out.CostUSD != nil:
		u.CostUSD = out.CostUSD
		if priorUsage { // an earlier attempt ran unpriced
			incomplete, costOK = true, false
		}
	case m.CostUSD != nil: // this attempt is unpriced; keep the known part
		incomplete, costOK = true, false
	}
	u.UsageIncomplete = &incomplete
	return u, costOK
}
