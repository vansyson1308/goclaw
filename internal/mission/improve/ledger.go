package improve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/mission"
)

// Event kinds recorded in the ledger.
const (
	EventPromoted   = "promoted"
	EventRejected   = "rejected"
	EventRolledBack = "rolled_back"
	EventMonitorOK  = "monitor_ok"
)

// Event is one lifecycle decision with the evidence behind it.
type Event struct {
	At              time.Time         `json:"at"`
	Kind            string            `json:"kind"`
	Candidate       string            `json:"candidate,omitempty"`
	Champion        string            `json:"champion"` // champion after the event
	Reasons         []string          `json:"reasons"`
	Evidence        map[string]string `json:"evidence"` // candidate id -> score digest
	Solved          map[string]int    `json:"solved"`   // candidate id -> tasks solved
	BenchmarkDigest string            `json:"benchmark_digest"`
	Executor        string            `json:"executor"`
}

// Ledger is the persistent lifecycle state: the current champion, the
// champions it replaced (for rollback) and every decision.
type Ledger struct {
	Champion string   `json:"champion"`
	Previous []string `json:"previous"` // stack; last = the champion before the current one
	Events   []Event  `json:"events"`
	path     string
}

// OpenLedger loads a ledger file, or starts one with an initial champion.
func OpenLedger(path, initialChampion string) (*Ledger, error) {
	l := &Ledger{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		l.Champion = initialChampion
		return l, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(raw, l); err != nil {
		return nil, fmt.Errorf("ledger %s: %w", path, err)
	}
	l.path = path
	return l, nil
}

// Save writes the ledger atomically.
func (l *Ledger) Save() error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

func benchmarkDigest(b *Benchmark) string {
	raw, _ := json.Marshal(b.Tasks)
	h := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(h[:])
}

// Propose evaluates candidate and the current champion on the same tasks
// with the same executor, applies the gate and records the decision. The
// champion changes only on promotion.
func (l *Ledger) Propose(ctx context.Context, b *Benchmark, candidate string, exec mission.Executor) (Decision, error) {
	if candidate == l.Champion {
		return Decision{}, fmt.Errorf("%s is already the champion", candidate)
	}
	cand, err := Evaluate(ctx, b, candidate, nil, exec)
	if err != nil {
		return Decision{}, err
	}
	var champ *Score
	if l.Champion != "" {
		if champ, err = Evaluate(ctx, b, l.Champion, nil, exec); err != nil {
			return Decision{}, err
		}
	}
	d := Gate(champ, cand)
	ev := Event{At: time.Now().UTC(), Candidate: candidate, Reasons: d.Reasons, BenchmarkDigest: benchmarkDigest(b), Executor: cand.Executor,
		Evidence: map[string]string{candidate: cand.Digest}, Solved: map[string]int{candidate: cand.SolvedTotal()}}
	if champ != nil {
		ev.Evidence[champ.Candidate], ev.Solved[champ.Candidate] = champ.Digest, champ.SolvedTotal()
	}
	if d.Promote {
		if l.Champion != "" {
			l.Previous = append(l.Previous, l.Champion)
		}
		l.Champion = candidate
		ev.Kind = EventPromoted
	} else {
		ev.Kind = EventRejected
	}
	ev.Champion = l.Champion
	l.Events = append(l.Events, ev)
	return d, nil
}

// Monitor re-evaluates the champion against the champion it replaced on the
// current benchmark (which may have grown, e.g. a task added after an
// incident). If the previous champion solves something the current one does
// not, or solves more, the promotion is rolled back.
func (l *Ledger) Monitor(ctx context.Context, b *Benchmark, exec mission.Executor) (rolledBack bool, err error) {
	if len(l.Previous) == 0 {
		return false, fmt.Errorf("nothing to compare against: %s has no predecessor", l.Champion)
	}
	prevID := l.Previous[len(l.Previous)-1]
	cur, err := Evaluate(ctx, b, l.Champion, nil, exec)
	if err != nil {
		return false, err
	}
	prev, err := Evaluate(ctx, b, prevID, nil, exec)
	if err != nil {
		return false, err
	}
	ev := Event{At: time.Now().UTC(), BenchmarkDigest: benchmarkDigest(b), Executor: cur.Executor,
		Evidence: map[string]string{cur.Candidate: cur.Digest, prev.Candidate: prev.Digest},
		Solved:   map[string]int{cur.Candidate: cur.SolvedTotal(), prev.Candidate: prev.SolvedTotal()}}
	curSolved := cur.solvedSet()
	var lost []string
	for t := range prev.solvedSet() {
		if !curSolved[t] {
			lost = append(lost, t)
		}
	}
	switch {
	case len(lost) > 0 || cur.SolvedTotal() < prev.SolvedTotal() || cur.Violations > prev.Violations:
		ev.Kind = EventRolledBack
		ev.Reasons = append(ev.Reasons, fmt.Sprintf("%s regressed against %s (solves %d vs %d; lost %v; violations %d vs %d)",
			cur.Candidate, prev.Candidate, cur.SolvedTotal(), prev.SolvedTotal(), lost, cur.Violations, prev.Violations))
		ev.Candidate = l.Champion
		l.Champion = prevID
		l.Previous = l.Previous[:len(l.Previous)-1]
		rolledBack = true
	default:
		ev.Kind = EventMonitorOK
		ev.Reasons = []string{fmt.Sprintf("%s still at least as good as %s (solves %d vs %d)", cur.Candidate, prev.Candidate, cur.SolvedTotal(), prev.SolvedTotal())}
	}
	ev.Champion = l.Champion
	l.Events = append(l.Events, ev)
	return rolledBack, nil
}
