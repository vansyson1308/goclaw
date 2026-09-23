package mission

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// These tests reproduce attacks found in the Phase C adversarial review.
// Each one made a mission report success (or hang) before the fix.

// hijackTestMain makes every `go test` in the package exit 0 without running
// any test, while leaving sum.go buggy.
const hijackTestMain = `package sum

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(0) }
`

func hijackEdit(ws string) error {
	if err := os.WriteFile(filepath.Join(ws, "zz_main_test.go"), []byte(hijackTestMain), 0o644); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(ws, "sum_test.go"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, _ = f.WriteString("\n// covers negatives\n")
	if err := f.Close(); err != nil {
		return err
	}
	// Try to hide the hijack from the evidence the way the review did.
	_ = os.MkdirAll(filepath.Join(ws, ".git", "info"), 0o755)
	return os.WriteFile(filepath.Join(ws, ".git", "info", "exclude"), []byte("zz_main_test.go\n"), 0o644)
}

func TestTestMainHijackCannotSucceed(t *testing.T) {
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "done", edit: hijackEdit})
	m := runToEnd(t, svc, st, ctx, contractJSON())
	if m.Status == StatusSucceeded || m.Status == StatusPartial {
		t.Fatalf("TestMain hijack accepted: %s (%s)", m.Status, m.StatusReason)
	}
	res := results(t, m)
	if res["_integrity"].Status != ResultError || !strings.Contains(res["_integrity"].Detail, "TestMain") {
		t.Fatalf("integrity check missed TestMain: %+v", res["_integrity"])
	}
	if !slices.Contains(m.ChangedFiles, "zz_main_test.go") || !strings.Contains(m.Diff, "os.Exit(0)") {
		t.Fatalf("hijack file hidden from evidence: %v", m.ChangedFiles)
	}
}

// With expect_tests the hijacked command itself fails: exit 0 but the named
// hidden test never reported a pass.
func TestExpectTestsRequiresExplicitPass(t *testing.T) {
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "done", edit: hijackEdit})
	raw := contractWith(func(c *Contract) {
		c.Acceptance[1].ExpectTests = []string{"TestAcceptanceSumIncludesNegatives"}
	})
	m := runToEnd(t, svc, st, ctx, raw)
	b := results(t, m)["behavior"]
	if b.Status != ResultFail || b.Tests["TestAcceptanceSumIncludesNegatives"] != "missing" || b.ExitCode == nil || *b.ExitCode != 0 {
		t.Fatalf("behavior with hijack: %+v", b)
	}
	if m.Status == StatusSucceeded || m.Status == StatusPartial {
		t.Fatalf("status %s", m.Status)
	}

	// The honest fix passes the same contract, with per-test evidence.
	svc2, st2, ctx2 := newTestService(t, &fakeRunner{reply: "fixed", edit: honestFix})
	m2 := runToEnd(t, svc2, st2, ctx2, raw)
	b2 := results(t, m2)["behavior"]
	if m2.Status != StatusSucceeded || b2.Tests["TestAcceptanceSumIncludesNegatives"] != "pass" || b2.BaselineStatus != ResultFail {
		t.Fatalf("honest fix: %s (%s) %+v", m2.Status, m2.StatusReason, b2)
	}
	if strings.Contains(b2.OutputTail, `"Action"`) {
		t.Errorf("output tail should be test output, not raw JSON events: %q", b2.OutputTail)
	}
}

func honestFix(ws string) error {
	if err := os.WriteFile(filepath.Join(ws, "sum.go"), []byte(fixedSum), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(ws, "sum_negative_test.go"), []byte(regressionTest), 0o644)
}

// The agent cannot make git run commands or rewrite the diff through a .git
// directory it creates in the workspace; the repository lives outside.
func TestWorkspaceGitConfigCannotRunCommandsOrForgeDiff(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	edit := func(ws string) error {
		if err := honestFix(ws); err != nil {
			return err
		}
		_ = os.MkdirAll(filepath.Join(ws, ".git"), 0o755)
		cfg := "[diff]\n\texternal = sh -c 'touch " + marker + "; echo FAKE-PATCH'\n[core]\n\tfsmonitor = touch " + marker + "\n"
		return os.WriteFile(filepath.Join(ws, ".git", "config"), []byte(cfg), 0o644)
	}
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "done", edit: edit})
	m := runToEnd(t, svc, st, ctx, contractJSON())
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git executed a command from agent-written config")
	}
	if strings.Contains(m.Diff, "FAKE-PATCH") || !strings.Contains(m.Diff, "-		if x > 0 {") {
		t.Fatalf("diff was forged or is missing the change:\n%s", m.Diff)
	}
	// A .git entry in the workspace means the diff cannot be complete.
	if m.Status != StatusBlocked || !strings.Contains(results(t, m)["_integrity"].Detail, ".git") {
		t.Fatalf("nested .git not reported: %s (%s)", m.Status, m.StatusReason)
	}
}

// A .gitignore written by the agent cannot hide new files from the evidence.
func TestGitignoreCannotHideChanges(t *testing.T) {
	edit := func(ws string) error {
		if err := honestFix(ws); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("*.txt\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(ws, "hidden.txt"), []byte("secret change\n"), 0o644)
	}
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "done", edit: edit})
	m := runToEnd(t, svc, st, ctx, contractJSON())
	if !slices.Contains(m.ChangedFiles, "hidden.txt") || !strings.Contains(m.Diff, "secret change") {
		t.Fatalf("ignored file hidden from evidence: %v", m.ChangedFiles)
	}
}

// Invalid UTF-8 and NUL bytes (rejected by PostgreSQL) are cleaned, and a
// long multi-byte diff is cut on a rune boundary, so the mission reaches a
// terminal state instead of failing to persist and hanging in verifying.
func TestEvidenceIsAlwaysStorableText(t *testing.T) {
	edit := func(ws string) error {
		if err := honestFix(ws); err != nil {
			return err
		}
		big := "x" + strings.Repeat("€", 200000)
		if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte(big), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(ws, "latin1.txt"), []byte{'a', 0xff, 0xfe, 0x00, 'b', '\n'}, 0o644)
	}
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "done \xff\x00", edit: edit})
	m := runToEnd(t, svc, st, ctx, contractJSON())
	if !slices.Contains([]string{StatusSucceeded, StatusPartial, StatusFailed, StatusBlocked}, m.Status) {
		t.Fatalf("not terminal: %s", m.Status)
	}
	for name, v := range map[string]string{"diff": m.Diff, "summary": m.Summary, "reason": m.StatusReason} {
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			t.Errorf("%s is not storable text", name)
		}
	}
	if !m.DiffTruncated {
		t.Error("large diff should be marked truncated")
	}
	if s, cut := truncateText("ab€", 3); s != "ab" || !cut {
		t.Errorf("truncateText cut inside a rune: %q", s)
	}
}

// Deleting the existing tests must not count as "a regression test was
// added", and deleting a test file is flagged for review.
func TestDeletedTestFileIsNotEvidence(t *testing.T) {
	edit := func(ws string) error {
		if err := os.WriteFile(filepath.Join(ws, "sum.go"), []byte(fixedSum), 0o644); err != nil {
			return err
		}
		return os.Remove(filepath.Join(ws, "sum_test.go"))
	}
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "done", edit: edit})
	m := runToEnd(t, svc, st, ctx, contractJSON())
	res := results(t, m)
	if res["regression-test"].Status != ResultFail {
		t.Fatalf("deleted test counted as added: %+v", res["regression-test"])
	}
	if !strings.Contains(res["_integrity"].Detail, "deletes a test file") || m.Status == StatusSucceeded {
		t.Fatalf("test deletion not flagged: %s %+v", m.Status, res["_integrity"])
	}
}

// Hidden acceptance files are pinned at creation; changing them during the
// run (e.g. an unconfined exec rewriting them) is an error, never a pass.
func TestOverlayTamperingIsDetected(t *testing.T) {
	var svc *Service
	edit := func(ws string) error {
		if err := honestFix(ws); err != nil {
			return err
		}
		weak := "package sum\n\nimport \"testing\"\n\nfunc TestAcceptanceSumIncludesNegatives(t *testing.T) {}\n"
		return os.WriteFile(filepath.Join(svc.cfg.SourceRoot, "sumrepo-acceptance", "zz_acceptance_test.go"), []byte(weak), 0o644)
	}
	s, st, ctx := newTestService(t, &fakeRunner{reply: "done", edit: edit})
	svc = s
	m := runToEnd(t, svc, st, ctx, contractJSON())
	b := results(t, m)["behavior"]
	if b.Status != ResultError || !strings.Contains(b.Detail, "changed since the mission was created") || m.Status != StatusBlocked {
		t.Fatalf("overlay tampering not detected: %s %+v", m.Status, b)
	}
}

// The source is pinned too: if it changes between creation and start, the
// mission is blocked instead of silently working on different inputs.
func TestSourceChangeBeforeStartIsBlocked(t *testing.T) {
	gate := make(chan struct{})
	first := &fakeRunner{reply: "done", block: gate, started: make(chan struct{})}
	svc, st, ctx := newTestService(t, first)
	m1, err := svc.Create(ctx, contractJSON(), "alice")
	must(t, err)
	<-first.started
	m2, err := svc.Create(ctx, contractJSON(), "alice") // queued behind m1 (MaxConcurrent 1)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(svc.cfg.SourceRoot, "sumrepo", "sum.go"), []byte(fixedSum), 0o644))
	close(gate)
	svc.Wait()
	got, _ := st.GetMission(ctx, m2.ID)
	if got.Status != StatusBlocked || !strings.Contains(got.StatusReason, "changed since the mission was created") {
		t.Fatalf("second mission: %s (%s)", got.Status, got.StatusReason)
	}
	if g1, _ := st.GetMission(ctx, m1.ID); g1.Status == StatusBlocked {
		t.Fatalf("first mission should not be blocked by the later change: %s", g1.StatusReason)
	}
}

func TestSourceRootItselfIsRejected(t *testing.T) {
	svc, _, ctx := newTestService(t, &fakeRunner{})
	raw := contractWith(func(c *Contract) { c.Workspace.SourceDir = "." })
	if _, err := svc.Create(ctx, raw, "alice"); err == nil || !strings.Contains(err.Error(), "not the root itself") {
		t.Fatalf("source_dir \".\" accepted: %v", err)
	}
	raw = contractWith(func(c *Contract) { c.Acceptance[1].OverlayDir = "." })
	if _, err := svc.Create(ctx, raw, "alice"); err == nil {
		t.Fatal("overlay_dir \".\" accepted")
	}
}

// A cost limit cannot be shown to hold when the run reports no cost.
func TestUnknownCostWithLimitIsNotSuccess(t *testing.T) {
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "fixed", edit: honestFix})
	raw := contractWith(func(c *Contract) { c.Limits.MaxCostUSD = 0.5 })
	m := runToEnd(t, svc, st, ctx, raw)
	if m.Status != StatusBlocked || !strings.Contains(m.StatusReason, "cost") {
		t.Fatalf("unknown cost with a limit: %s (%s)", m.Status, m.StatusReason)
	}
	cost := 0.01
	svc2, st2, ctx2 := newTestService(t, &fakeRunner{reply: "fixed", edit: honestFix, cost: &cost})
	if m2 := runToEnd(t, svc2, st2, ctx2, raw); m2.Status != StatusSucceeded {
		t.Fatalf("known cost within limit: %s (%s)", m2.Status, m2.StatusReason)
	}
}

// The scratch directory (build caches, throwaway check copies) is removed
// after verification; the workspace and base repository remain as evidence.
func TestScratchIsRemovedAfterVerification(t *testing.T) {
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "fixed", edit: honestFix})
	m := runToEnd(t, svc, st, ctx, contractJSON())
	dir := filepath.Dir(m.WorkspacePath)
	if _, err := os.Stat(filepath.Join(dir, "scratch")); !os.IsNotExist(err) {
		t.Errorf("scratch dir still present: %v", err)
	}
	for _, keep := range []string{"workspace", "base.git"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s missing: %v", keep, err)
		}
	}
}

func contractWith(mut func(*Contract)) []byte {
	var c Contract
	_ = json.Unmarshal(contractJSON(), &c)
	mut(&c)
	b, _ := json.Marshal(c)
	return b
}

var _ = context.Background

// If the store rejects the terminal write with the evidence (as PostgreSQL
// did for invalid UTF-8), the mission still ends, as blocked, instead of
// staying in verifying forever.
func TestEvidenceRejectedByStoreStillEnds(t *testing.T) {
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "fixed", edit: honestFix})
	st.fail = func(op string) error {
		if op == "transition:"+StatusSucceeded {
			return errors.New(`pq: invalid byte sequence for encoding "UTF8"`)
		}
		return nil
	}
	m := runToEnd(t, svc, st, ctx, contractJSON())
	if m.Status != StatusBlocked || !strings.Contains(m.StatusReason, "could not persist verification evidence") {
		t.Fatalf("status %s (%s)", m.Status, m.StatusReason)
	}
}
