package cmd

import (
	"strings"
	"testing"
)

// Agent-controlled text (summary, diff, check output) printed by the CLI must
// not carry terminal control sequences: they could rewrite what the operator
// sees (e.g. hide a failing check) or poke at the terminal.
func TestTermSafeStripsControlSequences(t *testing.T) {
	in := "ok\x1b[2K\x1b[1A\rFAIL hidden\x07\x9b31m\ttab\nnext"
	got := termSafe(in)
	for _, bad := range []string{"\x1b", "\r", "\x07", "\x9b"} {
		if strings.Contains(got, bad) {
			t.Fatalf("control byte %q kept: %q", bad, got)
		}
	}
	if !strings.Contains(got, "\ttab\nnext") {
		t.Fatalf("tabs and newlines must be kept: %q", got)
	}
}
