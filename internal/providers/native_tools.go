package providers

// NativeToolExecutor is implemented by providers that run tools themselves
// (a CLI or agent process with its own file/shell tools) instead of asking
// GoClaw to execute tool calls. GoClaw's tool guards (e.g. the mission tool
// allowlist and receipts) cannot see those calls.
type NativeToolExecutor interface {
	ExecutesToolsNatively() bool
}

// ExecutesToolsNatively: the Claude CLI runs its own built-in tools.
func (p *ClaudeCLIProvider) ExecutesToolsNatively() bool { return true }

// ExecutesToolsNatively: ACP agents run their own tools.
func (p *ACPProvider) ExecutesToolsNatively() bool { return true }

// ExecutesToolsNatively: a fallback chain is as permissive as its most
// permissive candidate, since any of them may serve the next call.
func (p *ModelFallbackProvider) ExecutesToolsNatively() bool {
	for _, c := range append([]FallbackCandidate{p.primary}, p.fallbacks...) {
		if n, ok := c.Provider.(NativeToolExecutor); ok && n.ExecutesToolsNatively() {
			return true
		}
	}
	return false
}
