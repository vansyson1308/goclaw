package tools

import (
	"context"

	"github.com/nextlevelbuilder/goclaw/internal/sandbox"
)

// Required sandbox (missions): the run's file and exec tools must use this
// manager and config, and exec never falls back to the host.

const (
	ctxSandboxManager  toolContextKey = "tool_sandbox_manager"
	ctxSandboxRequired toolContextKey = "tool_sandbox_required"
)

// WithRequiredSandbox routes every sandbox-aware tool of the run through
// mgr with cfg, and forbids host execution for exec.
func WithRequiredSandbox(ctx context.Context, mgr sandbox.Manager, cfg *sandbox.Config) context.Context {
	ctx = context.WithValue(ctx, ctxSandboxManager, mgr)
	ctx = WithSandboxConfig(ctx, cfg)
	return context.WithValue(ctx, ctxSandboxRequired, true)
}

// SandboxRequiredFromCtx reports whether host execution is forbidden.
func SandboxRequiredFromCtx(ctx context.Context) bool {
	v, _ := ctx.Value(ctxSandboxRequired).(bool)
	return v
}

// sandboxManagerFor returns the run's required manager, if any, else own.
func sandboxManagerFor(ctx context.Context, own sandbox.Manager) sandbox.Manager {
	if m, ok := ctx.Value(ctxSandboxManager).(sandbox.Manager); ok && m != nil {
		return m
	}
	return own
}

const sandboxRequiredError = "this run may only execute commands inside its sandbox, which is unavailable; host execution is not allowed"
