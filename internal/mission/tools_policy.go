package mission

import (
	"slices"
)

// Action classes for tools a mission agent may call. Everything outside
// toolClasses is refused: messaging, scheduling, delegation, memory, skills,
// MCP and other tools whose effects leave the mission workspace or cannot be
// safely repeated when an interrupted attempt is retried.
const (
	ClassWorkspaceRead  = "workspace_read"
	ClassWorkspaceWrite = "workspace_write"
	ClassExec           = "exec"
	ClassNetworkRead    = "network_read"
	ClassExternal       = "external" // refused
)

// toolClasses lists every tool a contract may enable.
var toolClasses = map[string]string{
	"read_file":     ClassWorkspaceRead,
	"list_files":    ClassWorkspaceRead,
	"read_document": ClassWorkspaceRead,
	"datetime":      ClassWorkspaceRead,
	"write_file":    ClassWorkspaceWrite,
	"edit":          ClassWorkspaceWrite,
	"exec":          ClassExec,
	"web_fetch":     ClassNetworkRead,
	"web_search":    ClassNetworkRead,
}

// DefaultTools is the allowlist when a contract does not set limits.tools.
// Network tools are opt-in.
var DefaultTools = []string{"read_file", "list_files", "write_file", "edit", "exec", "datetime"}

// SafeTools returns every tool a contract may enable, sorted.
func SafeTools() []string {
	out := make([]string, 0, len(toolClasses))
	for name := range toolClasses {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// ToolClass returns the action class of a tool (ClassExternal if unknown).
func ToolClass(name string) string {
	if c, ok := toolClasses[name]; ok {
		return c
	}
	return ClassExternal
}

// AllowedTools returns the effective allowlist for a contract.
func (c *Contract) AllowedTools() []string {
	if len(c.Limits.Tools) > 0 {
		return c.Limits.Tools
	}
	return DefaultTools
}
