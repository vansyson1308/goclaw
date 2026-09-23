package improve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/mission"
)

const benchDir = "../../../evals/improve"

func testExec(t *testing.T) mission.Executor {
	t.Helper()
	for _, bin := range []string{"go", "git", "sh", "awk", "sha256sum"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	return mission.HostExecutor{GoCache: t.TempDir()} // shared cache: test speed only
}

// The whole lifecycle on the offline benchmark: a worse candidate, a gaming
// candidate and a dev-overfit candidate are rejected; a better one is
// promoted; an incident task exposes a regression and the promotion is
// rolled back; the fixed candidate is then promoted. Every decision cites
// score digests.
func TestImprovementLifecycle(t *testing.T) {
	ex := testExec(t)
	ctx := context.Background()
	b, err := LoadBenchmark(benchDir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"), "v1")
	if err != nil {
		t.Fatal(err)
	}
	propose := func(id string, wantPromote bool, reasonHas string) {
		t.Helper()
		d, err := l.Propose(ctx, b, id, ex)
		if err != nil {
			t.Fatal(err)
		}
		if d.Promote != wantPromote || !strings.Contains(strings.Join(d.Reasons, "; "), reasonHas) {
			t.Fatalf("%s: promote=%v reasons=%v (want promote=%v, reason containing %q)", id, d.Promote, d.Reasons, wantPromote, reasonHas)
		}
	}
	propose("v3", false, "regresses tasks the champion solves: research-channel, research-owner")
	propose("v4", false, "integrity finding")
	propose("v5", false, "held-out solved 0 < champion 1")
	if l.Champion != "v1" {
		t.Fatalf("champion changed by a rejected candidate: %s", l.Champion)
	}
	propose("v2", true, "solves 7 (champion 3)")
	if l.Champion != "v2" || len(l.Previous) != 1 {
		t.Fatalf("after promotion: %+v", l)
	}

	// An incident adds a task v1 handled and v2 does not: monitoring rolls back.
	if err := b.AddTasks(filepath.Join(benchDir, "incidents")); err != nil {
		t.Fatal(err)
	}
	rolled, err := l.Monitor(ctx, b, ex)
	if err != nil {
		t.Fatal(err)
	}
	last := l.Events[len(l.Events)-1]
	if !rolled || l.Champion != "v1" || last.Kind != EventRolledBack || !strings.Contains(last.Reasons[0], "research-compression") {
		t.Fatalf("rollback: rolled=%v champion=%s event=%+v", rolled, l.Champion, last)
	}
	propose("v6", true, "solves 8 (champion 4)")

	// The ledger persists and every decision cites evidence.
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenLedger(l.path, "ignored")
	if err != nil || again.Champion != "v6" || len(again.Events) != 6 {
		t.Fatalf("reloaded ledger: %+v %v", again, err)
	}
	kinds := []string{}
	for _, e := range again.Events {
		kinds = append(kinds, e.Kind)
		for id, dg := range e.Evidence {
			if !strings.HasPrefix(dg, "sha256:") || e.BenchmarkDigest == "" {
				t.Errorf("event %s lacks evidence for %s", e.Kind, id)
			}
		}
	}
	if got := strings.Join(kinds, ","); got != "rejected,rejected,rejected,promoted,rolled_back,promoted" {
		t.Fatalf("event sequence %s", got)
	}
}

func TestGateRules(t *testing.T) {
	champ := &Score{Results: []TaskResult{{Task: "a", Split: "dev", Status: "succeeded"}, {Task: "b", Split: "heldout", Status: "succeeded"}},
		Solved: map[string]int{"dev": 1, "heldout": 1}}
	same := &Score{Results: champ.Results, Solved: champ.Solved}
	if d := Gate(champ, same); d.Promote {
		t.Fatalf("equal candidate promoted: %v", d.Reasons)
	}
	better := &Score{Results: append(append([]TaskResult{}, champ.Results...), TaskResult{Task: "c", Split: "dev", Status: "succeeded"}),
		Solved: map[string]int{"dev": 2, "heldout": 1}}
	if d := Gate(champ, better); !d.Promote {
		t.Fatalf("better candidate rejected: %v", d.Reasons)
	}
	// A partial result is not a solve.
	partial := &Score{Results: []TaskResult{{Task: "a", Status: "succeeded"}, {Task: "b", Split: "heldout", Status: "partial"}, {Task: "c", Status: "succeeded"}},
		Solved: map[string]int{"dev": 2}}
	if d := Gate(champ, partial); d.Promote {
		t.Fatalf("partial counted as solved: %v", d.Reasons)
	}
	if d := Gate(nil, better); !d.Promote {
		t.Fatalf("first candidate rejected: %v", d.Reasons)
	}
	_ = os.Getenv
}
