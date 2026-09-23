package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

func TestInjectContext_MissionWorkspacePinsToolWorkspace(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "missions", "m1", "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	req := &RunRequest{SessionKey: "agent:worker:mission:m1", UserID: "u1", MissionWorkspace: ws}
	setup, err := newArtifactTestLoop(root).injectContext(context.Background(), req)
	if err != nil {
		t.Fatalf("injectContext: %v", err)
	}
	if got := tools.ToolWorkspaceFromCtx(setup.ctx); got != ws {
		t.Fatalf("tool workspace = %q, want mission workspace %q", got, ws)
	}
}

func TestInjectContext_MissionWorkspaceFailsClosed(t *testing.T) {
	root := t.TempDir()
	loop := newArtifactTestLoop(root)
	cases := map[string]*RunRequest{
		"missing dir":   {SessionKey: "s", UserID: "u", MissionWorkspace: filepath.Join(root, "nope")},
		"relative path": {SessionKey: "s", UserID: "u", MissionWorkspace: "rel/ws"},
		"with team ws":  {SessionKey: "s", UserID: "u", MissionWorkspace: root, TeamWorkspace: filepath.Join(root, "team")},
	}
	for name, req := range cases {
		if _, err := loop.injectContext(context.Background(), req); err == nil {
			t.Errorf("%s: want error, got nil (must not fall back to another workspace)", name)
		}
	}
}
