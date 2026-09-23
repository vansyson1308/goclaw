package mission

import (
	"fmt"
	"strings"
)

// maxIntegrityScanBytes bounds the diff scanned for harness tampering. A
// larger change cannot be vouched for and is reported instead.
const maxIntegrityScanBytes = 32 << 20

// integrityPatterns are Go constructs that let code the agent wrote decide
// the outcome of a verifier command instead of the code under test: a
// TestMain or init() can exit 0 before any test runs, and linkname can
// replace runtime or testing internals. They are legitimate in some code,
// so a hit does not fail the mission; it blocks it for human review.
var integrityPatterns = []struct {
	needle   string
	testOnly bool
	why      string
}{
	{"func TestMain(", false, "adds TestMain (can bypass every test)"},
	{"func init()", false, "adds an init function (runs before any test)"},
	{"//go:linkname", false, "adds go:linkname (can replace runtime/testing internals)"},
	{"os.Exit(", true, "calls os.Exit in test code"},
	{"syscall.Exit(", true, "calls syscall.Exit in test code"},
	{"runtime.Goexit(", true, "calls runtime.Goexit in test code"},
}

// IntegrityFindings scans a unified diff for changes that can subvert
// command verifiers, and for nested .git entries that hide content from the
// diff. An empty result means nothing suspicious was found, not that the
// change is safe: verifier commands still execute agent-written code.
func IntegrityFindings(d *WorkspaceDiff, fullPatch string) []string {
	var out []string
	for _, n := range d.Nested {
		out = append(out, fmt.Sprintf("%s: nested .git entry (content is hidden from the diff)", n))
	}
	if len(fullPatch) > maxIntegrityScanBytes {
		return append(out, fmt.Sprintf("diff is larger than %d MiB and was not scanned", maxIntegrityScanBytes>>20))
	}
	for _, f := range d.ChangedFiles {
		if strings.HasSuffix(f, "_test.go") && !containsStr(d.Present, f) {
			out = append(out, f+": deletes a test file")
		}
	}
	file := ""
	for _, line := range strings.Split(fullPatch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			continue
		case strings.HasPrefix(line, "--- "), !strings.HasPrefix(line, "+"):
			continue
		}
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		added := line[1:]
		for _, p := range integrityPatterns {
			if p.testOnly && !strings.HasSuffix(file, "_test.go") {
				continue
			}
			if strings.Contains(added, p.needle) {
				out = append(out, fmt.Sprintf("%s: %s", file, p.why))
			}
		}
	}
	return dedupe(out)
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
