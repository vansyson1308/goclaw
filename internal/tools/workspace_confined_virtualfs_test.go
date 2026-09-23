package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/cache"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// A mission (workspace-confined) run works on the repository's own files. The
// agent's virtual context and memory files (stored in the database and
// injected into every later run of that agent) must be neither read nor
// written from it: a prompt-injected mission would otherwise persist
// instructions into the agent, and repository files named AGENTS.md or
// MEMORY.md would be shadowed by the agent's private copies.
func TestWorkspaceConfinedRunBypassesVirtualFiles(t *testing.T) {
	ws := t.TempDir()
	agentID := uuid.New()
	ctx := store.WithAgentID(context.Background(), agentID)
	ctx = store.WithUserID(ctx, "user-1")
	ctx = WithToolWorkspace(ctx, ws)
	ctx = WithWorkspaceConfined(ctx)

	memStore := newMockMemoryStore()
	memStore.docs[docKey(agentID.String(), "user-1", "MEMORY.md")] = "database-memory"
	memIntc := NewMemoryInterceptor(memStore, ws)
	ctxIntc := NewContextFileInterceptor(&stubAgentStore{agentFiles: []store.AgentContextFileData{
		{AgentID: agentID, FileName: "AGENTS.md", Content: "database-agents"},
	}}, ws, cache.NewInMemoryCache[[]store.AgentContextFileData](), cache.NewInMemoryCache[[]store.AgentContextFileData]())

	for name, body := range map[string]string{"MEMORY.md": "repo-memory", "AGENTS.md": "repo-agents"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	reader := NewReadFileTool(ws, true)
	reader.SetMemoryInterceptor(memIntc)
	reader.SetContextFileInterceptor(ctxIntc)
	for path, want := range map[string]string{"MEMORY.md": "repo-memory", "AGENTS.md": "repo-agents"} {
		r := reader.Execute(ctx, map[string]any{"path": path})
		if r.IsError || !strings.Contains(r.ForLLM, want) || strings.Contains(r.ForLLM, "database-") {
			t.Fatalf("read %s in a mission routed to the agent's virtual file: %#v", path, r)
		}
	}

	writer := NewWriteFileTool(ws, true)
	writer.SetMemoryInterceptor(memIntc)
	writer.SetContextFileInterceptor(ctxIntc)
	if r := writer.Execute(ctx, map[string]any{"path": "MEMORY.md", "content": "injected"}); r.IsError {
		t.Fatalf("physical write failed: %#v", r)
	}
	editor := NewEditTool(ws, true)
	editor.SetMemoryInterceptor(memIntc)
	editor.SetContextFileInterceptor(ctxIntc)
	if r := editor.Execute(ctx, map[string]any{"path": "MEMORY.md", "old_string": "injected", "new_string": "edited"}); r.IsError {
		t.Fatalf("physical edit failed: %#v", r)
	}
	if got := memStore.docs[docKey(agentID.String(), "user-1", "MEMORY.md")]; got != "database-memory" {
		t.Fatalf("mission mutated the agent's memory: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "MEMORY.md")); string(b) != "edited" {
		t.Fatalf("workspace file = %q, want the mission's edit", b)
	}
}
