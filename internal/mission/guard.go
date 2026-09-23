package mission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// toolGuard enforces a mission's tool allowlist and writes a receipt for
// every call before it runs. The receipt insert is fenced by the attempt's
// lease, so a worker that lost its lease (or a cancelled mission) cannot
// perform any further tool call, and no side effect happens without a
// durable record: if the receipt cannot be written, the call is refused.
type toolGuard struct {
	ctx       context.Context // tenant-scoped, outlives the run
	store     store.MissionStore
	missionID uuid.UUID
	fence     store.MissionFence
	allowed   map[string]bool
	allowList string
	seq       atomic.Int64
}

var _ tools.CallGuard = (*toolGuard)(nil)

func newToolGuard(ctx context.Context, st store.MissionStore, id uuid.UUID, fence store.MissionFence, c *Contract) *toolGuard {
	names := c.AllowedTools()
	g := &toolGuard{ctx: ctx, store: st, missionID: id, fence: fence, allowed: map[string]bool{}, allowList: strings.Join(names, ", ")}
	for _, n := range names {
		g.allowed[n] = true
	}
	return g
}

func (g *toolGuard) Begin(_ context.Context, tool string, args map[string]any) (func(bool), error) {
	r := store.MissionReceipt{
		MissionID:   g.missionID,
		Attempt:     g.fence.Attempt,
		Seq:         int(g.seq.Add(1)),
		Tool:        tool,
		ActionClass: ToolClass(tool),
		ArgsDigest:  argsDigest(args),
	}
	if !g.allowed[tool] {
		r.Status = store.ReceiptDenied
		r.Reason = fmt.Sprintf("not in the mission tool allowlist (class %s)", r.ActionClass)
		_ = g.store.BeginMissionReceipt(g.ctx, r, g.fence) // best effort: the call is refused either way
		return nil, fmt.Errorf("tool %q is not available in this mission (allowed: %s)", tool, g.allowList)
	}
	r.Status = store.ReceiptStarted
	if err := g.store.BeginMissionReceipt(g.ctx, r, g.fence); err != nil {
		return nil, fmt.Errorf("tool call refused: the mission could not record it (%v); the attempt may have been cancelled or taken over", err)
	}
	start := time.Now()
	return func(isError bool) {
		status := store.ReceiptOK
		if isError {
			status = store.ReceiptError
		}
		// If this write is lost the receipt stays "started": the outcome of
		// that call is unknown, which is exactly what it records.
		_ = g.store.CompleteMissionReceipt(g.ctx, g.missionID, r.Attempt, r.Seq, status, time.Since(start).Milliseconds())
	}, nil
}

// argsDigest identifies a call's arguments without storing them (they may
// contain file contents or secrets).
func argsDigest(args map[string]any) string {
	b, _ := json.Marshal(args) // map keys are sorted: stable
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:8])
}
