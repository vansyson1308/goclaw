package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A confined (mission) run may only touch its workspace, even when the
// tool, tenant or team would normally allow more.
func TestWorkspaceConfinedIgnoresExtraAllowedPaths(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	ctx := WithTenantAllowedPaths(context.Background(), []string{outside})
	ctx = WithToolTeamWorkspace(ctx, outside)
	if got := allowedWriteWithTeamWorkspace(ctx, []string{outside}); len(got) != 3 {
		t.Fatalf("precondition: unconfined run should see extra paths, got %v", got)
	}
	ctx = WithWorkspaceConfined(ctx)
	if got := allowedWithTeamWorkspace(ctx, []string{outside}); got != nil {
		t.Fatalf("confined read prefixes = %v, want none", got)
	}
	target := filepath.Join(outside, "x.txt")
	tool := NewWriteFileTool(ws, true)
	res := tool.Execute(WithToolWorkspace(ctx, ws), map[string]any{"path": target, "content": "x"})
	if !res.IsError {
		t.Fatal("confined write outside the workspace succeeded")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("file was written outside the workspace")
	}
	if res := tool.Execute(WithToolWorkspace(ctx, ws), map[string]any{"path": "ok.txt", "content": "x"}); res.IsError {
		t.Fatalf("write inside the workspace failed: %s", res.ForLLM)
	}
}
