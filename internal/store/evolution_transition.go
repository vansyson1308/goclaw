package store

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// CheckTransitionFrom reports ErrSuggestionStateConflict when status is not in from.
func CheckTransitionFrom(status string, from []string) error {
	if slices.Contains(from, status) {
		return nil
	}
	return fmt.Errorf("%w: status is %q, expected one of %v", ErrSuggestionStateConflict, status, from)
}

// ApplyTransitionFields updates sg in memory to reflect a committed transition.
// Stores call it before persisting so PG and SQLite stay field-for-field equal.
func ApplyTransitionFields(sg *EvolutionSuggestion, t SuggestionTransition, change *AgentConfigChange, now time.Time) {
	sg.Status = t.To
	sg.StateVersion++
	switch t.To {
	case SuggestionApproved, SuggestionRejected:
		sg.ReviewedBy, sg.ReviewedAt = t.Actor, &now
	case SuggestionApplied:
		if sg.ReviewedAt == nil {
			sg.ReviewedBy, sg.ReviewedAt = t.Actor, &now
		}
		sg.AppliedBy, sg.AppliedAt = t.Actor, &now
		if change != nil {
			sg.AppliedChange = change
		}
	case SuggestionRolledBack:
		sg.RolledBackBy, sg.RolledBackAt = t.Actor, &now
	}
}

// TransitionEventDetail builds the audit detail for a transition.
func TransitionEventDetail(t SuggestionTransition, change *AgentConfigChange) json.RawMessage {
	d := map[string]any{}
	for k, v := range t.Detail {
		d[k] = v
	}
	if change != nil {
		d["change"] = change
	}
	if len(d) == 0 {
		return nil
	}
	b, _ := json.Marshal(d)
	return b
}

// ConfigGetPath reads the value at path inside a JSON object document.
func ConfigGetPath(doc json.RawMessage, path []string) (ConfigValue, error) {
	root, err := decodeObject(doc)
	if err != nil {
		return ConfigValue{}, err
	}
	var cur any = root
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ConfigValue{}, nil
		}
		v, ok := m[key]
		if !ok {
			return ConfigValue{}, nil
		}
		cur = v
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return ConfigValue{}, err
	}
	return ConfigValue{Present: true, Value: b}, nil
}

// ConfigSetPath returns doc with path set to v, or with the key removed when
// v is absent. Intermediate objects are created as needed; empty
// intermediates left behind by a removal are pruned. That restores the
// original shape exactly for one-level paths (all current changes); for
// deeper paths a pre-existing empty intermediate object would also be pruned.
// A non-object intermediate is an error, never overwritten.
func ConfigSetPath(doc json.RawMessage, path []string, v ConfigValue) (json.RawMessage, error) {
	if len(path) == 0 {
		return nil, fmt.Errorf("empty config path")
	}
	root, err := decodeObject(doc)
	if err != nil {
		return nil, err
	}
	var val any
	if v.Present {
		if err := json.Unmarshal(v.Value, &val); err != nil {
			return nil, fmt.Errorf("decode value: %w", err)
		}
	}
	if err := setPath(root, path, v.Present, val); err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

// ConfigValuesEqual compares two values semantically (JSON equality).
func ConfigValuesEqual(a, b ConfigValue) bool {
	if a.Present != b.Present {
		return false
	}
	if !a.Present {
		return true
	}
	var x, y any
	if json.Unmarshal(a.Value, &x) != nil || json.Unmarshal(b.Value, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x) // map keys are sorted by encoding/json
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

func setPath(m map[string]any, path []string, present bool, val any) error {
	key := path[0]
	if len(path) == 1 {
		if present {
			m[key] = val
		} else {
			delete(m, key)
		}
		return nil
	}
	raw, exists := m[key]
	child, ok := raw.(map[string]any)
	switch {
	case exists && !ok:
		return fmt.Errorf("config key %q is not an object", key)
	case !exists && !present:
		return nil // nothing to remove
	case !exists:
		child = map[string]any{}
		m[key] = child
	}
	if err := setPath(child, path[1:], present, val); err != nil {
		return err
	}
	if !present && len(child) == 0 {
		delete(m, key)
	}
	return nil
}

func decodeObject(doc json.RawMessage) (map[string]any, error) {
	root := map[string]any{}
	if len(doc) == 0 || string(doc) == "null" {
		return root, nil
	}
	if err := json.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("config is not a JSON object: %w", err)
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, nil
}
