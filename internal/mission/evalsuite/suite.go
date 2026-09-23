// Package evalsuite runs the offline mission evaluation suite: each case is a
// mission contract plus a scripted agent behaviour and the verdict the
// mission system must reach. Cases replay the agent's tool calls through the
// real tool registry, mission tool guard, verifiers and outcome rules, with
// no model and no network, so results are deterministic.
//
// What it measures: whether missions judge agent behaviour correctly
// (including adversarial behaviour). It does not measure a live model's
// ability to solve the tasks; that needs a provider and a budget.
package evalsuite

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Splits: dev cases are used while building, regression cases pin past
// bugs, held-out cases are not looked at when changing the system and gate
// promotion (Phase G).
const (
	SplitDev        = "dev"
	SplitRegression = "regression"
	SplitHeldOut    = "heldout"
)

// Case is one evaluation case.
type Case struct {
	ID          string          `json:"id"`
	Split       string          `json:"split"`
	Category    string          `json:"category"` // coding | research | data | adversarial | reliability
	Description string          `json:"description"`
	Contract    json.RawMessage `json:"contract"`
	Agent       AgentScript     `json:"agent"`
	Expect      Expectation     `json:"expect"`
	// Requires lists environment needs, e.g. "host" (only meaningful with
	// the host executor). Unmet cases are reported as skipped.
	Requires []string `json:"requires,omitempty"`
}

// AgentScript is the replayed agent behaviour.
type AgentScript struct {
	Steps []Step `json:"steps"`
	Reply string `json:"reply"`
	// Behavior: "" (run steps, reply), "hang" (steps, then block until the
	// mission times out), "error" (steps, then fail), "loop_killed".
	Behavior string   `json:"behavior,omitempty"`
	CostUSD  *float64 `json:"cost_usd,omitempty"`
}

// Step is one tool call.
type Step struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// Expectation is what the mission system must conclude.
type Expectation struct {
	Status   string            `json:"status"`
	Criteria map[string]string `json:"criteria,omitempty"` // criterion id -> pass|fail|error
	// Refused tools must not have run successfully (denied by the guard or
	// failed by a boundary).
	Refused        []string `json:"refused,omitempty"`
	ReasonContains string   `json:"reason_contains,omitempty"`
	ChangedHas     []string `json:"changed_has,omitempty"`
	ChangedLacks   []string `json:"changed_lacks,omitempty"`
}

// LoadCases reads every *.json case under dir/cases and validates it.
func LoadCases(dir string) ([]Case, error) {
	files, err := filepath.Glob(filepath.Join(dir, "cases", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Case
	seen := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var c Case
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("%s: duplicate case id %q", filepath.Base(f), c.ID)
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	return out, nil
}

var validStatuses = []string{"succeeded", "partial", "failed", "blocked", "cancelled"}

func (c *Case) validate() error {
	switch {
	case c.ID == "":
		return fmt.Errorf("id is required")
	case !slices.Contains([]string{SplitDev, SplitRegression, SplitHeldOut}, c.Split):
		return fmt.Errorf("split must be dev, regression or heldout")
	case !slices.Contains(validStatuses, c.Expect.Status):
		return fmt.Errorf("expect.status %q is not a terminal status", c.Expect.Status)
	case len(c.Contract) == 0:
		return fmt.Errorf("contract is required")
	case !slices.Contains([]string{"", "hang", "error", "loop_killed"}, c.Agent.Behavior):
		return fmt.Errorf("unknown agent.behavior %q", c.Agent.Behavior)
	}
	return nil
}

// Filter keeps cases whose split is in splits (all when empty).
func Filter(cases []Case, splits []string) []Case {
	if len(splits) == 0 {
		return cases
	}
	var out []Case
	for _, c := range cases {
		if slices.Contains(splits, c.Split) {
			out = append(out, c)
		}
	}
	return out
}
