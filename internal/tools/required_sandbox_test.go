package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/sandbox"
)

// missionRecSandbox records commands instead of running them.
type missionRecSandbox struct {
	mu   sync.Mutex
	cmds [][]string
}

func (r *missionRecSandbox) Exec(_ context.Context, command []string, _ string, _ ...sandbox.ExecOption) (*sandbox.ExecResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, command)
	return &sandbox.ExecResult{Stdout: "in-sandbox"}, nil
}
func (r *missionRecSandbox) Destroy(context.Context) error { return nil }
func (r *missionRecSandbox) ID() string                    { return "rec" }

type missionFakeManager struct {
	sb       *missionRecSandbox
	disabled bool
	gotCfg   *sandbox.Config
	gotWS    string
}

func (m *missionFakeManager) Get(_ context.Context, _ string, ws string, cfg *sandbox.Config, _ ...sandbox.GetOption) (sandbox.Sandbox, error) {
	if m.disabled {
		return nil, sandbox.ErrSandboxDisabled
	}
	m.gotCfg, m.gotWS = cfg, ws
	return m.sb, nil
}
func (m *missionFakeManager) Release(context.Context, string) error { return nil }
func (m *missionFakeManager) ReleaseAll(context.Context) error      { return nil }
func (m *missionFakeManager) Stop()                                 {}
func (m *missionFakeManager) Stats() map[string]any                 { return nil }

func requiredCtx(ws string, mgr sandbox.Manager, cfg *sandbox.Config) context.Context {
	ctx := WithToolWorkspace(context.Background(), ws)
	ctx = WithToolSandboxKey(ctx, "agent:a:mission:m:a1")
	return WithRequiredSandbox(ctx, mgr, cfg)
}

// A mission's exec runs in its sandbox even though the tool itself was
// built without one, using the mission's config (not the agent's).
func TestRequiredSandboxRoutesExec(t *testing.T) {
	ws := t.TempDir()
	mgr := &missionFakeManager{sb: &missionRecSandbox{}}
	cfg := &sandbox.Config{Mode: sandbox.ModeAll, Image: "mission-image"}
	tool := NewExecTool(ws, true) // host-only tool
	res := tool.Execute(requiredCtx(ws, mgr, cfg), map[string]any{"command": "touch marker-file"})
	if res.IsError || len(mgr.sb.cmds) != 1 || mgr.gotCfg != cfg {
		t.Fatalf("exec not routed to the mission sandbox: %+v cmds=%v", res, mgr.sb.cmds)
	}
	if _, err := os.Stat(filepath.Join(ws, "marker-file")); !os.IsNotExist(err) {
		t.Fatal("command ran on the host")
	}
}

// Without a usable sandbox, a mission's exec is refused, never run on the host.
func TestRequiredSandboxNeverFallsBackToHost(t *testing.T) {
	ws := t.TempDir()
	for name, mgr := range map[string]sandbox.Manager{"disabled": &missionFakeManager{disabled: true}, "none": nil} {
		ctx := WithToolWorkspace(context.Background(), ws)
		ctx = WithToolSandboxKey(ctx, "k")
		ctx = context.WithValue(ctx, ctxSandboxRequired, true)
		if mgr != nil {
			ctx = WithRequiredSandbox(ctx, mgr, nil)
		}
		res := NewExecTool(ws, true).Execute(ctx, map[string]any{"command": "touch marker-file"})
		if !res.IsError || !strings.Contains(res.ForLLM, "host execution is not allowed") {
			t.Fatalf("%s: %+v", name, res)
		}
		if _, err := os.Stat(filepath.Join(ws, "marker-file")); !os.IsNotExist(err) {
			t.Fatalf("%s: command ran on the host", name)
		}
	}
}

// File tools of a mission use the same sandbox (one container per attempt).
func TestRequiredSandboxRoutesFileTools(t *testing.T) {
	ws := t.TempDir()
	mgr := &missionFakeManager{sb: &missionRecSandbox{}}
	res := NewWriteFileTool(ws, true).Execute(requiredCtx(ws, mgr, nil), map[string]any{"path": "a.txt", "content": "x"})
	// FsBridge talks to the container directly; with this fake container ID
	// the call fails, which is enough to show it never touched the host.
	if mgr.gotWS == "" || !res.IsError {
		t.Fatalf("write_file did not use the mission sandbox: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("write_file wrote on the host")
	}
}
