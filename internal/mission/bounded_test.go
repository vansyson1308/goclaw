//go:build linux

package mission

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// spawnDetached starts a process in its own session in the workspace, like
// an agent's `nohup cmd &` through exec, and returns its pid.
func spawnDetached(t *testing.T, ws, script string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = ws
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	must(t, cmd.Start())
	go func() { _ = cmd.Wait() }() // reap it in this test process
	return cmd.Process.Pid
}

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(b))
	return len(fields) > 2 && fields[2] != "Z"
}

// Review D/H2: work the agent detached from its tool call does not outlive
// the attempt.
func TestDetachedProcessesDoNotOutliveTheAttempt(t *testing.T) {
	var pid int
	edit := func(ws string) error {
		pid = spawnDetached(t, ws, "sleep 300")
		return honestFix(ws)
	}
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "fixed", edit: edit})
	m := runToEnd(t, svc, st, ctx, contractJSON())
	deadline := time.Now().Add(2 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("detached process %d still running after the mission ended (%s)", pid, m.Status)
	}
	if m.Status != StatusSucceeded {
		t.Fatalf("status %s (%s)", m.Status, m.StatusReason)
	}
}

// Review D/H2: a background writer that swaps in the fix right after the
// diff is taken cannot make the verified tree differ from the recorded
// evidence. It runs outside the workspace (cwd /), so the process sweep does
// not catch it; the frozen evidence copy is what defends here.
func TestLateWriterCannotForgeEvidence(t *testing.T) {
	edit := func(ws string) error {
		attemptDir := filepath.Dir(ws)
		must(t, os.WriteFile(filepath.Join(attemptDir, "fixed.go.txt"), []byte(fixedSum), 0o644))
		must(t, os.WriteFile(filepath.Join(attemptDir, "late_test.go.txt"), []byte(regressionTest), 0o644))
		idx := filepath.Join(attemptDir, "base.git", "index")
		script := `i=$(stat -c %y ` + idx + `); while [ "$(stat -c %y ` + idx + `)" = "$i" ]; do sleep 0.02; done; ` +
			`cp ` + filepath.Join(attemptDir, "fixed.go.txt") + ` ` + filepath.Join(ws, "sum.go") + `; ` +
			`cp ` + filepath.Join(attemptDir, "late_test.go.txt") + ` ` + filepath.Join(ws, "late_test.go")
		spawnDetached(t, "/", script)
		return nil // the agent itself changes nothing
	}
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "done", edit: edit})
	m := runToEnd(t, svc, st, ctx, contractJSON())
	if m.Status == StatusSucceeded || m.Status == StatusPartial {
		t.Fatalf("late writer made the mission %s: changed=%v", m.Status, m.ChangedFiles)
	}
	if !strings.Contains(m.WorkspacePath, "evidence-") {
		t.Fatalf("verification did not use a frozen copy: %s", m.WorkspacePath)
	}
	data, err := os.ReadFile(filepath.Join(m.WorkspacePath, "sum.go"))
	must(t, err)
	if string(data) != buggySum {
		t.Fatal("the evidence copy changed after it was frozen")
	}
}

// Review D/M2: the cost limit applies to the mission, across attempts.
func TestCostLimitCountsAllAttempts(t *testing.T) {
	prev, this := 0.9, 0.9
	u, ok := aggregateUsage(&store.Mission{CostUSD: &prev, InputTokens: 10}, &RunOutput{CostUSD: &this, InputTokens: 10})
	if !ok || costLimitReason(1.0, u) == "" {
		t.Fatalf("two attempts at $0.90 passed a $1.00 limit (total %v)", *u.CostUSD)
	}
	if r := costLimitReason(1.0, store.MissionUpdate{CostUSD: &this}); r != "" {
		t.Fatalf("single attempt under the limit refused: %s", r)
	}
}
