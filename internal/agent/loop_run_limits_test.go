package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/pipeline"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

type nativeToolsProvider struct{ providers.Provider }

func (nativeToolsProvider) Name() string                { return "cli-agent" }
func (nativeToolsProvider) ExecutesToolsNatively() bool { return true }

func TestRunTokenBudgetStopsBeforeNextCall(t *testing.T) {
	req := &RunRequest{TokenBudget: 1000}
	state := &pipeline.RunState{}
	if err := checkRunLimits(req, state, nil); err != nil {
		t.Fatalf("fresh run refused: %v", err)
	}
	state.Think.TotalUsage = providers.Usage{PromptTokens: 900, CompletionTokens: 99}
	if err := checkRunLimits(req, state, nil); err != nil {
		t.Fatalf("under budget refused: %v", err)
	}
	state.Think.TotalUsage.CompletionTokens = 100
	if err := checkRunLimits(req, state, nil); !errors.Is(err, ErrTokenBudgetExhausted) {
		t.Fatalf("at budget: want ErrTokenBudgetExhausted, got %v", err)
	}
	if err := checkRunLimits(&RunRequest{}, state, nil); err != nil {
		t.Fatalf("no budget set must not limit: %v", err)
	}
}

func TestMissionRefusesProvidersThatRunTheirOwnTools(t *testing.T) {
	p := nativeToolsProvider{}
	err := checkRunLimits(&RunRequest{MissionWorkspace: "/ws"}, &pipeline.RunState{}, p)
	if err == nil || !strings.Contains(err.Error(), "cannot be used for missions") {
		t.Fatalf("native-tool provider allowed for a mission: %v", err)
	}
	if err := checkRunLimits(&RunRequest{}, &pipeline.RunState{}, p); err != nil {
		t.Fatalf("ordinary runs may use it: %v", err)
	}
	var _ providers.NativeToolExecutor = (*providers.ClaudeCLIProvider)(nil)
	var _ providers.NativeToolExecutor = (*providers.ACPProvider)(nil)
}
