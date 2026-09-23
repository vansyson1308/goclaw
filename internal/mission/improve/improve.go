// Package improve is the evidence-driven improvement lifecycle for agent
// candidates: a candidate (a new agent version) is evaluated on a fixed
// benchmark of mission tasks, promoted only when the evidence says it is
// better without regressing anything, and rolled back automatically when
// later evaluation shows the promoted version doing worse than the one it
// replaced. Every decision is recorded with the evidence it was based on.
//
// Candidates here are scripted agent behaviours (offline, deterministic),
// which exercises the lifecycle itself. Whether a new prompt or model
// actually improves needs live runs against a provider and is not claimed.
package improve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/mission"
	"github.com/nextlevelbuilder/goclaw/internal/mission/evalsuite"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Task is one benchmark task: a mission contract, independent of any agent.
type Task struct {
	ID       string          `json:"id"`
	Split    string          `json:"split"` // dev | heldout
	Contract json.RawMessage `json:"contract"`
}

// Candidate is one agent version: how it behaves on each task. A task with
// no script is attempted with no action (and fails).
type Candidate struct {
	ID          string                           `json:"id"`
	Parent      string                           `json:"parent,omitempty"`
	Description string                           `json:"description"`
	Scripts     map[string]evalsuite.AgentScript `json:"scripts"`
}

// TaskResult is a candidate's verdict on one task.
type TaskResult struct {
	Task   string `json:"task"`
	Split  string `json:"split"`
	Status string `json:"status"`
	// Violation: the mission was blocked for review by an integrity finding
	// or the candidate tried a refused tool — gaming or unsafe behaviour.
	Violation string `json:"violation,omitempty"`
}

// Score is a candidate's evaluation.
type Score struct {
	Candidate   string         `json:"candidate"`
	Results     []TaskResult   `json:"results"`
	Solved      map[string]int `json:"solved"` // split -> succeeded
	Total       map[string]int `json:"total"`
	Violations  int            `json:"violations"`
	Executor    string         `json:"executor"`
	Digest      string         `json:"digest"` // sha256 of the results, cited in the ledger
	EvaluatedAt time.Time      `json:"evaluated_at"`
}

func (s *Score) solvedSet() map[string]bool {
	out := map[string]bool{}
	for _, r := range s.Results {
		if r.Status == store.MissionSucceeded && r.Violation == "" {
			out[r.Task] = true
		}
	}
	return out
}

// SolvedTotal is the number of tasks solved across splits.
func (s *Score) SolvedTotal() int { return len(s.solvedSet()) }

// Benchmark is a fixed set of tasks plus the sources they run against.
type Benchmark struct {
	Dir        string
	Tasks      []Task
	Candidates map[string]*Candidate
}

// LoadBenchmark reads dir/tasks/*.json, dir/candidates/*.json; sources are
// in dir/testdata.
func LoadBenchmark(dir string) (*Benchmark, error) {
	b := &Benchmark{Dir: dir, Candidates: map[string]*Candidate{}}
	if err := loadJSONDir(filepath.Join(dir, "tasks"), func(raw []byte, name string) error {
		t, err := parseTask(raw)
		if err != nil {
			return err
		}
		b.Tasks = append(b.Tasks, t)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := loadJSONDir(filepath.Join(dir, "candidates"), func(raw []byte, name string) error {
		var c Candidate
		if err := strictJSON(raw, &c); err != nil {
			return err
		}
		if c.ID == "" {
			return fmt.Errorf("candidate id required")
		}
		b.Candidates[c.ID] = &c
		return nil
	}); err != nil {
		return nil, err
	}
	return b, nil
}

func parseTask(raw []byte) (Task, error) {
	var t Task
	if err := strictJSON(raw, &t); err != nil {
		return t, err
	}
	if t.ID == "" || (t.Split != evalsuite.SplitDev && t.Split != evalsuite.SplitHeldOut) || len(t.Contract) == 0 || string(t.Contract) == "null" {
		return t, fmt.Errorf("task needs id, split dev|heldout and a contract")
	}
	return t, nil
}

func loadJSONDir(dir string, fn func([]byte, string) error) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if err := fn(raw, f); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func strictJSON(raw []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Evaluate runs a candidate on the tasks (optionally one split only).
func Evaluate(ctx context.Context, b *Benchmark, candidateID string, splits []string, exec mission.Executor) (*Score, error) {
	c, ok := b.Candidates[candidateID]
	if !ok {
		return nil, fmt.Errorf("unknown candidate %q", candidateID)
	}
	if exec == nil {
		exec = mission.HostExecutor{}
	}
	work, err := os.MkdirTemp("", "goclaw-improve-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	src, err := filepath.Abs(filepath.Join(b.Dir, "testdata"))
	if err != nil {
		return nil, err
	}
	s := &Score{Candidate: c.ID, Solved: map[string]int{}, Total: map[string]int{}, Executor: exec.Name(), EvaluatedAt: time.Now().UTC()}
	for _, t := range b.Tasks {
		if len(splits) > 0 && !slices.Contains(splits, t.Split) {
			continue
		}
		run, err := evalsuite.RunScripted(ctx, t.Contract, c.Scripts[t.ID], src, filepath.Join(work, t.ID), exec)
		if err != nil {
			return nil, fmt.Errorf("task %s: %w", t.ID, err)
		}
		r := TaskResult{Task: t.ID, Split: t.Split, Status: run.Mission.Status, Violation: violation(run)}
		s.Results = append(s.Results, r)
		s.Total[t.Split]++
		if r.Status == store.MissionSucceeded && r.Violation == "" {
			s.Solved[t.Split]++
		}
		if r.Violation != "" {
			s.Violations++
		}
	}
	sum, _ := json.Marshal(s.Results)
	h := sha256.Sum256(sum)
	s.Digest = "sha256:" + hex.EncodeToString(h[:])
	return s, nil
}

func violation(run *evalsuite.ScriptedRun) string {
	if run.Criteria["_integrity"] == mission.ResultError {
		return "integrity finding (tried to subvert the verifiers)"
	}
	if len(run.SideEffects) > 0 {
		return "external side effect: " + strings.Join(run.SideEffects, ",")
	}
	for _, r := range run.Receipts {
		if r.Status == store.ReceiptDenied {
			return "attempted a refused tool: " + r.Tool
		}
	}
	return ""
}

// Decision is the promotion gate's verdict with its reasons.
type Decision struct {
	Promote bool     `json:"promote"`
	Reasons []string `json:"reasons"`
}

// Gate decides whether candidate may replace champion. All must hold:
//  1. no violations (gaming the verifiers, refused tools, external effects);
//  2. no regression: every task the champion solves, the candidate solves;
//  3. held-out solved count not lower than the champion's;
//  4. strictly more tasks solved overall (an actual improvement).
//
// With no champion, rules 2–4 compare against zero.
func Gate(champion, candidate *Score) Decision {
	var d Decision
	ok := true
	if candidate.Violations > 0 {
		ok = false
		for _, r := range candidate.Results {
			if r.Violation != "" {
				d.Reasons = append(d.Reasons, fmt.Sprintf("violation on %s: %s", r.Task, r.Violation))
			}
		}
	}
	champSolved := map[string]bool{}
	champHeld, champTotal := 0, 0
	if champion != nil {
		champSolved = champion.solvedSet()
		champHeld, champTotal = champion.Solved[evalsuite.SplitHeldOut], champion.SolvedTotal()
	}
	candSolved := candidate.solvedSet()
	var regressed []string
	for t := range champSolved {
		if !candSolved[t] {
			regressed = append(regressed, t)
		}
	}
	sort.Strings(regressed)
	if len(regressed) > 0 {
		ok = false
		d.Reasons = append(d.Reasons, "regresses tasks the champion solves: "+strings.Join(regressed, ", "))
	}
	if held := candidate.Solved[evalsuite.SplitHeldOut]; held < champHeld {
		ok = false
		d.Reasons = append(d.Reasons, fmt.Sprintf("held-out solved %d < champion %d (overfits dev)", held, champHeld))
	}
	if total := candidate.SolvedTotal(); total <= champTotal {
		ok = false
		d.Reasons = append(d.Reasons, fmt.Sprintf("no improvement: solves %d, champion %d", total, champTotal))
	}
	if ok {
		d.Reasons = append(d.Reasons, fmt.Sprintf("solves %d (champion %d), held-out %d (champion %d), no regressions, no violations",
			candidate.SolvedTotal(), champTotal, candidate.Solved[evalsuite.SplitHeldOut], champHeld))
	}
	d.Promote = ok
	return d
}

// AddTasks appends the tasks in dir (e.g. incidents discovered after a
// promotion) to the benchmark.
func (b *Benchmark) AddTasks(dir string) error {
	return loadJSONDir(dir, func(raw []byte, name string) error {
		t, err := parseTask(raw)
		if err != nil {
			return err
		}
		for _, x := range b.Tasks {
			if x.ID == t.ID {
				return fmt.Errorf("duplicate task %q", t.ID)
			}
		}
		b.Tasks = append(b.Tasks, t)
		return nil
	})
}
