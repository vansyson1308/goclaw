// Package mission runs objective-driven agent work in an isolated workspace
// and judges the outcome with machine-checkable acceptance criteria that run
// outside the agent. See docs/mission-control/MISSIONS.md.
package mission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// ContractVersion is the only contract schema version this build accepts.
const ContractVersion = 1

// Criterion kinds.
const (
	KindCommand      = "command"
	KindFileChanged  = "file_changed"
	KindFileContains = "file_contains"
)

const (
	maxCriteria         = 20
	maxCommandArgs      = 64
	defaultCmdTimeout   = 120 // seconds
	maxCmdTimeout       = 1800
	defaultRunTimeout   = 900
	maxRunTimeout       = 7200
	defaultMaxIteration = 30
	maxIterations       = 200
)

// Contract is the versioned, immutable description of a mission.
type Contract struct {
	Version     int         `json:"version"`
	Title       string      `json:"title"`
	Objective   string      `json:"objective"`
	Constraints []string    `json:"constraints,omitempty"`
	NonGoals    []string    `json:"non_goals,omitempty"`
	Agent       string      `json:"agent"`
	Workspace   Workspace   `json:"workspace"`
	Acceptance  []Criterion `json:"acceptance"`
	Limits      Limits      `json:"limits,omitempty"`
}

// Workspace names the source copied into the mission's isolated workspace.
// SourceDir is resolved relative to the configured missions source root and
// may not escape it.
type Workspace struct {
	SourceDir string `json:"source_dir"`
}

// Criterion is one acceptance check, evaluated after the agent run.
type Criterion struct {
	ID             string   `json:"id"`
	Description    string   `json:"description,omitempty"`
	Kind           string   `json:"kind"`
	Command        []string `json:"command,omitempty"`         // command
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"` // command
	// MustChange requires the check to FAIL on the unmodified baseline, so a
	// pass proves the change (a check that already passes proves nothing).
	MustChange bool `json:"must_change,omitempty"` // command, file_contains
	// OverlayDir (relative to the mission source root) holds hidden
	// acceptance files copied over a throwaway copy of the workspace before
	// the command runs. The agent never sees them.
	OverlayDir string `json:"overlay_dir,omitempty"` // command
	Glob       string `json:"glob,omitempty"`        // file_changed
	Path       string `json:"path,omitempty"`        // file_contains
	Text       string `json:"text,omitempty"`        // file_contains
}

// Limits bound one mission run.
type Limits struct {
	MaxIterations  int     `json:"max_iterations,omitempty"`
	TimeoutSeconds int     `json:"timeout_seconds,omitempty"`
	MaxCostUSD     float64 `json:"max_cost_usd,omitempty"`
}

// ParseContract decodes and validates a contract, rejecting unknown fields
// so a typo in a criterion never silently weakens verification.
func ParseContract(raw []byte) (*Contract, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var c Contract
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("invalid contract JSON: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks structure and fills defaults.
func (c *Contract) Validate() error {
	if c.Version != ContractVersion {
		return fmt.Errorf("unsupported contract version %d (want %d)", c.Version, ContractVersion)
	}
	c.Title = strings.TrimSpace(c.Title)
	c.Objective = strings.TrimSpace(c.Objective)
	c.Agent = strings.TrimSpace(c.Agent)
	switch {
	case c.Title == "":
		return fmt.Errorf("title is required")
	case len(c.Title) > 200:
		return fmt.Errorf("title too long")
	case c.Objective == "":
		return fmt.Errorf("objective is required")
	case c.Agent == "":
		return fmt.Errorf("agent is required")
	}
	if err := validateRelPath(c.Workspace.SourceDir, "workspace.source_dir"); err != nil {
		return err
	}
	if len(c.Acceptance) == 0 {
		return fmt.Errorf("at least one acceptance criterion is required")
	}
	if len(c.Acceptance) > maxCriteria {
		return fmt.Errorf("too many acceptance criteria (%d > %d)", len(c.Acceptance), maxCriteria)
	}
	seen := map[string]bool{}
	provesChange := false
	for i := range c.Acceptance {
		cr := &c.Acceptance[i]
		cr.ID = strings.TrimSpace(cr.ID)
		if cr.ID == "" {
			return fmt.Errorf("criterion %d: id is required", i)
		}
		if seen[cr.ID] {
			return fmt.Errorf("criterion %q: duplicate id", cr.ID)
		}
		seen[cr.ID] = true
		if err := cr.validate(); err != nil {
			return fmt.Errorf("criterion %q: %w", cr.ID, err)
		}
		provesChange = provesChange || cr.ProvesChange()
	}
	if !provesChange {
		return fmt.Errorf("at least one criterion must prove the change (file_changed, or must_change on a command/file_contains); checks that already pass on the untouched code prove nothing")
	}
	l := &c.Limits
	switch {
	case l.MaxIterations < 0 || l.MaxIterations > maxIterations:
		return fmt.Errorf("limits.max_iterations must be 0..%d", maxIterations)
	case l.TimeoutSeconds < 0 || l.TimeoutSeconds > maxRunTimeout:
		return fmt.Errorf("limits.timeout_seconds must be 0..%d", maxRunTimeout)
	case l.MaxCostUSD < 0:
		return fmt.Errorf("limits.max_cost_usd must be >= 0")
	}
	if l.MaxIterations == 0 {
		l.MaxIterations = defaultMaxIteration
	}
	if l.TimeoutSeconds == 0 {
		l.TimeoutSeconds = defaultRunTimeout
	}
	return nil
}

func (cr *Criterion) validate() error {
	switch cr.Kind {
	case KindCommand:
		if len(cr.Command) == 0 || strings.TrimSpace(cr.Command[0]) == "" {
			return fmt.Errorf("command is required")
		}
		if len(cr.Command) > maxCommandArgs {
			return fmt.Errorf("too many command arguments")
		}
		if cr.TimeoutSeconds < 0 || cr.TimeoutSeconds > maxCmdTimeout {
			return fmt.Errorf("timeout_seconds must be 0..%d", maxCmdTimeout)
		}
		if cr.TimeoutSeconds == 0 {
			cr.TimeoutSeconds = defaultCmdTimeout
		}
		if cr.OverlayDir != "" {
			if err := validateRelPath(cr.OverlayDir, "overlay_dir"); err != nil {
				return err
			}
		}
	case KindFileChanged:
		if cr.MustChange || cr.OverlayDir != "" {
			return fmt.Errorf("must_change/overlay_dir do not apply to file_changed (it proves change by definition)")
		}
		if cr.Glob == "" {
			return fmt.Errorf("glob is required")
		}
		if _, err := path.Match(cr.Glob, "x"); err != nil {
			return fmt.Errorf("invalid glob: %w", err)
		}
	case KindFileContains:
		if cr.OverlayDir != "" {
			return fmt.Errorf("overlay_dir applies to command criteria only")
		}
		if err := validateRelPath(cr.Path, "path"); err != nil {
			return err
		}
		if cr.Text == "" {
			return fmt.Errorf("text is required")
		}
	default:
		return fmt.Errorf("unknown kind %q", cr.Kind)
	}
	return nil
}

// ProvesChange reports whether passing this criterion is evidence that the
// workspace was actually changed (as opposed to a guard that may already pass).
func (cr Criterion) ProvesChange() bool {
	return cr.Kind == KindFileChanged || cr.MustChange
}

// validateRelPath accepts only clean, relative paths that stay inside their root.
func validateRelPath(p, field string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return fmt.Errorf("%s must be relative", field)
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%s escapes its root", field)
	}
	return nil
}

// Canonical returns the contract's canonical JSON (after defaults).
func (c *Contract) Canonical() []byte {
	b, _ := json.Marshal(c)
	return b
}

// Digest is the SHA-256 of the canonical contract; evidence references it.
func (c *Contract) Digest() string {
	sum := sha256.Sum256(c.Canonical())
	return hex.EncodeToString(sum[:])
}
