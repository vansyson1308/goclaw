package agent

import (
	"errors"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/internal/pipeline"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

// ErrTokenBudgetExhausted is returned instead of a model call once a run
// has used its RunRequest.TokenBudget.
var ErrTokenBudgetExhausted = errors.New("run token budget exhausted")

// checkRunLimits runs before every model call of a run.
func checkRunLimits(req *RunRequest, state *pipeline.RunState, provider providers.Provider) error {
	if req.TokenBudget > 0 {
		// Cached input counts: providers such as Anthropic report cache reads
		// and writes outside prompt_tokens.
		used := int64(pipeline.InputContextTokens(state.Think.TotalUsage) + state.Think.TotalUsage.CompletionTokens)
		if used >= req.TokenBudget {
			return fmt.Errorf("%w: %d of %d tokens used", ErrTokenBudgetExhausted, used, req.TokenBudget)
		}
	}
	// A mission's tool guard only sees tools GoClaw executes; a provider
	// that runs its own tools would bypass the allowlist and receipts.
	if req.MissionWorkspace != "" {
		if n, ok := provider.(providers.NativeToolExecutor); ok && n.ExecutesToolsNatively() {
			return fmt.Errorf("provider %q runs its own tools outside GoClaw's tool guard and cannot be used for missions", provider.Name())
		}
	}
	return nil
}
