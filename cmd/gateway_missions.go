package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	httpapi "github.com/nextlevelbuilder/goclaw/internal/http"
	"github.com/nextlevelbuilder/goclaw/internal/mission"
	"github.com/nextlevelbuilder/goclaw/internal/scheduler"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Missions env configuration. Missions run verifier commands on the host
// (see docs/mission-control/MISSIONS.md), so they are opt-in.
const (
	envMissionsEnabled    = "GOCLAW_MISSIONS"               // "1" enables create/cancel
	envMissionsSourceRoot = "GOCLAW_MISSIONS_SOURCE_ROOT"   // default <data>/mission-sources
	envMissionsLease      = "GOCLAW_MISSIONS_LEASE_SECONDS" // default 60; a dead worker's attempt is retried after this
	envMissionsMax        = "GOCLAW_MISSIONS_MAX_CONCURRENT"
)

// wireMissions registers the missions API. Reads always work; the service
// (create/cancel/execute) is only built when GOCLAW_MISSIONS=1.
func (d *gatewayDeps) wireMissions(ctx context.Context, sched *scheduler.Scheduler) {
	if d.pgStores == nil || d.pgStores.Missions == nil {
		return
	}
	var svc httpapi.MissionService
	if os.Getenv(envMissionsEnabled) == "1" {
		s, err := d.buildMissionService(sched)
		if err != nil {
			slog.Error("missions disabled: configuration error", "error", err)
		} else {
			svc = s
			// Resume queued missions and retry attempts whose worker died
			// (this process before a restart, or another gateway).
			go s.RunRecovery(ctx, 0)
		}
	}
	d.server.SetMissionsHandler(httpapi.NewMissionsHandler(d.pgStores.Missions, svc))
}

func (d *gatewayDeps) buildMissionService(sched *scheduler.Scheduler) (*mission.Service, error) {
	if d.dataDir == "" {
		return nil, fmt.Errorf("data dir not configured")
	}
	sourceRoot := os.Getenv(envMissionsSourceRoot)
	if sourceRoot == "" {
		sourceRoot = filepath.Join(d.dataDir, "mission-sources")
	}
	sourceRoot, err := filepath.Abs(sourceRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create mission source root: %w", err)
	}
	dataRoot, err := filepath.Abs(filepath.Join(d.dataDir, "missions"))
	if err != nil {
		return nil, err
	}
	maxConc, _ := strconv.Atoi(os.Getenv(envMissionsMax))
	leaseSec, _ := strconv.Atoi(os.Getenv(envMissionsLease))
	runner := &schedulerMissionRunner{sched: sched, tenants: d.pgStores.Tenants}
	svc, err := mission.NewService(mission.Config{SourceRoot: sourceRoot, DataRoot: dataRoot, MaxConcurrent: maxConc,
		LeaseTTL: time.Duration(leaseSec) * time.Second},
		d.pgStores.Missions, runner, mission.HostExecutor{})
	if err != nil {
		return nil, err
	}
	hardenProcessForMissions()
	slog.Warn("missions enabled (verifiers run on the host executor)", "source_root", sourceRoot, "data_root", dataRoot)
	return svc, nil
}

// schedulerMissionRunner runs a mission's agent turn through the scheduler,
// the same path cron jobs use, pinned to the mission workspace.
type schedulerMissionRunner struct {
	sched   *scheduler.Scheduler
	tenants store.TenantStore
}

func (r *schedulerMissionRunner) RunMission(ctx context.Context, in mission.RunInput) (*mission.RunOutput, error) {
	ctx = cronTenantContext(ctx, r.tenants, in.TenantID)
	outCh := r.sched.Schedule(ctx, scheduler.LaneCron, agent.RunRequest{
		SessionKey:       in.SessionKey,
		Message:          in.Message,
		Channel:          "mission",
		ChatID:           in.RunID,
		PeerKind:         "direct",
		UserID:           in.UserID,
		RunID:            in.RunID,
		MissionWorkspace: in.Workspace,
		ToolGuard:        in.Guard,
		TokenBudget:      in.TokenBudget,
		MaxIterations:    in.MaxIterations,
		TraceName:        "Mission " + in.RunID,
		TraceTags:        []string{"mission"},
	})
	var outcome scheduler.RunOutcome
	select {
	case outcome = <-outCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if outcome.Err != nil {
		return nil, outcome.Err
	}
	res := outcome.Result
	out := &mission.RunOutput{Content: res.Content, Iterations: res.Iterations, LoopKilled: res.LoopKilled}
	if res.Usage != nil {
		out.InputTokens, out.OutputTokens = int64(res.Usage.PromptTokens), int64(res.Usage.CompletionTokens)
	}
	// Unpriced calls report 0; a zero total is recorded as unknown, not free.
	var cost float64
	for _, c := range res.Calls {
		cost += c.CostUSD
	}
	if cost > 0 {
		out.CostUSD = &cost
	}
	return out, nil
}
