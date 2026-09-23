package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	httpapi "github.com/nextlevelbuilder/goclaw/internal/http"
	"github.com/nextlevelbuilder/goclaw/internal/mission"
	"github.com/nextlevelbuilder/goclaw/internal/pipeline"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/sandbox"
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
	envMissionsExecutor   = "GOCLAW_MISSIONS_EXECUTOR" // "docker" (default) or "host"
	envMissionsImage      = "GOCLAW_MISSIONS_IMAGE"    // image for verifiers and the agent's exec; must be pulled
	envMissionsUser       = "GOCLAW_MISSIONS_SANDBOX_USER"
)

const defaultMissionImage = "golang:1.26-bookworm"

// minLeaseSeconds bounds GOCLAW_MISSIONS_LEASE_SECONDS from below.
const minLeaseSeconds = 3

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
	if leaseSec != 0 && leaseSec < minLeaseSeconds {
		// Shorter leases turn a brief database hiccup into a takeover.
		slog.Warn("missions: lease too short, using the minimum", "requested", leaseSec, "min", minLeaseSeconds)
		leaseSec = minLeaseSeconds
	}
	runner := &schedulerMissionRunner{sched: sched, tenants: d.pgStores.Tenants}
	executor, err := missionExecutor(runner)
	if err != nil {
		return nil, err
	}
	mcfg := mission.Config{SourceRoot: sourceRoot, DataRoot: dataRoot, MaxConcurrent: maxConc,
		LeaseTTL: time.Duration(leaseSec) * time.Second}
	if runner.sandbox != nil {
		mcfg.OnAttemptLost = removeAttemptContainers
	}
	svc, err := mission.NewService(mcfg,
		d.pgStores.Missions, runner, executor)
	if err != nil {
		return nil, err
	}
	hardenProcessForMissions()
	if executor.Name() == "host" {
		slog.Warn("security.missions_host_executor",
			"detail", "verifier commands and the agent's exec run on the gateway host; use the docker executor for anything untrusted",
			"source_root", sourceRoot, "data_root", dataRoot)
	} else {
		slog.Info("missions enabled (verifiers and agent exec run in containers)", "image", runner.sandboxCfg.Image,
			"source_root", sourceRoot, "data_root", dataRoot)
	}
	return svc, nil
}

// missionExecutor selects where verifier commands and the agent's exec run.
// "docker" (default) fails closed: missions stay disabled if Docker or the
// image is unavailable, rather than silently running on the host.
func missionExecutor(runner *schedulerMissionRunner) (mission.Executor, error) {
	switch mode := os.Getenv(envMissionsExecutor); mode {
	case "host":
		return mission.HostExecutor{}, nil
	case "", "docker":
	default:
		return nil, fmt.Errorf("%s must be docker or host, got %q", envMissionsExecutor, mode)
	}
	image := os.Getenv(envMissionsImage)
	if image == "" {
		image = defaultMissionImage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sandbox.CheckDockerAvailable(ctx); err != nil {
		return nil, fmt.Errorf("docker executor: %w (set %s=host to run missions on the host)", err, envMissionsExecutor)
	}
	if err := mission.CheckSandboxImage(ctx, image); err != nil {
		return nil, err
	}
	user := os.Getenv(envMissionsUser)
	if user == "" {
		user = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	cfg := missionSandboxConfig(image, user)
	runner.sandboxCfg = &cfg
	runner.sandbox = sandbox.NewDockerManager(cfg)
	return mission.DockerExecutor{Image: image, User: user, HostPath: sandbox.HostPath}, nil
}

// attemptSandboxConfig labels the attempt's container so it can be found
// and removed if this process dies before releasing it.
func (r *schedulerMissionRunner) attemptSandboxConfig(in mission.RunInput) *sandbox.Config {
	if r.sandboxCfg == nil {
		return nil
	}
	cfg := *r.sandboxCfg
	cfg.Labels = map[string]string{"goclaw.mission": in.MissionID.String(), "goclaw.attempt": strconv.Itoa(in.Attempt)}
	return &cfg
}

// removeAttemptContainers removes containers a dead worker left behind for
// a mission attempt (their processes would otherwise keep running).
func removeAttemptContainers(ctx context.Context, missionID uuid.UUID, attempt int) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "ps", "-aq",
		"--filter", "label=goclaw.mission="+missionID.String(),
		"--filter", "label=goclaw.attempt="+strconv.Itoa(attempt)).Output()
	if err != nil {
		slog.Warn("mission container lookup failed", "mission", missionID, "attempt", attempt, "error", err)
		return
	}
	for _, id := range strings.Fields(string(out)) {
		if err := exec.CommandContext(cctx, "docker", "rm", "-f", id).Run(); err != nil {
			slog.Warn("mission container removal failed", "mission", missionID, "container", id, "error", err)
		} else {
			slog.Info("removed container of a lost mission attempt", "mission", missionID, "attempt", attempt, "container", id)
		}
	}
}

// missionSandboxConfig is the container a mission attempt's tools run in:
// no network, read-only root, no capabilities, only the attempt workspace
// mounted; /tmp allows running built binaries (go test).
func missionSandboxConfig(image, user string) sandbox.Config {
	cfg := sandbox.DefaultConfig()
	cfg.Mode = sandbox.ModeAll
	cfg.Image = image
	cfg.User = user
	cfg.Scope = sandbox.ScopeSession
	cfg.WorkspaceAccess = sandbox.AccessRW
	cfg.NetworkEnabled = false
	cfg.ReadOnlyRoot = true
	cfg.CapDrop = []string{"ALL"}
	cfg.Tmpfs = []string{"/var/tmp", "/run"}
	cfg.TmpfsExec = []string{"/tmp:size=1024m"}
	cfg.MemoryMB, cfg.CPUs, cfg.PidsLimit = 2048, 2, 512
	cfg.TimeoutSec = 600
	cfg.ContainerPrefix = "goclaw-mission-"
	cfg.Env = map[string]string{
		"HOME": "/tmp/home", "TMPDIR": "/tmp", "GOCACHE": "/tmp/gocache", "GOPATH": "/tmp/gopath",
		"GOFLAGS": "-mod=mod", "GOPROXY": "off", "GOTOOLCHAIN": "local", "LANG": "C.UTF-8",
	}
	return cfg
}

// schedulerMissionRunner runs a mission's agent turn through the scheduler,
// the same path cron jobs use, pinned to the mission workspace.
type schedulerMissionRunner struct {
	sched   *scheduler.Scheduler
	tenants store.TenantStore
	// sandbox/sandboxCfg: the container each attempt's tools run in (docker
	// executor). nil = host executor.
	sandbox    sandbox.Manager
	sandboxCfg *sandbox.Config
}

func (r *schedulerMissionRunner) RunMission(ctx context.Context, in mission.RunInput) (*mission.RunOutput, error) {
	ctx = cronTenantContext(ctx, r.tenants, in.TenantID)
	if r.sandbox != nil {
		// Destroying the attempt's container ends everything it started.
		defer func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := r.sandbox.Release(rctx, in.SessionKey); err != nil {
				slog.Warn("mission sandbox release failed", "run", in.RunID, "error", err)
			}
		}()
	}
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
		SandboxManager:   r.sandbox,
		SandboxConfig:    r.attemptSandboxConfig(in),
		TokenBudget:      in.TokenBudget,
		MaxIterations:    in.MaxIterations,
		TraceName:        "Mission " + in.RunID,
		TraceTags:        []string{"mission"},
	})
	var outcome scheduler.RunOutcome
	select {
	case outcome = <-outCh:
	case <-ctx.Done():
		// Wait (bounded) for the run to actually stop, so none of its tool or
		// model calls overlaps the verification that follows.
		select {
		case <-outCh:
		case <-time.After(runStopGrace):
			slog.Warn("mission run did not stop within the grace period", "run", in.RunID)
		}
		return nil, ctx.Err()
	}
	if outcome.Err != nil {
		return nil, outcome.Err
	}
	res := outcome.Result
	out := &mission.RunOutput{Content: res.Content, Iterations: res.Iterations, LoopKilled: res.LoopKilled}
	if res.Usage != nil {
		// Include cached input (reported separately by some providers).
		out.InputTokens, out.OutputTokens = int64(pipeline.InputContextTokens(*res.Usage)), int64(res.Usage.CompletionTokens)
	}
	out.CostUSD = missionRunCost(res.Calls)
	return out, nil
}

// runStopGrace bounds how long a cancelled mission run may take to stop.
const runStopGrace = 30 * time.Second

// missionRunCost sums per-call cost only when every call was priced.
// Unpriced calls report 0, so a single one makes the total unknown (nil)
// rather than an undercount that could pass a cost limit.
func missionRunCost(calls []providers.CallUsage) *float64 {
	if len(calls) == 0 {
		return nil
	}
	var total float64
	for _, c := range calls {
		if c.CostUSD <= 0 {
			return nil
		}
		total += c.CostUSD
	}
	return &total
}
