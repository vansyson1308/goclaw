package mission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Criterion ids become part of host paths (the per-check directory), so a
// contract must not be able to name one that leaves that directory, or that
// collides with the ids the system reserves for its own results.
func TestCriterionIDMustBeASafeName(t *testing.T) {
	for _, id := range []string{"x/../../../../home/gw/.ssh", "../x", "a/b", `a\b`, "a b", "_integrity", "_diff", "-x", ".", "..", "é"} {
		raw := contractWith(func(c *Contract) { c.Acceptance[0].ID = id })
		if _, err := ParseContract(raw); err == nil {
			t.Errorf("criterion id %q accepted", id)
		}
	}
	for _, id := range []string{"zz-check", "New_Check_2", "c9"} {
		raw := contractWith(func(c *Contract) { c.Acceptance[0].ID = id })
		if _, err := ParseContract(raw); err != nil {
			t.Errorf("criterion id %q rejected: %v", id, err)
		}
	}
}

// Defense in depth: even if an unvalidated id reaches the verifier (e.g. a
// contract stored before the id rule existed), the check directory it removes,
// fills and opens up stays inside the checks directory.
func TestVerifyCheckDirectoryCannotEscape(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(root, "scratch")
	victim := filepath.Join(root, "victim")
	must(t, os.MkdirAll(scratch, 0o700))
	must(t, os.MkdirAll(victim, 0o700))
	must(t, os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600))
	ws := t.TempDir()
	must(t, os.WriteFile(filepath.Join(ws, "f"), []byte("y"), 0o644))

	// checks/final-x/../../../victim == <root>/victim
	c := &Contract{Acceptance: []Criterion{{ID: "x/../../../victim", Kind: KindCommand, Command: []string{"true"}}}}
	res := Verify(context.Background(), c, VerifyEnv{Workspace: ws, Scratch: scratch, Exec: HostExecutor{}}, nil)
	if res[0].Status != ResultError {
		t.Errorf("escaping check id must be an error, got %+v", res[0])
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Fatalf("verifier touched a directory outside its checks dir: %v", err)
	}
	if fi, _ := os.Stat(victim); fi.Mode().Perm() != 0o700 {
		t.Fatalf("verifier changed permissions outside its checks dir: %v", fi.Mode().Perm())
	}
	_ = json.Valid
}

// The agent's final message is shown to every viewer of the mission; secrets
// it read from the workspace are scrubbed like command output, and its size
// is bounded.
func TestSummaryIsScrubbedAndBounded(t *testing.T) {
	out := &RunOutput{Content: "done, key=sk-ant-abc123-def456-ghi789-jkl012 " + strings.Repeat("x", 100<<10)}
	u, _ := aggregateUsage(&store.Mission{}, out)
	if u.Summary == nil || strings.Contains(*u.Summary, "sk-ant-abc123") {
		t.Fatalf("summary not scrubbed: %.80q", *u.Summary)
	}
	if len(*u.Summary) > maxSummaryBytes+64 {
		t.Fatalf("summary not bounded: %d bytes", len(*u.Summary))
	}
}

// The stored diff (evidence shown to viewers) is scrubbed; the frozen
// evidence copy on disk is untouched.
func TestStoredDiffIsScrubbed(t *testing.T) {
	if got := scrubPatch("+token = \"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij\"\n"); strings.Contains(got, "ghp_ABCDEF") {
		t.Fatalf("diff not scrubbed: %q", got)
	}
}
