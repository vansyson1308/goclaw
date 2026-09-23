package evalsuite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/mission"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// Options configure a suite run.
type Options struct {
	// SourceRoot holds the case sources and hidden overlays (dir/testdata).
	SourceRoot string
	// Executor runs verifier commands (default: host).
	Executor mission.Executor
	// WorkDir holds mission data; a temp dir is used when empty.
	WorkDir string
}

// CaseResult is the outcome of one case.
type CaseResult struct {
	ID         string            `json:"id"`
	Split      string            `json:"split"`
	Category   string            `json:"category"`
	Expected   string            `json:"expected"`
	Actual     string            `json:"actual"`
	Pass       bool              `json:"pass"`
	Skipped    string            `json:"skipped,omitempty"`
	Mismatches []string          `json:"mismatches,omitempty"`
	Criteria   map[string]string `json:"criteria,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	DurationMS int64             `json:"duration_ms"`
}

// Report summarises a suite run.
type Report struct {
	Executor string       `json:"executor"`
	Cases    []CaseResult `json:"cases"`
	Total    int          `json:"total"`
	Passed   int          `json:"passed"`
	Skipped  int          `json:"skipped"`
	// FalseSuccess counts cases the system reported as succeeded/partial
	// although the expected verdict was not: the critical error.
	FalseSuccess int `json:"false_success"`
	// FalseFailure counts expected successes that were not reported as such.
	FalseFailure int                   `json:"false_failure"`
	BySplit      map[string]SplitScore `json:"by_split"`
}

// SplitScore is pass counts for one split.
type SplitScore struct {
	Passed int `json:"passed"`
	Total  int `json:"total"`
}

// OK reports whether every non-skipped case passed.
func (r *Report) OK() bool { return r.Passed == r.Total-r.Skipped && r.FalseSuccess == 0 }

func isSuccess(s string) bool { return s == store.MissionSucceeded || s == store.MissionPartial }

// Run executes cases sequentially and compares verdicts.
func Run(ctx context.Context, cases []Case, opts Options) (*Report, error) {
	if opts.Executor == nil {
		opts.Executor = mission.HostExecutor{}
	}
	src, err := filepath.Abs(opts.SourceRoot)
	if err != nil {
		return nil, err
	}
	work := opts.WorkDir
	if work == "" {
		if work, err = os.MkdirTemp("", "goclaw-evals-"); err != nil {
			return nil, err
		}
		defer os.RemoveAll(work)
	}
	rep := &Report{Executor: opts.Executor.Name(), BySplit: map[string]SplitScore{}}
	for _, c := range cases {
		res := runCase(ctx, c, src, filepath.Join(work, c.ID), opts.Executor)
		rep.Cases = append(rep.Cases, res)
		rep.Total++
		sc := rep.BySplit[c.Split]
		switch {
		case res.Skipped != "":
			rep.Skipped++
		case res.Pass:
			rep.Passed++
			sc.Passed++
			sc.Total++
		default:
			sc.Total++
		}
		rep.BySplit[c.Split] = sc
		if res.Skipped == "" && isSuccess(res.Actual) && !isSuccess(res.Expected) {
			rep.FalseSuccess++
		}
		if res.Skipped == "" && isSuccess(res.Expected) && res.Actual != res.Expected {
			rep.FalseFailure++
		}
	}
	return rep, nil
}

func runCase(ctx context.Context, c Case, sourceRoot, dataDir string, exec mission.Executor) CaseResult {
	res := CaseResult{ID: c.ID, Split: c.Split, Category: c.Category, Expected: c.Expect.Status}
	if slices.Contains(c.Requires, "host") && exec.Name() != "host" {
		res.Skipped = "requires the host executor"
		return res
	}
	start := time.Now()
	defer func() { res.DurationMS = time.Since(start).Milliseconds() }()

	out, err := RunScripted(ctx, c.Contract, c.Agent, sourceRoot, dataDir, exec)
	if err != nil {
		res.Actual = "rejected"
		res.Mismatches = append(res.Mismatches, err.Error())
		return res
	}
	got, receipts := out.Mission, out.Receipts
	res.Actual, res.Reason, res.Criteria = got.Status, got.StatusReason, out.Criteria
	res.Mismatches = append(res.Mismatches, compare(c.Expect, got, res.Criteria, receipts, out.SideEffects)...)
	res.Pass = len(res.Mismatches) == 0
	return res
}

// ScriptedRun is the result of one mission driven by a scripted agent.
type ScriptedRun struct {
	Mission     *store.Mission
	Criteria    map[string]string
	Receipts    []store.MissionReceipt
	SideEffects []string // external tools that actually executed (must stay empty)
}

// RunScripted creates one mission from contract, drives it with script and
// returns the verdict. dataDir must be unique per call.
func RunScripted(ctx context.Context, contract json.RawMessage, script AgentScript, sourceRoot, dataDir string, exec mission.Executor) (*ScriptedRun, error) {
	runner := &replayRunner{script: script}
	st := mission.NewMemoryStore()
	svc, err := mission.NewService(mission.Config{SourceRoot: sourceRoot, DataRoot: dataDir, LeaseTTL: time.Minute}, st, runner, exec)
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	tctx := store.WithTenantID(ctx, uuid.New())
	m, err := svc.Create(tctx, contract, "eval")
	if err != nil {
		return nil, fmt.Errorf("contract rejected: %w", err)
	}
	svc.Wait()
	got, _ := st.GetMission(tctx, m.ID)
	receipts, _ := st.ListMissionReceipts(tctx, m.ID)
	out := &ScriptedRun{Mission: got, Receipts: receipts, SideEffects: runner.sideEffects(), Criteria: map[string]string{}}
	var crs []mission.CriterionResult
	_ = json.Unmarshal(got.Verification, &crs)
	for _, r := range crs {
		out.Criteria[r.ID] = r.Status
	}
	return out, nil
}

func compare(e Expectation, m *store.Mission, criteria map[string]string, receipts []store.MissionReceipt, effects []string) []string {
	var out []string
	if m.Status != e.Status {
		out = append(out, fmt.Sprintf("status %s, want %s (%s)", m.Status, e.Status, m.StatusReason))
	}
	for id, want := range e.Criteria {
		if criteria[id] != want {
			out = append(out, fmt.Sprintf("criterion %s = %q, want %q", id, criteria[id], want))
		}
	}
	for _, tool := range e.Refused {
		for _, r := range receipts {
			if r.Tool == tool && r.Status == store.ReceiptOK {
				out = append(out, fmt.Sprintf("tool %s ran successfully; it must be refused", tool))
			}
		}
		if slices.Contains(effects, tool) {
			out = append(out, fmt.Sprintf("external tool %s performed its side effect", tool))
		}
	}
	if e.ReasonContains != "" && !strings.Contains(m.StatusReason, e.ReasonContains) {
		out = append(out, fmt.Sprintf("reason %q lacks %q", m.StatusReason, e.ReasonContains))
	}
	for _, f := range e.ChangedHas {
		if !slices.Contains(m.ChangedFiles, f) {
			out = append(out, fmt.Sprintf("changed files %v lack %s", m.ChangedFiles, f))
		}
	}
	for _, f := range e.ChangedLacks {
		if slices.Contains(m.ChangedFiles, f) {
			out = append(out, fmt.Sprintf("changed files %v contain %s", m.ChangedFiles, f))
		}
	}
	return out
}

// replayRunner plays a case's scripted tool calls through a real tool
// registry, with the mission's guard and workspace confinement installed,
// exactly as the agent loop would.
type replayRunner struct {
	script  AgentScript
	mu      sync.Mutex
	effects []string
}

// externalTools stand in for tools with effects outside the workspace. If
// one ever executes, the side effect is recorded and the case fails.
var externalTools = []string{"message", "spawn", "delegate", "cron", "team_tasks", "memory_search", "send_file", "web_fetch"}

func (r *replayRunner) sideEffects() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.effects)
}

func (r *replayRunner) RunMission(ctx context.Context, in mission.RunInput) (*mission.RunOutput, error) {
	reg := tools.NewRegistry()
	reg.Register(tools.NewReadFileTool(in.Workspace, true))
	reg.Register(tools.NewWriteFileTool(in.Workspace, true))
	reg.Register(tools.NewEditTool(in.Workspace, true))
	reg.Register(tools.NewListFilesTool(in.Workspace, true))
	reg.Register(tools.NewExecTool(in.Workspace, true))
	for _, name := range externalTools {
		reg.Register(&sideEffectTool{name: name, record: func(n string) {
			r.mu.Lock()
			r.effects = append(r.effects, n)
			r.mu.Unlock()
		}})
	}
	tctx := tools.WithToolWorkspace(ctx, in.Workspace)
	tctx = tools.WithWorkspaceConfined(tctx)
	if in.Guard != nil {
		tctx = tools.WithCallGuard(tctx, in.Guard)
	}
	out := &mission.RunOutput{Content: r.script.Reply, CostUSD: r.script.CostUSD}
	for _, s := range r.script.Steps {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		reg.ExecuteWithContext(tctx, s.Tool, s.Args, "", "", "", in.SessionKey, nil)
		out.Iterations++
	}
	switch r.script.Behavior {
	case "hang":
		<-ctx.Done()
		return nil, ctx.Err()
	case "error":
		return nil, errors.New("scripted agent error: provider returned 500")
	case "loop_killed":
		out.LoopKilled = true
	}
	return out, nil
}

// sideEffectTool records that it ran.
type sideEffectTool struct {
	name   string
	record func(string)
}

func (t *sideEffectTool) Name() string        { return t.name }
func (t *sideEffectTool) Description() string { return "external side effect (evaluation stub)" }
func (t *sideEffectTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *sideEffectTool) Execute(context.Context, map[string]any) *tools.Result {
	t.record(t.name)
	return tools.NewResult("sent")
}
