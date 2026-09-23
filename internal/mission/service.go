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

// Status aliases for readability inside the package.
const (
	StatusPlanned   = store.MissionPlanned
	StatusPreparing = store.MissionPreparing
	StatusRunning   = store.MissionRunning
	StatusVerifying = store.MissionVerifying
	StatusSucceeded = store.MissionSucceeded
	StatusPartial   = store.MissionPartial
	StatusFailed    = store.MissionFailed
	StatusBlocked   = store.MissionBlocked
	StatusCancelled = store.MissionCancelled
)

// ActorSystem is the audit actor for transitions the service makes itself.
const ActorSystem = "system:mission"

// RunInput is what the service asks the agent runtime to execute.
type RunInput struct {
	TenantID      uuid.UUID
	AgentKey      string
	SessionKey    string
	RunID         string
	UserID        string
	Message       string
	Workspace     string
	MaxIterations int
}

// RunOutput is what came back from the agent run. CostUSD is nil when the
// provider/pricing could not attribute a cost (unknown is not zero).
type RunOutput struct {
	Content      string
	InputTokens  int64
	OutputTokens int64
	CostUSD      *float64
	Iterations   int
}

// AgentRunner executes one agent run pinned to a mission workspace.
type AgentRunner interface {
	RunMission(ctx context.Context, in RunInput) (*RunOutput, error)
}

// Config configures the mission service.
type Config struct {
	// SourceRoot is the only directory contract source_dir may resolve under.
	SourceRoot string
	// DataRoot holds per-mission workspaces: <DataRoot>/<tenant>/<mission>/.
	DataRoot string
	// MaxConcurrent bounds simultaneously executing missions (default 1).
	MaxConcurrent int
}

// Service creates, executes, cancels and recovers missions.
type Service struct {
	cfg    Config
	store  store.MissionStore
	runner AgentRunner
	exec   Executor

	sem     chan struct{}
	mu      sync.Mutex
	running map[uuid.UUID]context.CancelFunc
	wg      sync.WaitGroup
}

// NewService builds a Service. exec defaults to HostExecutor.
func NewService(cfg Config, st store.MissionStore, runner AgentRunner, exec Executor) (*Service, error) {
	if st == nil || runner == nil {
		return nil, fmt.Errorf("mission service: store and runner are required")
	}
	if !filepath.IsAbs(cfg.SourceRoot) || !filepath.IsAbs(cfg.DataRoot) {
		return nil, fmt.Errorf("mission service: source and data roots must be absolute")
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if exec == nil {
		exec = HostExecutor{}
	}
	return &Service{
		cfg: cfg, store: st, runner: runner, exec: exec,
		sem:     make(chan struct{}, cfg.MaxConcurrent),
		running: map[uuid.UUID]context.CancelFunc{},
	}, nil
}

// ErrInvalidContract wraps contract validation failures (HTTP 400).
var ErrInvalidContract = errors.New("invalid mission contract")

// Create validates the contract, persists the mission and starts it.
// ctx must carry the tenant; owner is the authenticated creator.
func (s *Service) Create(ctx context.Context, raw []byte, owner string) (*store.Mission, error) {
	c, err := ParseContract(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidContract, err)
	}
	if _, err := s.resolveSource(c.Workspace.SourceDir); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidContract, err)
	}
	if _, err := s.resolveOverlays(c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidContract, err)
	}
	if owner == "" {
		return nil, fmt.Errorf("mission owner required")
	}
	m := &store.Mission{
		OwnerID:        owner,
		AgentKey:       c.Agent,
		Title:          c.Title,
		Contract:       c.Canonical(),
		ContractDigest: c.Digest(),
		Status:         StatusPlanned,
	}
	if err := s.store.CreateMission(ctx, m, owner); err != nil {
		return nil, err
	}
	s.start(ctx, m.ID, c)
	return m, nil
}

// Cancel stops a mission that has not finished.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID, actor, reason string) (*store.Mission, error) {
	if reason == "" {
		reason = "cancelled by " + actor
	}
	now := time.Now().UTC()
	m, err := s.store.TransitionMission(ctx, id, store.MissionActiveStatuses, StatusCancelled, actor, reason,
		store.MissionUpdate{StatusReason: &reason, FinishedAt: &now})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	cancel := s.running[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return m, nil
}

// RecoverInterrupted marks missions left active by a previous process as
// failed ("interrupted"). Durable resume is out of scope for v1.
func (s *Service) RecoverInterrupted(ctx context.Context) (int, error) {
	active, err := s.store.ListActiveMissionsAllTenants(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range active {
		s.mu.Lock()
		_, live := s.running[m.ID]
		s.mu.Unlock()
		if live {
			continue
		}
		reason := "interrupted: gateway restarted while the mission was " + m.Status
		now := time.Now().UTC()
		tctx := store.WithTenantID(ctx, m.TenantID)
		if _, err := s.store.TransitionMission(tctx, m.ID, store.MissionActiveStatuses, StatusFailed, ActorSystem, reason,
			store.MissionUpdate{StatusReason: &reason, FinishedAt: &now}); err == nil {
			n++
		}
	}
	return n, nil
}

// Wait blocks until all started missions have finished (tests, shutdown).
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) start(reqCtx context.Context, id uuid.UUID, c *Contract) {
	// Detach from the HTTP request; keep tenant, slug and locale.
	base := context.WithoutCancel(reqCtx)
	runCtx, cancel := context.WithTimeout(base, time.Duration(c.Limits.TimeoutSeconds)*time.Second+10*time.Minute)
	s.mu.Lock()
	s.running[id] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.running, id)
			s.mu.Unlock()
			cancel()
		}()
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-runCtx.Done():
			// Timed out while queued (or cancelled: then the CAS below is a no-op).
			s.finish(base, id, StatusPlanned, StatusFailed, "timed out waiting for a free mission slot", store.MissionUpdate{})
			return
		}
		s.execute(runCtx, id, c)
	}()
}

// execute drives one mission through its lifecycle. Every transition is a
// compare-and-set; a conflict (e.g. user cancelled) stops execution quietly.
func (s *Service) execute(ctx context.Context, id uuid.UUID, c *Contract) {
	log := slog.With("mission", id)
	// Bookkeeping uses a context that outlives run timeouts so the mission
	// can always reach a terminal state. A user cancel is detected by the
	// compare-and-set transitions failing, not by context errors.
	bctx := context.WithoutCancel(ctx)
	m, err := s.store.GetMission(bctx, id)
	if err != nil || m == nil {
		log.Warn("mission.execute: load failed", "error", err)
		return
	}
	started := time.Now().UTC()
	execName := s.exec.Name()
	if m, err = s.transition(bctx, id, StatusPlanned, StatusPreparing, "preparing workspace",
		store.MissionUpdate{StartedAt: &started, Executor: &execName}); err != nil {
		return
	}

	src, err := s.resolveSource(c.Workspace.SourceDir)
	missionDir := filepath.Join(s.cfg.DataRoot, m.TenantID.String(), id.String())
	var pw *PreparedWorkspace
	if err == nil {
		pw, err = PrepareWorkspace(ctx, src, filepath.Join(missionDir, "workspace"))
	}
	if err != nil {
		s.finish(bctx, id, StatusPreparing, StatusBlocked, "workspace preparation failed: "+err.Error(), store.MissionUpdate{})
		return
	}
	if len(pw.Skipped) > 0 {
		s.note(bctx, id, fmt.Sprintf("skipped %d non-regular entries (symlinks/special files)", len(pw.Skipped)), pw.Skipped)
	}
	overlays, err := s.resolveOverlays(c)
	if err != nil {
		s.finish(bctx, id, StatusPreparing, StatusBlocked, "acceptance overlay unavailable: "+err.Error(), store.MissionUpdate{})
		return
	}
	env := VerifyEnv{Workspace: pw.Path, Scratch: filepath.Join(missionDir, "scratch"), Overlays: overlays, Exec: s.exec}
	// Checks that must prove the change are run on the untouched baseline first.
	baseline := Baseline(ctx, c, env)
	if len(baseline) > 0 {
		s.note(bctx, id, fmt.Sprintf("baseline evaluated for %d must_change check(s)", len(baseline)), baselineSummary(baseline))
	}
	if _, err = s.transition(bctx, id, StatusPreparing, StatusRunning, "agent run started",
		store.MissionUpdate{WorkspacePath: &pw.Path, BaseRevision: &pw.BaseRevision}); err != nil {
		return
	}

	runCtx, cancelRun := context.WithTimeout(ctx, time.Duration(c.Limits.TimeoutSeconds)*time.Second)
	out, runErr := s.runner.RunMission(runCtx, RunInput{
		TenantID:      m.TenantID,
		AgentKey:      c.Agent,
		SessionKey:    fmt.Sprintf("agent:%s:mission:%s", c.Agent, id),
		RunID:         "mission:" + id.String(),
		UserID:        m.OwnerID,
		Message:       BuildPrompt(c),
		Workspace:     pw.Path,
		MaxIterations: c.Limits.MaxIterations,
	})
	cancelRun()
	usage := store.MissionUpdate{}
	var runReason string
	if out != nil {
		usage.Summary = &out.Content
		usage.InputTokens, usage.OutputTokens = &out.InputTokens, &out.OutputTokens
		usage.CostUSD, usage.Iterations = out.CostUSD, &out.Iterations
		if c.Limits.MaxCostUSD > 0 && out.CostUSD != nil && *out.CostUSD > c.Limits.MaxCostUSD {
			runReason = fmt.Sprintf("cost limit exceeded: $%.4f > $%.4f", *out.CostUSD, c.Limits.MaxCostUSD)
		}
	}
	if runErr != nil {
		runReason = "agent run failed: " + runErr.Error()
	}
	if _, err = s.transition(bctx, id, StatusRunning, StatusVerifying, "verifying acceptance criteria", usage); err != nil {
		return
	}

	// Evidence is collected even when the run failed, so users see real state.
	diff, diffErr := Diff(bctx, pw.Path, pw.BaseRevision)
	var changed []string
	upd := store.MissionUpdate{}
	if diffErr == nil {
		changed = diff.ChangedFiles
		upd.Diff, upd.DiffTruncated, upd.ChangedFiles = &diff.Patch, &diff.Truncated, orEmpty(diff.ChangedFiles)
	}
	env.Changed = changed
	results := Verify(ctx, c, env, baseline)
	if diffErr != nil {
		results = append(results, CriterionResult{ID: "_diff", Kind: "internal", Status: ResultError,
			Detail: "could not compute diff: " + diffErr.Error(), Executor: s.exec.Name(), ContractDigest: c.Digest()})
	}
	verification, _ := json.Marshal(results)
	upd.Verification = verification

	final := Outcome(results)
	reason := summarizeOutcome(results)
	if runReason != "" {
		// A failed or over-budget run cannot be reported as success even if
		// the checks happen to pass.
		if final == StatusSucceeded || final == StatusPartial {
			final = StatusFailed
		}
		reason = runReason + "; " + reason
	}
	s.finish(bctx, id, StatusVerifying, final, reason, upd)
}

func (s *Service) transition(ctx context.Context, id uuid.UUID, from, to, msg string, u store.MissionUpdate) (*store.Mission, error) {
	m, err := s.store.TransitionMission(ctx, id, []string{from}, to, ActorSystem, msg, u)
	if err != nil && !errors.Is(err, store.ErrMissionStateConflict) {
		slog.Warn("mission.transition failed", "mission", id, "from", from, "to", to, "error", err)
	}
	return m, err
}

func (s *Service) finish(ctx context.Context, id uuid.UUID, from, to, reason string, u store.MissionUpdate) {
	now := time.Now().UTC()
	u.FinishedAt, u.StatusReason = &now, &reason
	// Persist the terminal state even if the run context expired.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, _ = s.transition(fctx, id, from, to, reason, u)
}

func (s *Service) note(ctx context.Context, id uuid.UUID, msg string, detail any) {
	d, _ := json.Marshal(detail)
	_ = s.store.AppendMissionEvent(ctx, store.MissionEvent{MissionID: id, Kind: "note", Actor: ActorSystem, Message: msg, Detail: d})
}

// resolveSource maps a contract source_dir to an existing directory under
// SourceRoot, rejecting anything that resolves outside it (incl. symlinks).
func (s *Service) resolveSource(rel string) (string, error) {
	if err := validateRelPath(rel, "workspace.source_dir"); err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(s.cfg.SourceRoot)
	if err != nil {
		return "", fmt.Errorf("mission source root unavailable")
	}
	p, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", fmt.Errorf("source %q not found under the mission source root", rel)
	}
	if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", fmt.Errorf("source %q resolves outside the mission source root", rel)
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("source %q is not a directory", rel)
	}
	return p, nil
}

// resolveOverlays maps command criteria overlay dirs to absolute paths
// under the source root.
func (s *Service) resolveOverlays(c *Contract) (map[string]string, error) {
	out := map[string]string{}
	for _, cr := range c.Acceptance {
		if cr.OverlayDir == "" {
			continue
		}
		p, err := s.resolveSource(cr.OverlayDir)
		if err != nil {
			return nil, fmt.Errorf("criterion %q overlay: %w", cr.ID, err)
		}
		out[cr.ID] = p
	}
	return out, nil
}

func baselineSummary(b map[string]CriterionResult) map[string]string {
	out := map[string]string{}
	for id, r := range b {
		out[id] = r.Status
	}
	return out
}

func summarizeOutcome(results []CriterionResult) string {
	var pass, fail, errs int
	for _, r := range results {
		switch r.Status {
		case ResultPass:
			pass++
		case ResultFail:
			fail++
		default:
			errs++
		}
	}
	return fmt.Sprintf("%d/%d criteria passed, %d failed, %d could not be evaluated", pass, len(results), fail, errs)
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// BuildPrompt renders the contract as the agent's task. The acceptance
// commands are shown so the agent can check its own work, and it is told
// they will be re-run independently.
func BuildPrompt(c *Contract) string {
	var b strings.Builder
	fmt.Fprintf(&b, "MISSION: %s\n\nObjective:\n%s\n", c.Title, c.Objective)
	if len(c.Constraints) > 0 {
		b.WriteString("\nConstraints:\n")
		for _, x := range c.Constraints {
			fmt.Fprintf(&b, "- %s\n", x)
		}
	}
	if len(c.NonGoals) > 0 {
		b.WriteString("\nOut of scope:\n")
		for _, x := range c.NonGoals {
			fmt.Fprintf(&b, "- %s\n", x)
		}
	}
	b.WriteString("\nAcceptance criteria (re-checked independently after you finish; your own claim of success is not accepted as evidence):\n")
	for _, cr := range c.Acceptance {
		desc := cr.Description
		if desc == "" {
			desc = cr.ID
		}
		switch cr.Kind {
		case KindCommand:
			if cr.OverlayDir != "" {
				fmt.Fprintf(&b, "- [%s] %s — hidden acceptance tests are added, then must exit 0: `%s`\n", cr.ID, desc, strings.Join(cr.Command, " "))
			} else {
				fmt.Fprintf(&b, "- [%s] %s — must exit 0: `%s`\n", cr.ID, desc, strings.Join(cr.Command, " "))
			}
		case KindFileChanged:
			fmt.Fprintf(&b, "- [%s] %s — at least one changed file matching %q\n", cr.ID, desc, cr.Glob)
		case KindFileContains:
			fmt.Fprintf(&b, "- [%s] %s — file %s must contain %q\n", cr.ID, desc, cr.Path, cr.Text)
		}
	}
	b.WriteString("\nWork only inside your current workspace directory using relative paths. When finished, reply with a short summary of what you changed and which checks you ran.\n")
	return b.String()
}
