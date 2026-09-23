package mission

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// guardFixture is a running mission attempt with a real tool registry.
type guardFixture struct {
	st    *memStore
	ctx   context.Context
	id    uuid.UUID
	fence store.MissionFence
	ws    string
	reg   *tools.Registry
}

func newGuardFixture(t *testing.T) *guardFixture {
	t.Helper()
	st := newMemStore()
	ctx := store.WithTenantID(context.Background(), uuid.New())
	m := &store.Mission{Title: "g", MaxAttempts: 2}
	must(t, st.CreateMission(ctx, m, "alice"))
	fence := store.MissionFence{Owner: "w1", Attempt: 1}
	_, err := st.TransitionMission(ctx, m.ID, []string{StatusPlanned}, StatusRunning, "sys", "",
		store.MissionUpdate{Claim: &store.MissionClaim{Owner: "w1", TTL: time.Minute}})
	must(t, err)
	ws := t.TempDir()
	reg := tools.NewRegistry()
	reg.Register(tools.NewWriteFileTool(ws, true))
	return &guardFixture{st: st, ctx: ctx, id: m.ID, fence: fence, ws: ws, reg: reg}
}

func (f *guardFixture) call(g *toolGuard, tool string, args map[string]any) *tools.Result {
	ctx := tools.WithCallGuard(tools.WithToolWorkspace(f.ctx, f.ws), g)
	return f.reg.ExecuteWithContext(ctx, tool, args, "", "", "", "", nil)
}

func contractWithTools(names ...string) *Contract {
	return &Contract{Limits: Limits{Tools: names}}
}

func TestToolGuardRecordsAllowedCallsBeforeTheyRun(t *testing.T) {
	f := newGuardFixture(t)
	g := newToolGuard(f.ctx, f.st, f.id, f.fence, contractWithTools())
	res := f.call(g, "write_file", map[string]any{"path": "a.txt", "content": "secret-value-123"})
	if res.IsError {
		t.Fatalf("allowed call failed: %s", res.ForLLM)
	}
	recs, _ := f.st.ListMissionReceipts(f.ctx, f.id)
	if len(recs) != 1 || recs[0].Status != store.ReceiptOK || recs[0].Tool != "write_file" ||
		recs[0].ActionClass != ClassWorkspaceWrite || recs[0].Seq != 1 || recs[0].Attempt != 1 {
		t.Fatalf("receipts: %+v", recs)
	}
	if strings.Contains(recs[0].ArgsDigest, "secret") || !strings.HasPrefix(recs[0].ArgsDigest, "sha256:") {
		t.Fatalf("arguments must be digested, not stored: %q", recs[0].ArgsDigest)
	}
}

// A tool outside the allowlist (here: write_file when the contract only
// allows reading) is refused before it runs, and the refusal is recorded.
func TestToolGuardRefusesToolsOutsideTheAllowlist(t *testing.T) {
	f := newGuardFixture(t)
	g := newToolGuard(f.ctx, f.st, f.id, f.fence, contractWithTools("read_file", "list_files"))
	res := f.call(g, "write_file", map[string]any{"path": "b.txt", "content": "x"})
	if !res.IsError || !strings.Contains(res.ForLLM, "not available in this mission") {
		t.Fatalf("refused call result: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(f.ws, "b.txt")); !os.IsNotExist(err) {
		t.Fatal("refused tool still ran")
	}
	recs, _ := f.st.ListMissionReceipts(f.ctx, f.id)
	if len(recs) != 1 || recs[0].Status != store.ReceiptDenied {
		t.Fatalf("denial not recorded: %+v", recs)
	}
}

// External, non-idempotent tools are never allowed by default.
func TestDefaultMissionToolsExcludeExternalEffects(t *testing.T) {
	for _, name := range []string{"message", "spawn", "delegate", "cron", "team_tasks", "memory_search", "skill_manage", "send_file", "web_fetch", "mcp_github_create_issue"} {
		g := newToolGuard(context.Background(), newMemStore(), uuid.New(), store.MissionFence{}, contractWithTools())
		if g.allowed[name] {
			t.Errorf("%s allowed by default", name)
		}
	}
	if ToolClass("message") != ClassExternal || ToolClass("exec") != ClassExec {
		t.Error("tool classes")
	}
	if _, err := ParseContract(contractWith(func(c *Contract) { c.Limits.Tools = []string{"read_file", "message"} })); err == nil {
		t.Error("contract may not enable an external tool")
	}
}

// A worker that lost its lease cannot act: the fenced receipt insert fails,
// so the call is refused and nothing runs.
func TestToolGuardFailsClosedWithoutTheLease(t *testing.T) {
	f := newGuardFixture(t)
	g := newToolGuard(f.ctx, f.st, f.id, f.fence, contractWithTools())
	// Another worker takes over (requeue + new claim).
	_, err := f.st.TransitionMission(f.ctx, f.id, []string{StatusRunning}, StatusPlanned, "sys", "", store.MissionUpdate{Fence: &f.fence, ClearLease: true})
	must(t, err)
	res := f.call(g, "write_file", map[string]any{"path": "c.txt", "content": "x"})
	if !res.IsError || !strings.Contains(res.ForLLM, "could not record") {
		t.Fatalf("stale worker's call not refused: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(f.ws, "c.txt")); !os.IsNotExist(err) {
		t.Fatal("stale worker's tool ran")
	}
	// Same when the database is unreachable: no side effect without a record.
	f2 := newGuardFixture(t)
	g2 := newToolGuard(f2.ctx, f2.st, f2.id, f2.fence, contractWithTools())
	f2.st.fail = func(op string) error {
		if op == "receipt" {
			return errDBDown
		}
		return nil
	}
	if res := f2.call(g2, "write_file", map[string]any{"path": "d.txt", "content": "x"}); !res.IsError {
		t.Fatal("call ran without a durable receipt")
	}
}

// A call whose outcome was never acknowledged (crash between the side effect
// and the completion write) stays "started": honestly unknown.
func TestUnacknowledgedCallStaysStarted(t *testing.T) {
	f := newGuardFixture(t)
	g := newToolGuard(f.ctx, f.st, f.id, f.fence, contractWithTools())
	f.st.fail = func(op string) error {
		if op == "receipt_complete" {
			return errDBDown
		}
		return nil
	}
	if res := f.call(g, "write_file", map[string]any{"path": "e.txt", "content": "x"}); res.IsError {
		t.Fatalf("call failed: %s", res.ForLLM)
	}
	f.st.fail = nil
	recs, _ := f.st.ListMissionReceipts(f.ctx, f.id)
	if len(recs) != 1 || recs[0].Status != store.ReceiptStarted {
		t.Fatalf("want an unacknowledged started receipt, got %+v", recs)
	}
}

// Review D/M4: once the run is stopping (timeout, cancel), no new tool call
// starts, even if the lease is still held.
func TestToolGuardRefusesCallsAfterTheRunStops(t *testing.T) {
	f := newGuardFixture(t)
	g := newToolGuard(f.ctx, f.st, f.id, f.fence, contractWithTools())
	stopped, cancel := context.WithCancel(tools.WithToolWorkspace(f.ctx, f.ws))
	cancel()
	res := f.reg.ExecuteWithContext(tools.WithCallGuard(stopped, g), "write_file", map[string]any{"path": "late.txt", "content": "x"}, "", "", "", "", nil)
	if !res.IsError || !strings.Contains(res.ForLLM, "stopping") {
		t.Fatalf("call after stop: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(f.ws, "late.txt")); !os.IsNotExist(err) {
		t.Fatal("tool ran after the run stopped")
	}
}
