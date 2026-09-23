package mission

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// fakeRunner simulates the agent by applying edits to the mission workspace.
type fakeRunner struct {
	edit    func(ws string) error
	reply   string
	err     error
	cost    *float64
	block   chan struct{} // if set, wait until closed or ctx done
	lastIn  RunInput
	mu      sync.Mutex
	started chan struct{}
}

func (f *fakeRunner) RunMission(ctx context.Context, in RunInput) (*RunOutput, error) {
	f.mu.Lock()
	f.lastIn = in
	f.mu.Unlock()
	if f.started != nil {
		close(f.started)
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.edit != nil {
		if err := f.edit(in.Workspace); err != nil {
			return nil, err
		}
	}
	return &RunOutput{Content: f.reply, InputTokens: 100, OutputTokens: 20, CostUSD: f.cost, Iterations: 2}, f.err
}

// seedRepo writes a tiny Go module with a bug: Sum ignores negatives.
func seedRepo(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "sumrepo")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/sum\n\ngo 1.22\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "sum.go"), []byte(buggySum), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "sum_test.go"), []byte(baseTest), 0o644))
	// Hidden acceptance test, kept outside the agent's source copy.
	acc := filepath.Join(root, "sumrepo-acceptance")
	must(t, os.MkdirAll(acc, 0o755))
	must(t, os.WriteFile(filepath.Join(acc, "zz_acceptance_test.go"), []byte(acceptanceTest), 0o644))
}

const acceptanceTest = `package sum

import "testing"

func TestAcceptanceSumIncludesNegatives(t *testing.T) {
	cases := []struct {
		in   []int
		want int
	}{{[]int{5, -2}, 3}, {[]int{-1, -1}, -2}, {nil, 0}}
	for _, c := range cases {
		if got := Sum(c.in); got != c.want {
			t.Fatalf("Sum(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
`

const buggySum = `package sum

// Sum returns the sum of xs.
func Sum(xs []int) int {
	total := 0
	for _, x := range xs {
		if x > 0 {
			total += x
		}
	}
	return total
}
`

const fixedSum = `package sum

// Sum returns the sum of xs.
func Sum(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}
`

const baseTest = `package sum

import "testing"

func TestSumPositive(t *testing.T) {
	if got := Sum([]int{1, 2, 3}); got != 6 {
		t.Fatalf("got %d", got)
	}
}
`

const regressionTest = `package sum

import "testing"

func TestSumNegative(t *testing.T) {
	if got := Sum([]int{5, -2}); got != 3 {
		t.Fatalf("got %d, want 3", got)
	}
}
`

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func contractJSON(extra ...Criterion) []byte {
	crits := []Criterion{
		{ID: "tests", Kind: KindCommand, Command: []string{"go", "test", "./..."}, TimeoutSeconds: 120},
		{ID: "behavior", Kind: KindCommand, Command: []string{"go", "test", "-run", "TestAcceptance", "./..."}, TimeoutSeconds: 120, MustChange: true, OverlayDir: "sumrepo-acceptance"},
		{ID: "regression-test", Kind: KindFileChanged, Glob: "*_test.go"},
	}
	crits = append(crits, extra...)
	b, _ := json.Marshal(Contract{
		Version: 1, Title: "Fix Sum", Objective: "Sum must include negative numbers.",
		Agent: "coder", Workspace: Workspace{SourceDir: "sumrepo"}, Acceptance: crits,
		Limits: Limits{TimeoutSeconds: 120},
	})
	return b
}

func newTestService(t *testing.T, r AgentRunner) (*Service, *memStore, context.Context) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	src := filepath.Join(root, "sources")
	seedRepo(t, src)
	st := newMemStore()
	svc, err := NewService(Config{SourceRoot: src, DataRoot: filepath.Join(root, "data")}, st, r, HostExecutor{})
	must(t, err)
	return svc, st, store.WithTenantID(context.Background(), uuid.New())
}

func runToEnd(t *testing.T, svc *Service, st *memStore, ctx context.Context, raw []byte) *store.Mission {
	t.Helper()
	m, err := svc.Create(ctx, raw, "alice")
	must(t, err)
	svc.Wait()
	got, _ := st.GetMission(ctx, m.ID)
	return got
}

func results(t *testing.T, m *store.Mission) map[string]CriterionResult {
	t.Helper()
	var rs []CriterionResult
	must(t, json.Unmarshal(m.Verification, &rs))
	out := map[string]CriterionResult{}
	for _, r := range rs {
		out[r.ID] = r
	}
	return out
}

func TestMissionSucceedsOnlyWithVerifiedFixAndTest(t *testing.T) {
	r := &fakeRunner{reply: "Fixed Sum and added a regression test.", edit: func(ws string) error {
		if err := os.WriteFile(filepath.Join(ws, "sum.go"), []byte(fixedSum), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(ws, "sum_negative_test.go"), []byte(regressionTest), 0o644)
	}}
	svc, st, ctx := newTestService(t, r)
	m := runToEnd(t, svc, st, ctx, contractJSON())
	if m.Status != StatusSucceeded {
		t.Fatalf("status %s (%s), want succeeded", m.Status, m.StatusReason)
	}
	res := results(t, m)
	for _, id := range []string{"tests", "behavior", "regression-test"} {
		if res[id].Status != ResultPass {
			t.Errorf("%s = %s (%s)", id, res[id].Status, res[id].OutputTail)
		}
	}
	if !strings.Contains(m.Diff, "-		if x > 0 {") || !slices.Contains(m.ChangedFiles, "sum_negative_test.go") {
		t.Errorf("diff/changed files missing the fix: %v\n%s", m.ChangedFiles, m.Diff)
	}
	if res["tests"].ContractDigest == "" || res["tests"].Executor != "host" {
		t.Errorf("evidence lacks digest/executor: %+v", res["tests"])
	}
	// The agent was pinned to the prepared workspace, never the source.
	if r.lastIn.Workspace != m.WorkspacePath || !strings.HasSuffix(r.lastIn.Workspace, "/workspace") {
		t.Errorf("runner workspace %q vs mission %q", r.lastIn.Workspace, m.WorkspacePath)
	}
	src, _ := os.ReadFile(filepath.Join(svc.cfg.SourceRoot, "sumrepo", "sum.go"))
	if string(src) != buggySum {
		t.Error("source repository was modified; missions must work on a copy")
	}
}

// The core promise: a narrative "done" without the change is not success.
func TestMissionRejectsFalseCompletion(t *testing.T) {
	r := &fakeRunner{reply: "All done! The bug is fixed and all tests pass."}
	svc, st, ctx := newTestService(t, r)
	m := runToEnd(t, svc, st, ctx, contractJSON())
	if m.Status == StatusSucceeded || m.Status == StatusPartial {
		t.Fatalf("false completion accepted: status %s", m.Status)
	}
	res := results(t, m)
	if res["behavior"].Status != ResultFail || res["behavior"].BaselineStatus != ResultFail || res["regression-test"].Status != ResultFail {
		t.Fatalf("expected failing checks, got %+v", res)
	}
	if m.Summary != r.reply {
		t.Errorf("agent narrative should be kept as summary (not evidence)")
	}
}

func TestMissionPartialWhenOnlySomeCriteriaPass(t *testing.T) {
	r := &fakeRunner{edit: func(ws string) error {
		return os.WriteFile(filepath.Join(ws, "sum.go"), []byte(fixedSum), 0o644) // fix, but no test file
	}}
	svc, st, ctx := newTestService(t, r)
	m := runToEnd(t, svc, st, ctx, contractJSON())
	if m.Status != StatusPartial {
		t.Fatalf("status %s, want partial", m.Status)
	}
}

// A check that cannot run is "error" → blocked, never pass.
func TestMissionBlockedWhenVerifierCannotRun(t *testing.T) {
	r := &fakeRunner{edit: func(ws string) error {
		must(t, os.WriteFile(filepath.Join(ws, "sum.go"), []byte(fixedSum), 0o644))
		return os.WriteFile(filepath.Join(ws, "sum_negative_test.go"), []byte(regressionTest), 0o644)
	}}
	svc, st, ctx := newTestService(t, r)
	m := runToEnd(t, svc, st, ctx, contractJSON(Criterion{ID: "lint", Kind: KindCommand, Command: []string{"definitely-not-a-binary-xyz"}}))
	if m.Status != StatusBlocked {
		t.Fatalf("status %s, want blocked", m.Status)
	}
	if results(t, m)["lint"].Status != ResultError {
		t.Fatal("missing binary must be recorded as error")
	}
}

func TestMissionTimeoutIsErrorNotPass(t *testing.T) {
	r := &fakeRunner{}
	svc, st, ctx := newTestService(t, r)
	m := runToEnd(t, svc, st, ctx, contractJSON(Criterion{ID: "slow", Kind: KindCommand, Command: []string{"sleep", "5"}, TimeoutSeconds: 1}))
	if got := results(t, m)["slow"]; got.Status != ResultError || !strings.Contains(got.Detail, "timed out") {
		t.Fatalf("timeout result: %+v", got)
	}
	if m.Status != StatusBlocked {
		t.Fatalf("status %s, want blocked", m.Status)
	}
}

func TestMissionRunErrorCannotSucceed(t *testing.T) {
	r := &fakeRunner{err: errors.New("provider unavailable"), edit: func(ws string) error {
		must(t, os.WriteFile(filepath.Join(ws, "sum.go"), []byte(fixedSum), 0o644))
		return os.WriteFile(filepath.Join(ws, "sum_negative_test.go"), []byte(regressionTest), 0o644)
	}}
	svc, st, ctx := newTestService(t, r)
	m := runToEnd(t, svc, st, ctx, contractJSON())
	if m.Status != StatusFailed || !strings.Contains(m.StatusReason, "provider unavailable") {
		t.Fatalf("status %s (%s), want failed with run error", m.Status, m.StatusReason)
	}
}

func TestMissionCostOverLimitCannotSucceed(t *testing.T) {
	cost := 2.5
	r := &fakeRunner{cost: &cost, edit: func(ws string) error {
		must(t, os.WriteFile(filepath.Join(ws, "sum.go"), []byte(fixedSum), 0o644))
		return os.WriteFile(filepath.Join(ws, "sum_negative_test.go"), []byte(regressionTest), 0o644)
	}}
	svc, st, ctx := newTestService(t, r)
	var c Contract
	must(t, json.Unmarshal(contractJSON(), &c))
	c.Limits.MaxCostUSD = 1
	raw, _ := json.Marshal(c)
	m := runToEnd(t, svc, st, ctx, raw)
	if m.Status != StatusFailed || !strings.Contains(m.StatusReason, "cost limit exceeded") {
		t.Fatalf("status %s (%s)", m.Status, m.StatusReason)
	}
}

func TestMissionCancelStopsRunAndStaysCancelled(t *testing.T) {
	r := &fakeRunner{block: make(chan struct{}), started: make(chan struct{})}
	svc, st, ctx := newTestService(t, r)
	m, err := svc.Create(ctx, contractJSON(), "alice")
	must(t, err)
	<-r.started
	if _, err := svc.Cancel(ctx, m.ID, "bob", ""); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	got, _ := st.GetMission(ctx, m.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("status %s, want cancelled", got.Status)
	}
	if _, err := svc.Cancel(ctx, m.ID, "bob", ""); !errors.Is(err, store.ErrMissionStateConflict) {
		t.Fatalf("second cancel: want conflict, got %v", err)
	}
}

func TestMissionRejectsSourceOutsideRoot(t *testing.T) {
	svc, _, ctx := newTestService(t, &fakeRunner{})
	outside := t.TempDir()
	must(t, os.Symlink(outside, filepath.Join(svc.cfg.SourceRoot, "escape")))
	for _, dir := range []string{"../etc", "/etc", "escape", "missing"} {
		var c Contract
		must(t, json.Unmarshal(contractJSON(), &c))
		c.Workspace.SourceDir = dir
		raw, _ := json.Marshal(c)
		if _, err := svc.Create(ctx, raw, "alice"); !errors.Is(err, ErrInvalidContract) {
			t.Errorf("source %q: want ErrInvalidContract, got %v", dir, err)
		}
	}
}

func TestRecoverInterruptedMarksStaleMissionsFailed(t *testing.T) {
	svc, st, ctx := newTestService(t, &fakeRunner{})
	stale := &store.Mission{Title: "t", Status: StatusRunning}
	must(t, st.CreateMission(ctx, stale, "alice"))
	n, err := svc.RecoverInterrupted(context.Background())
	must(t, err)
	got, _ := st.GetMission(ctx, stale.ID)
	if n != 1 || got.Status != StatusFailed || !strings.Contains(got.StatusReason, "interrupted") {
		t.Fatalf("n=%d status=%s reason=%q", n, got.Status, got.StatusReason)
	}
}

func TestParseContractRejectsBadInput(t *testing.T) {
	bad := []string{
		`{"version":2}`,
		`{"version":1,"title":"t","objective":"o","agent":"a","workspace":{"source_dir":"x"},"acceptance":[]}`,
		`{"version":1,"title":"t","objective":"o","agent":"a","workspace":{"source_dir":"x"},"acceptance":[{"id":"a","kind":"command"}]}`,
		`{"version":1,"title":"t","objective":"o","agent":"a","workspace":{"source_dir":"x"},"acceptance":[{"id":"a","kind":"nope"}]}`,
		`{"version":1,"title":"t","objective":"o","agent":"a","workspace":{"source_dir":"x"},"acceptance":[{"id":"a","kind":"file_changed","glob":"*"},{"id":"a","kind":"file_changed","glob":"*"}]}`,
		`{"version":1,"title":"t","objective":"o","agent":"a","workspace":{"source_dir":"x"},"acceptance":[{"id":"a","kind":"file_contains","path":"../x","text":"y"}]}`,
		`{"version":1,"title":"t","objective":"o","agent":"a","workspace":{"source_dir":"x"},"acceptance":[{"id":"a","kind":"file_changed","glob":"*","typo_field":1}]}`,
	}
	for i, b := range bad {
		if _, err := ParseContract([]byte(b)); err == nil {
			t.Errorf("case %d accepted: %s", i, b)
		}
	}
	c, err := ParseContract(contractJSON())
	must(t, err)
	if c.Limits.MaxIterations != defaultMaxIteration || c.Digest() == "" {
		t.Fatalf("defaults/digest: %+v", c.Limits)
	}
	c2, _ := ParseContract(contractJSON())
	if c.Digest() != c2.Digest() {
		t.Fatal("digest must be deterministic")
	}
}

func TestVerifyFileContainsRefusesSymlinkEscape(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("TOKEN"), 0o600))
	must(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(ws, "link")))
	c := &Contract{Acceptance: []Criterion{{ID: "x", Kind: KindFileContains, Path: "link", Text: "TOKEN"}}}
	res := Verify(context.Background(), c, VerifyEnv{Workspace: ws, Scratch: t.TempDir(), Exec: HostExecutor{}}, nil)
	if res[0].Status != ResultError {
		t.Fatalf("symlink escape must be an error, got %+v", res[0])
	}
}

// A must_change check that already passes on the untouched code proves
// nothing; the mission is blocked with a contract diagnosis.
func TestMissionMustChangeCheckPassingOnBaselineIsRejected(t *testing.T) {
	r := &fakeRunner{edit: func(ws string) error {
		must(t, os.WriteFile(filepath.Join(ws, "sum.go"), []byte(fixedSum), 0o644))
		return os.WriteFile(filepath.Join(ws, "sum_negative_test.go"), []byte(regressionTest), 0o644)
	}}
	svc, st, ctx := newTestService(t, r)
	// "go test -run TestSumNegative" passes before the change ("no tests to run").
	m := runToEnd(t, svc, st, ctx, contractJSON(Criterion{ID: "weak", Kind: KindCommand,
		Command: []string{"go", "test", "-run", "TestSumNegative", "./..."}, MustChange: true}))
	got := results(t, m)["weak"]
	if got.Status != ResultError || got.BaselineStatus != ResultPass || !strings.Contains(got.Detail, "already passes") {
		t.Fatalf("weak check: %+v", got)
	}
	if m.Status != StatusBlocked {
		t.Fatalf("status %s, want blocked", m.Status)
	}
}

// Hidden acceptance files never appear in the agent's workspace or diff.
func TestMissionHiddenOverlayNotVisibleToAgent(t *testing.T) {
	var sawHidden bool
	r := &fakeRunner{edit: func(ws string) error {
		_, err := os.Stat(filepath.Join(ws, "zz_acceptance_test.go"))
		sawHidden = err == nil
		return nil
	}}
	svc, st, ctx := newTestService(t, r)
	m := runToEnd(t, svc, st, ctx, contractJSON())
	if sawHidden {
		t.Fatal("hidden acceptance test leaked into the agent workspace")
	}
	if slices.Contains(m.ChangedFiles, "zz_acceptance_test.go") || strings.Contains(m.Diff, "TestAcceptance") {
		t.Fatal("hidden acceptance test leaked into the diff")
	}
}
