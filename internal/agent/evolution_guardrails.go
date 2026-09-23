package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// AdaptationGuardrails controls auto-adaptation safety limits.
// Stored in agent other_config JSONB under "evolution_guardrails".
type AdaptationGuardrails struct {
	MaxDeltaPerCycle float64  `json:"max_delta_per_cycle"`     // max parameter change per cycle (default 0.1)
	MinDataPoints    int      `json:"min_data_points"`         // min metrics before applying (default 100)
	RollbackOnDrop   float64  `json:"rollback_on_drop_pct"`    // quality drop % triggering rollback (default 20.0)
	LockedParams     []string `json:"locked_params,omitempty"` // params that cannot be auto-changed
}

// DefaultGuardrails returns conservative defaults.
func DefaultGuardrails() AdaptationGuardrails {
	return AdaptationGuardrails{
		MaxDeltaPerCycle: 0.1,
		MinDataPoints:    100,
		RollbackOnDrop:   20.0,
	}
}

// GuardrailsForAgent returns the agent's configured guardrails
// (other_config.evolution_guardrails) layered over the defaults. Missing or
// non-positive numeric fields keep their default; the UI writes this key.
func GuardrailsForAgent(ag *store.AgentData) AdaptationGuardrails {
	g := DefaultGuardrails()
	if ag == nil || len(ag.OtherConfig) == 0 {
		return g
	}
	var oc struct {
		Guardrails *AdaptationGuardrails `json:"evolution_guardrails"`
	}
	if err := json.Unmarshal(ag.OtherConfig, &oc); err != nil || oc.Guardrails == nil {
		return g
	}
	c := oc.Guardrails
	if c.MaxDeltaPerCycle > 0 {
		g.MaxDeltaPerCycle = c.MaxDeltaPerCycle
	}
	if c.MinDataPoints > 0 {
		g.MinDataPoints = c.MinDataPoints
	}
	if c.RollbackOnDrop > 0 {
		g.RollbackOnDrop = c.RollbackOnDrop
	}
	g.LockedParams = c.LockedParams
	return g
}

// CheckGuardrails validates a suggestion against guardrail constraints.
// Returns an error describing the violation, or nil if safe to apply.
// LockedParams match suggestion parameter keys and, via CheckLockedTarget,
// the dotted config path a change would touch.
func CheckGuardrails(g AdaptationGuardrails, sg store.EvolutionSuggestion, dataPoints int) error {
	// Check minimum data points.
	minDP := g.MinDataPoints
	if minDP <= 0 {
		minDP = 100
	}
	if dataPoints < minDP {
		return fmt.Errorf("insufficient data: %d points, need %d", dataPoints, minDP)
	}

	// Check locked parameters.
	if len(g.LockedParams) > 0 {
		var params map[string]any
		if err := json.Unmarshal(sg.Parameters, &params); err == nil {
			for key := range params {
				if slices.Contains(g.LockedParams, key) {
					return fmt.Errorf("parameter %q is locked", key)
				}
			}
		}
	}
	return nil
}

// CheckLockedTarget rejects a change to a config path listed in LockedParams
// (dotted form, e.g. "tools_config.deny").
func CheckLockedTarget(g AdaptationGuardrails, column string, path []string) error {
	target := column + "." + strings.Join(path, ".")
	if slices.Contains(g.LockedParams, target) {
		return fmt.Errorf("parameter %q is locked", target)
	}
	return nil
}

var (
	// ErrRollbackConflict means the config no longer holds the applied value,
	// so rolling back would overwrite a newer, unrelated change.
	ErrRollbackConflict = errors.New("rollback conflict: config changed since apply")
	// ErrNotApplicable means the suggestion type has no reversible config change.
	ErrNotApplicable = errors.New("suggestion type has no config change to apply or roll back")
)

// ApplyToolOrder applies an approved tool_order suggestion by adding the tool
// to the originating agent's tools_config.deny — scoped to that agent only.
// Status check, config write, baseline and audit are one transaction.
func ApplyToolOrder(ctx context.Context, sugStore store.EvolutionSuggestionStore, sg store.EvolutionSuggestion, actor string) (*store.EvolutionSuggestion, error) {
	if sg.SuggestionType != store.SuggestToolOrder {
		return nil, ErrNotApplicable
	}
	var params struct {
		Tool string `json:"tool"`
	}
	if err := json.Unmarshal(sg.Parameters, &params); err != nil || strings.TrimSpace(params.Tool) == "" {
		return nil, fmt.Errorf("tool_order suggestion has no tool parameter")
	}
	tool := strings.TrimSpace(params.Tool)
	path := []string{"deny"}

	return sugStore.TransitionSuggestion(ctx, store.SuggestionTransition{
		ID:     sg.ID,
		From:   []string{store.SuggestionPending, store.SuggestionApproved},
		To:     store.SuggestionApplied,
		Actor:  actor,
		Action: "apply",
		Column: "tools_config",
		Detail: map[string]any{"tool": tool, "scope": "agent"},
		Mutate: func(_ *store.EvolutionSuggestion, current json.RawMessage) (json.RawMessage, *store.AgentConfigChange, error) {
			before, err := store.ConfigGetPath(current, path)
			if err != nil {
				return nil, nil, err
			}
			var deny []string
			if before.Present {
				if err := json.Unmarshal(before.Value, &deny); err != nil {
					return nil, nil, fmt.Errorf("tools_config.deny is not a string list: %w", err)
				}
			}
			if slices.Contains(deny, tool) {
				return nil, nil, fmt.Errorf("%w: tool %q is already denied for this agent", store.ErrSuggestionStateConflict, tool)
			}
			afterVal, _ := json.Marshal(append(slices.Clone(deny), tool))
			after := store.ConfigValue{Present: true, Value: afterVal}
			next, err := store.ConfigSetPath(current, path, after)
			if err != nil {
				return nil, nil, err
			}
			return next, &store.AgentConfigChange{Column: "tools_config", Path: path, Before: before, After: after}, nil
		},
	})
}

// RollbackSuggestion reverts an applied suggestion to its exact prior state
// (including removing keys that did not exist before). It refuses with
// ErrRollbackConflict when the current value differs from what was applied.
func RollbackSuggestion(ctx context.Context, sugStore store.EvolutionSuggestionStore, sg store.EvolutionSuggestion, actor, reason string) (*store.EvolutionSuggestion, error) {
	change := sg.AppliedChange
	legacy := false
	if change == nil {
		// Rows applied before migration 000098 kept a presence-less baseline in
		// parameters._baseline (threshold only). A key missing from it was
		// absent before apply, so restoring means deleting it.
		c, ok := legacyThresholdChange(sg)
		if !ok {
			return nil, ErrNotApplicable
		}
		change, legacy = c, true
	}
	return sugStore.TransitionSuggestion(ctx, store.SuggestionTransition{
		ID:     sg.ID,
		From:   []string{store.SuggestionApplied},
		To:     store.SuggestionRolledBack,
		Actor:  actor,
		Action: "rollback",
		Column: change.Column,
		Detail: map[string]any{"reason": reason, "legacy_baseline": legacy},
		Mutate: func(locked *store.EvolutionSuggestion, current json.RawMessage) (json.RawMessage, *store.AgentConfigChange, error) {
			c := change
			if locked.AppliedChange != nil {
				c = locked.AppliedChange // authoritative copy under lock
			}
			now, err := store.ConfigGetPath(current, c.Path)
			if err != nil {
				return nil, nil, err
			}
			if !legacy && !store.ConfigValuesEqual(now, c.After) {
				return nil, nil, fmt.Errorf("%w (at %s.%s)", ErrRollbackConflict, c.Column, strings.Join(c.Path, "."))
			}
			next, err := store.ConfigSetPath(current, c.Path, c.Before)
			if err != nil {
				return nil, nil, err
			}
			return next, &store.AgentConfigChange{Column: c.Column, Path: c.Path, Before: now, After: c.Before}, nil
		},
	})
}

func legacyThresholdChange(sg store.EvolutionSuggestion) (*store.AgentConfigChange, bool) {
	if sg.SuggestionType != store.SuggestThreshold {
		return nil, false
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(sg.Parameters, &params) != nil {
		return nil, false
	}
	raw, ok := params["_baseline"]
	if !ok {
		return nil, false
	}
	var baseline map[string]json.RawMessage
	if json.Unmarshal(raw, &baseline) != nil || baseline == nil {
		return nil, false
	}
	before := store.ConfigValue{}
	if v, ok := baseline["retrieval_threshold"]; ok {
		before = store.ConfigValue{Present: true, Value: v}
	}
	return &store.AgentConfigChange{Column: "other_config", Path: []string{"retrieval_threshold"}, Before: before}, true
}

// AcknowledgeAdvisory records review of a suggestion type that has no safe
// automatic change (see docs/mission-control/DECISIONS.md D5). Status moves to
// "approved"; nothing in the agent's config changes.
func AcknowledgeAdvisory(ctx context.Context, sugStore store.EvolutionSuggestionStore, sg store.EvolutionSuggestion, actor, reason string) (*store.EvolutionSuggestion, error) {
	updated, err := sugStore.TransitionSuggestion(ctx, store.SuggestionTransition{
		ID:     sg.ID,
		From:   []string{store.SuggestionPending},
		To:     store.SuggestionApproved,
		Actor:  actor,
		Action: "acknowledge",
		Detail: map[string]any{"advisory": true, "reason": reason},
	})
	if err == nil {
		slog.Info("evolution.suggestion.acknowledged", "suggestion", sg.ID, "type", sg.SuggestionType)
	}
	return updated, err
}
