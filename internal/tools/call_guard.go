package tools

import "context"

// CallGuard authorizes and records every tool call of one run. Missions
// use it to enforce a tool allowlist and to write a durable receipt before
// each call (see docs/mission-control/MISSIONS.md). Begin returns a non-nil
// error to refuse the call (the text is shown to the model); otherwise it
// returns done, which must be called with the call's outcome.
type CallGuard interface {
	Begin(ctx context.Context, tool string, args map[string]any) (done func(isError bool), err error)
}

const ctxCallGuard toolContextKey = "tool_call_guard"

// WithCallGuard installs a guard for every tool call made with ctx.
func WithCallGuard(ctx context.Context, g CallGuard) context.Context {
	return context.WithValue(ctx, ctxCallGuard, g)
}

// CallGuardFromContext returns the installed guard, or nil.
func CallGuardFromContext(ctx context.Context) CallGuard {
	g, _ := ctx.Value(ctxCallGuard).(CallGuard)
	return g
}

// GuardedExecute runs exec under the context's CallGuard, if any. A refused
// call returns an error result and exec is not invoked.
func GuardedExecute(ctx context.Context, tool string, args map[string]any, exec func() *Result) *Result {
	g := CallGuardFromContext(ctx)
	if g == nil {
		return exec()
	}
	done, err := g.Begin(ctx, tool, args)
	if err != nil {
		return ErrorResult(err.Error())
	}
	res := exec()
	done(res == nil || res.IsError)
	return res
}
