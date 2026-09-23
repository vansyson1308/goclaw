package mission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
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
	MissionID     uuid.UUID
	Attempt       int
	TenantID      uuid.UUID
	AgentKey      string
	SessionKey    string
	RunID         string
	UserID        string
	Message       string
	Workspace     string
	MaxIterations int
	// Guard must be installed on every tool call of the run (allowlist +
	// write-ahead receipts).
	Guard tools.CallGuard
	// TokenBudget caps prompt+completion tokens of the run (0 = none).
	TokenBudget int64
}

// RunOutput is what came back from the agent run. CostUSD is nil when the
// provider/pricing could not attribute a cost (unknown is not zero).
type RunOutput struct {
	Content      string
	InputTokens  int64
	OutputTokens int64
	CostUSD      *float64
	Iterations   int
	// LoopKilled is set when the loop detector stopped the run; such a run
	// cannot count as successful.
	LoopKilled bool
}

// AgentRunner executes one agent run pinned to a mission workspace.
type AgentRunner interface {
	RunMission(ctx context.Context, in RunInput) (*RunOutput, error)
}

// Config configures the mission service.
type Config struct {
	// SourceRoot is the only directory contract source_dir may resolve under.
	SourceRoot string
	// DataRoot holds per-mission workspaces: <DataRoot>/<tenant>/<mission>/attempt-<n>/.
	DataRoot string
	// MaxConcurrent bounds simultaneously executing missions (default 1).
	MaxConcurrent int
	// WorkerID identifies this process in leases (default host:pid:random).
	WorkerID string
	// LeaseTTL is how long a claim survives without a heartbeat (default
	// 60s). The attempt of a worker that died is retried once it expires.
	LeaseTTL time.Duration
	// Now is the clock used for leases (tests).
	Now func() time.Time
	// OnAttemptLost, if set, is called when recovery takes over an attempt
	// whose worker died, to clean up what it left running (containers).
	OnAttemptLost func(ctx context.Context, missionID uuid.UUID, attempt int)
}

// Service creates, executes, cancels and recovers missions. Several
// services (gateway processes) may share one store: a mission is executed
// by whichever worker holds its lease, and every write a worker makes is
// fenced by (worker, attempt).
type Service struct {
	cfg    Config
	store  store.MissionStore
	runner AgentRunner
	exec   Executor

	sem     chan struct{}
	mu      sync.Mutex
	running map[uuid.UUID]context.CancelCauseFunc
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
	if cfg.WorkerID == "" {
		cfg.WorkerID = defaultWorkerID()
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 60 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if exec == nil {
		exec = HostExecutor{}
	}
	return &Service{
		cfg: cfg, store: st, runner: runner, exec: exec,
		sem:     make(chan struct{}, cfg.MaxConcurrent),
		running: map[uuid.UUID]context.CancelCauseFunc{},
	}, nil
}

func defaultWorkerID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), uuid.NewString()[:8])
}

// ErrInvalidContract wraps contract validation failures (HTTP 400).
var ErrInvalidContract = errors.New("invalid mission contract")

// Causes recorded on an attempt's context when it is stopped.
var (
	errCancelled        = errors.New("mission cancelled")
	errLeaseLost        = errors.New("mission lease lost (cancelled or claimed by another worker)")
	errLeaseUnrenewable = errors.New("mission lease could not be renewed")
)

// Create validates the contract, persists the mission and starts it.
// ctx must carry the tenant; owner is the authenticated creator.
func (s *Service) Create(ctx context.Context, raw []byte, owner string) (*store.Mission, error) {
	c, err := ParseContract(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidContract, err)
	}
	if owner == "" {
		return nil, fmt.Errorf("mission owner required")
	}
	pins, err := s.pinInputs(c)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidContract, err)
	}
	pinsJSON, _ := json.Marshal(pins)
	m := &store.Mission{
		OwnerID:        owner,
		AgentKey:       c.Agent,
		Title:          c.Title,
		Contract:       c.Canonical(),
		ContractDigest: c.Digest(),
		Status:         StatusPlanned,
		MaxAttempts:    c.Limits.MaxAttempts,
		Pins:           pinsJSON,
	}
	if err := s.store.CreateMission(ctx, m, owner); err != nil {
		return nil, err
	}
	s.start(ctx, m.ID, c)
	return m, nil
}

// Cancel stops a mission that has not finished. A worker in another
// process notices on its next heartbeat and stops its run.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID, actor, reason string) (*store.Mission, error) {
	if reason == "" {
		reason = "cancelled by " + actor
	}
	now := time.Now().UTC()
	m, err := s.store.TransitionMission(ctx, id, store.MissionActiveStatuses, StatusCancelled, actor, reason,
		store.MissionUpdate{StatusReason: &reason, FinishedAt: &now, ClearLease: true})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	cancel := s.running[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel(errCancelled)
	}
	return m, nil
}

// Wait blocks until all started missions have finished (tests, shutdown).
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) isLive(id uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[id]
	return ok
}

// start queues a mission for execution in this process. It is a no-op if
// the mission is already queued or running here.
func (s *Service) start(reqCtx context.Context, id uuid.UUID, c *Contract) bool {
	// Detach from the HTTP request; keep tenant, slug and locale.
	base := context.WithoutCancel(reqCtx)
	bounded, cancelTimeout := context.WithTimeout(base, time.Duration(c.Limits.TimeoutSeconds)*time.Second+10*time.Minute)
	runCtx, cancel := context.WithCancelCause(bounded)
	s.mu.Lock()
	if _, dup := s.running[id]; dup {
		s.mu.Unlock()
		cancel(nil)
		cancelTimeout()
		return false
	}
	s.running[id] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.running, id)
			s.mu.Unlock()
			cancel(nil)
			cancelTimeout()
		}()
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-runCtx.Done():
			// Timed out while queued (a cancel makes the CAS below a no-op).
			s.finishUnleased(base, id, StatusPlanned, StatusFailed, "timed out waiting for a free mission slot", store.MissionUpdate{})
			return
		}
		s.execute(runCtx, id, c)
	}()
	return true
}

// finishUnleased ends a mission that no worker holds (queued/planned).
func (s *Service) finishUnleased(ctx context.Context, id uuid.UUID, from, to, reason string, u store.MissionUpdate) {
	now := time.Now().UTC()
	reason = cleanText(reason)
	u.FinishedAt, u.StatusReason, u.ClearLease = &now, &reason, true
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, err := s.store.TransitionMission(fctx, id, []string{from}, to, ActorSystem, reason, u); err != nil &&
		!errors.Is(err, store.ErrMissionStateConflict) {
		slog.Warn("mission.finish failed", "mission", id, "to", to, "error", err)
	}
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
	if path.Clean(rel) == "." {
		return "", fmt.Errorf("source %q must name a directory inside the mission source root, not the root itself", rel)
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

// Pins are digests of the mission inputs taken at creation. The workspace
// copy and each hidden overlay are checked against them before use, so a
// change to the inputs (or tampering during the run) cannot go unnoticed.
type Pins struct {
	Source   string            `json:"source"`
	Overlays map[string]string `json:"overlays,omitempty"`
}

func (s *Service) pinInputs(c *Contract) (*Pins, error) {
	src, err := s.resolveSource(c.Workspace.SourceDir)
	if err != nil {
		return nil, err
	}
	overlays, err := s.resolveOverlays(c)
	if err != nil {
		return nil, err
	}
	p := &Pins{Overlays: map[string]string{}}
	if p.Source, err = TreeDigest(src); err != nil {
		return nil, fmt.Errorf("read source %q: %w", c.Workspace.SourceDir, err)
	}
	for id, dir := range overlays {
		if p.Overlays[id], err = TreeDigest(dir); err != nil {
			return nil, fmt.Errorf("read overlay for %q: %w", id, err)
		}
	}
	return p, nil
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
	fmt.Fprintf(&b, "\nTools available in this mission: %s. Other tools are refused.\n", strings.Join(c.AllowedTools(), ", "))
	b.WriteString("\nWork only inside your current workspace directory using relative paths. When finished, reply with a short summary of what you changed and which checks you ran.\n")
	return b.String()
}
