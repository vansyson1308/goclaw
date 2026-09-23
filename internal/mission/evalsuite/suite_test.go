package evalsuite

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const suiteDir = "../../../evals/missions"

func TestSuiteCasesAreValidAndBalanced(t *testing.T) {
	cases, err := LoadCases(suiteDir)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	expectSuccess := 0
	for _, c := range cases {
		counts[c.Split]++
		if isSuccess(c.Expect.Status) {
			expectSuccess++
		}
	}
	if len(cases) < 24 || counts[SplitDev] < 8 || counts[SplitRegression] < 8 || counts[SplitHeldOut] < 6 {
		t.Fatalf("suite too small: %d cases %v", len(cases), counts)
	}
	// Both directions must be exercised: cases that must succeed and cases
	// that must not.
	if expectSuccess < 6 || len(cases)-expectSuccess < 12 {
		t.Fatalf("unbalanced expectations: %d success of %d", expectSuccess, len(cases))
	}
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"go", "git", "sh", "awk", "sha256sum"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
}

// A smoke subset always runs; the whole suite runs with GOCLAW_RUN_EVALS=1
// (or through `goclaw mission eval`).
func TestSuite(t *testing.T) {
	requireTools(t)
	cases, err := LoadCases(suiteDir)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GOCLAW_RUN_EVALS") != "1" {
		var smoke []Case
		for _, c := range cases {
			if c.ID == "research-correct" || c.ID == "held-research-edits-docs" || c.ID == "data-correct" {
				smoke = append(smoke, c)
			}
		}
		cases = smoke
	}
	rep, err := Run(context.Background(), cases, Options{SourceRoot: filepath.Join(suiteDir, "testdata")})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Cases {
		if !r.Pass && r.Skipped == "" {
			t.Errorf("%s (%s): %v", r.ID, r.Split, r.Mismatches)
		}
	}
	if !rep.OK() {
		t.Fatalf("suite: %d/%d passed, %d false successes", rep.Passed, rep.Total-rep.Skipped, rep.FalseSuccess)
	}
}
