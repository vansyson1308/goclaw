package evalsuite

import (
	"fmt"
	"strings"
)

// Markdown renders a report as a Markdown table.
func Markdown(rep *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Executor: `%s`. %d/%d passed (%d skipped). False successes: **%d**. False failures: %d.\n\n",
		rep.Executor, rep.Passed, rep.Total-rep.Skipped, rep.Skipped, rep.FalseSuccess, rep.FalseFailure)
	b.WriteString("| Split | Passed |\n|---|---|\n")
	for _, split := range []string{SplitDev, SplitRegression, SplitHeldOut} {
		if sc, ok := rep.BySplit[split]; ok {
			fmt.Fprintf(&b, "| %s | %d/%d |\n", split, sc.Passed, sc.Total)
		}
	}
	b.WriteString("\n| Case | Split | Category | Expected | Actual | Result | ms |\n|---|---|---|---|---|---|---|\n")
	for _, c := range rep.Cases {
		result := "pass"
		switch {
		case c.Skipped != "":
			result = "skipped: " + c.Skipped
		case !c.Pass:
			result = "**FAIL**: " + strings.ReplaceAll(strings.Join(c.Mismatches, "; "), "|", "\\|")
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %d |\n", c.ID, c.Split, c.Category, c.Expected, c.Actual, result, c.DurationMS)
	}
	return b.String()
}
