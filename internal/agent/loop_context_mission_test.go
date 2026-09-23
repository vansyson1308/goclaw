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
	req := &RunRequest{SessionKey: "agent:worker:mission:m1", UserID: "u1", MissionWorkspace: ws, ToolGuard: allowAllGuard{}}
	setup, err := newArtifactTestLoop(root).injectContext(context.Background(), req)
	if err != nil {
		t.Fatalf("injectContext: %v", err)
	}
	if got := tools.ToolWorkspaceFromCtx(setup.ctx); got != ws {
		t.Fatalf("tool workspace = %q, want mission workspace %q", got, ws)
	}
	// Exclusive: no team, tenant or tool-level paths widen file access.
	if !tools.WorkspaceConfinedFromCtx(setup.ctx) {
		t.Fatal("mission run is not workspace-confined")
	}
	if got := tools.ToolTeamWorkspaceFromCtx(setup.ctx); got != "" {
		t.Fatalf("mission run has a team workspace %q", got)
	}
	if tools.CallGuardFromContext(setup.ctx) == nil {
		t.Fatal("mission run has no tool guard installed")
	}
}

type allowAllGuard struct{}

func (allowAllGuard) Begin(context.Context, string, map[string]any) (func(bool), error) {
	return func(bool) {}, nil
}

func TestInjectContext_MissionWorkspaceFailsClosed(t *testing.T) {
	root := t.TempDir()
	loop := newArtifactTestLoop(root)
	cases := map[string]*RunRequest{
		"missing dir":   {SessionKey: "s", UserID: "u", MissionWorkspace: filepath.Join(root, "nope"), ToolGuard: allowAllGuard{}},
		"relative path": {SessionKey: "s", UserID: "u", MissionWorkspace: "rel/ws", ToolGuard: allowAllGuard{}},
		"without guard": {SessionKey: "s", UserID: "u", MissionWorkspace: root},
		"with team ws":  {SessionKey: "s", UserID: "u", MissionWorkspace: root, TeamWorkspace: filepath.Join(root, "team"), ToolGuard: allowAllGuard{}},
		"with team id":  {SessionKey: "s", UserID: "u", MissionWorkspace: root, TeamID: "00000000-0000-0000-0000-000000000001", ToolGuard: allowAllGuard{}},
		"with leader":   {SessionKey: "s", UserID: "u", MissionWorkspace: root, LeaderAgentID: "lead", ToolGuard: allowAllGuard{}},
	}
	for name, req := range cases {
		if _, err := loop.injectContext(context.Background(), req); err == nil {
			t.Errorf("%s: want error, got nil (must not fall back to another workspace)", name)
		}
	}
}
