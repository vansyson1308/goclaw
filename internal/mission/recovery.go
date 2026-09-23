package mission

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// RecoveryReport counts what one recovery pass did.
type RecoveryReport struct {
	Resumed int // planned missions nobody was running, started here
	Retried int // attempts whose worker died, requeued for another attempt
	Failed  int // attempts whose worker died with no attempts left
}

// Recover finds missions that no live worker owns and makes progress on
// them. Lease expiry is judged by the store's clock under the row lock, so
// a mission leased by a worker that is still renewing is never taken over,
// whatever the local clocks say. Running several gateways on one database
// is therefore safe:
//   - planned and unleased (queued in a process that died, or requeued):
//     started in this process;
//   - preparing/running/verifying with an expired or missing lease (the
//     worker crashed or was partitioned): requeued for a fresh attempt in a
//     new workspace, or failed when max_attempts is used up.
func (s *Service) Recover(ctx context.Context) (RecoveryReport, error) {
	var rep RecoveryReport
	active, err := s.store.ListActiveMissionsAllTenants(ctx)
	if err != nil {
		return rep, err
	}
	for _, m := range active {
		if s.isLive(m.ID) {
			continue
		}
		tctx := store.WithTenantID(ctx, m.TenantID)
		if m.Status == StatusPlanned {
			if s.resume(tctx, &m) {
				rep.Resumed++
			}
			continue
		}
		fence := store.MissionFence{Owner: m.LeaseOwner, Attempt: m.Attempt}
		// Usage of an attempt that died before verifying was never recorded.
		incomplete := m.UsageIncomplete || m.Status != StatusVerifying
		lost := fmt.Sprintf("attempt %d/%d was interrupted while %s (worker %s stopped renewing its lease)",
			m.Attempt, m.MaxAttempts, m.Status, orUnknown(m.LeaseOwner))
		if m.Attempt < m.MaxAttempts {
			msg := lost + "; retrying in a fresh workspace"
			upd, err := s.store.TransitionMission(tctx, m.ID, []string{m.Status}, StatusPlanned, ActorSystem, msg,
				store.MissionUpdate{Fence: &fence, ClearLease: true, RequireLeaseExpired: true, UsageIncomplete: &incomplete, StatusReason: &msg})
			if err != nil {
				continue // lease still live, someone else recovered it, or it moved on
			}
			rep.Retried++
			s.resume(tctx, upd)
			continue
		}
		reason := "interrupted: " + lost + "; no attempts left"
		finished := time.Now().UTC()
		if _, err := s.store.TransitionMission(tctx, m.ID, []string{m.Status}, StatusFailed, ActorSystem, reason,
			store.MissionUpdate{Fence: &fence, ClearLease: true, RequireLeaseExpired: true, UsageIncomplete: &incomplete, StatusReason: &reason, FinishedAt: &finished}); err == nil {
			rep.Failed++
		}
	}
	return rep, nil
}

// resume starts a planned mission in this process from its stored contract.
func (s *Service) resume(ctx context.Context, m *store.Mission) bool {
	c, err := ParseContract(m.Contract)
	if err != nil {
		s.finishUnleased(ctx, m.ID, StatusPlanned, StatusBlocked, "stored contract is no longer valid: "+err.Error(), store.MissionUpdate{})
		return false
	}
	return s.start(ctx, m.ID, c)
}

// RunRecovery runs Recover now and then every interval until ctx ends.
func (s *Service) RunRecovery(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = s.cfg.LeaseTTL / 2
	}
	pass := func() {
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		rep, err := s.Recover(rctx)
		switch {
		case err != nil && !errors.Is(err, context.Canceled):
			slog.Warn("missions.recover_failed", "error", err)
		case rep != RecoveryReport{}:
			slog.Warn("missions.recovered", "resumed", rep.Resumed, "retried", rep.Retried, "failed", rep.Failed)
		}
	}
	pass()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
