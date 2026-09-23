// Package storetest holds store conformance checks shared by the PostgreSQL
// and SQLite implementations, so both editions are held to the same contract.
package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// MissionLeases checks attempts, lease fencing and write-ahead receipts.
// ctxA and ctxB must carry two different tenants.
func MissionLeases(t *testing.T, ms store.MissionStore, ctxA, ctxB context.Context) {
	t.Helper()
	m := &store.Mission{OwnerID: "alice", AgentKey: "coder", Title: "lease", Contract: []byte(`{"version":1}`), ContractDigest: "d", MaxAttempts: 2,
		Pins: []byte(`{"source":"sha256:abc"}`)}
	if err := ms.CreateMission(ctxA, m, "alice"); err != nil {
		t.Fatal(err)
	}
	if g, err := ms.GetMission(ctxA, m.ID); err != nil || g == nil || !strings.Contains(string(g.Pins), "sha256:abc") {
		t.Fatalf("pins not stored: %+v %v", g, err)
	}
	if m.MaxAttempts != 2 {
		t.Fatalf("max attempts %d", m.MaxAttempts)
	}
	w1 := store.MissionFence{Owner: "worker-1", Attempt: 1}

	// Claim starts attempt 1 and takes the lease for a TTL on the store's clock.
	before := time.Now()
	got, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPlanned}, store.MissionPreparing, "sys", "claim",
		store.MissionUpdate{Claim: &store.MissionClaim{Owner: w1.Owner, TTL: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 1 || got.LeaseOwner != "worker-1" || got.LeaseExpiresAt == nil ||
		got.LeaseExpiresAt.Before(before.Add(time.Minute-5*time.Second)) || got.LeaseExpiresAt.After(time.Now().Add(time.Minute+5*time.Second)) {
		t.Fatalf("claim state: attempt=%d owner=%q until=%v", got.Attempt, got.LeaseOwner, got.LeaseExpiresAt)
	}
	// Recovery may only take a mission whose lease has expired, judged under
	// the row lock by the store's clock.
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPreparing}, store.MissionPlanned, "sys", "",
		store.MissionUpdate{Fence: &w1, ClearLease: true, RequireLeaseExpired: true}); !errors.Is(err, store.ErrMissionStateConflict) {
		t.Fatalf("requeue of a live lease: want conflict, got %v", err)
	}

	// Fenced transitions: the holder passes, anyone else is refused with a
	// lease-lost conflict and nothing changes.
	stale := store.MissionFence{Owner: "worker-2", Attempt: 1}
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPreparing}, store.MissionRunning, "sys", "", store.MissionUpdate{Fence: &stale}); !errors.Is(err, store.ErrMissionLeaseLost) || !errors.Is(err, store.ErrMissionStateConflict) {
		t.Fatalf("stale fence: want lease lost + conflict, got %v", err)
	}
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPreparing}, store.MissionRunning, "sys", "", store.MissionUpdate{Fence: &store.MissionFence{Owner: "worker-1", Attempt: 2}}); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("wrong attempt fence: want lease lost, got %v", err)
	}
	if g, _ := ms.GetMission(ctxA, m.ID); g.Status != store.MissionPreparing {
		t.Fatalf("refused transition changed status to %s", g.Status)
	}
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPreparing}, store.MissionRunning, "sys", "", store.MissionUpdate{Fence: &w1}); err != nil {
		t.Fatal(err)
	}

	// Lease renewal: only the holder, only while active.
	if err := ms.RenewMissionLease(ctxA, m.ID, w1, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := ms.RenewMissionLease(ctxA, m.ID, stale, 2*time.Minute); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("stale renew: %v", err)
	}
	if err := ms.RenewMissionLease(ctxB, m.ID, w1, 2*time.Minute); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("cross-tenant renew: %v", err)
	}

	// Write-ahead receipts: fenced insert, idempotent duplicate, completion.
	r := store.MissionReceipt{MissionID: m.ID, Attempt: 1, Seq: 1, Tool: "write_file", ActionClass: "workspace_write", Status: store.ReceiptStarted, ArgsDigest: "abc"}
	if err := ms.BeginMissionReceipt(ctxA, r, w1); err != nil {
		t.Fatal(err)
	}
	if err := ms.BeginMissionReceipt(ctxA, r, w1); err != nil {
		t.Fatalf("duplicate receipt must be a no-op, got %v", err)
	}
	if err := ms.BeginMissionReceipt(ctxA, store.MissionReceipt{MissionID: m.ID, Attempt: 1, Seq: 2, Tool: "exec", ActionClass: "exec", Status: store.ReceiptStarted, ArgsDigest: "x"}, stale); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("stale receipt: want lease lost, got %v", err)
	}
	if err := ms.BeginMissionReceipt(ctxB, store.MissionReceipt{MissionID: m.ID, Attempt: 1, Seq: 3, Tool: "exec", ActionClass: "exec", Status: store.ReceiptStarted, ArgsDigest: "x"}, w1); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("cross-tenant receipt: want lease lost, got %v", err)
	}
	if err := ms.BeginMissionReceipt(ctxA, store.MissionReceipt{MissionID: m.ID, Attempt: 1, Seq: 2, Tool: "message", ActionClass: "external", Status: store.ReceiptDenied, Reason: "not allowed", ArgsDigest: "y"}, w1); err != nil {
		t.Fatal(err)
	}
	if err := ms.CompleteMissionReceipt(ctxA, m.ID, 1, 1, store.ReceiptOK, 42); err != nil {
		t.Fatal(err)
	}
	// A denied receipt can never be turned into a completed one.
	if err := ms.CompleteMissionReceipt(ctxA, m.ID, 1, 2, store.ReceiptOK, 1); err != nil {
		t.Fatal(err)
	}
	recs, err := ms.ListMissionReceipts(ctxA, m.ID)
	if err != nil || len(recs) != 2 {
		t.Fatalf("receipts: %+v %v", recs, err)
	}
	if recs[0].Seq != 1 || recs[0].Status != store.ReceiptOK || recs[0].DurationMS != 42 || recs[0].Tool != "write_file" {
		t.Fatalf("receipt 1: %+v", recs[0])
	}
	if recs[1].Status != store.ReceiptDenied || recs[1].Reason != "not allowed" {
		t.Fatalf("receipt 2: %+v", recs[1])
	}
	if other, _ := ms.ListMissionReceipts(ctxB, m.ID); len(other) != 0 {
		t.Fatal("cross-tenant receipts visible")
	}

	// Receipts are only accepted while the attempt is running: nothing may
	// run once verification has started.
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionRunning}, store.MissionVerifying, "sys", "", store.MissionUpdate{Fence: &w1}); err != nil {
		t.Fatal(err)
	}
	if err := ms.BeginMissionReceipt(ctxA, store.MissionReceipt{MissionID: m.ID, Attempt: 1, Seq: 7, Tool: "exec", ActionClass: "exec", Status: store.ReceiptStarted, ArgsDigest: "v"}, w1); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("receipt while verifying: want refusal, got %v", err)
	}
	// Even a duplicate of an existing receipt is refused once it may no longer run.
	if err := ms.BeginMissionReceipt(ctxA, r, w1); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("duplicate receipt after the run: want refusal, got %v", err)
	}
	// A receipt for another attempt than the fence is refused.
	if err := ms.BeginMissionReceipt(ctxA, store.MissionReceipt{MissionID: m.ID, Attempt: 2, Seq: 1, Tool: "exec", ActionClass: "exec", Status: store.ReceiptStarted, ArgsDigest: "v"}, w1); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("receipt attempt differs from fence: %v", err)
	}
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionVerifying}, store.MissionRunning, "sys", "", store.MissionUpdate{Fence: &w1}); err != nil {
		t.Fatal(err)
	}

	// Requeue after a lost worker: fenced by the stale holder, lease cleared.
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionRunning}, store.MissionPlanned, "sys", "requeue",
		store.MissionUpdate{Fence: &w1, ClearLease: true}); err != nil {
		t.Fatal(err)
	}
	if err := ms.RenewMissionLease(ctxA, m.ID, w1, 3*time.Minute); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("renew after requeue: %v", err)
	}
	if err := ms.BeginMissionReceipt(ctxA, store.MissionReceipt{MissionID: m.ID, Attempt: 1, Seq: 9, Tool: "exec", ActionClass: "exec", Status: store.ReceiptStarted, ArgsDigest: "z"}, w1); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("receipt after requeue: %v", err)
	}
	w2 := store.MissionFence{Owner: "worker-2", Attempt: 2}
	got, err = ms.TransitionMission(ctxA, m.ID, []string{store.MissionPlanned}, store.MissionPreparing, "sys", "claim",
		store.MissionUpdate{Claim: &store.MissionClaim{Owner: w2.Owner, TTL: 50 * time.Millisecond}})
	if err != nil || got.Attempt != 2 || got.LeaseOwner != "worker-2" {
		t.Fatalf("second claim: %+v %v", got, err)
	}
	// Once the short lease has run out, an expiry-conditioned requeue works.
	time.Sleep(150 * time.Millisecond)
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPreparing}, store.MissionPreparing, "sys", "",
		store.MissionUpdate{Fence: &w2, RequireLeaseExpired: true}); err != nil {
		t.Fatalf("expired lease not recognised: %v", err)
	}
	// The old holder's attempt-1 fence no longer works, even for the same owner name.
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPreparing}, store.MissionFailed, "sys", "", store.MissionUpdate{Fence: &w1}); !errors.Is(err, store.ErrMissionLeaseLost) {
		t.Fatalf("attempt-1 fence after attempt 2 claim: %v", err)
	}
	// Attempts are bounded: back to planned, a third claim is refused.
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPreparing}, store.MissionPlanned, "sys", "", store.MissionUpdate{Fence: &w2, ClearLease: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.TransitionMission(ctxA, m.ID, []string{store.MissionPlanned}, store.MissionPreparing, "sys", "claim",
		store.MissionUpdate{Claim: &store.MissionClaim{Owner: "worker-3", TTL: time.Minute}}); !errors.Is(err, store.ErrMissionStateConflict) || !errors.Is(err, store.ErrMissionNoAttempts) {
		t.Fatalf("claim beyond max attempts: want conflict, got %v", err)
	}
	// Terminal transition with ClearLease leaves no lease behind.
	incomplete := true
	got, err = ms.TransitionMission(ctxA, m.ID, []string{store.MissionPlanned}, store.MissionFailed, "sys", "exhausted",
		store.MissionUpdate{ClearLease: true, UsageIncomplete: &incomplete})
	if err != nil || got.LeaseOwner != "" || got.LeaseExpiresAt != nil || !got.UsageIncomplete || got.Attempt != 2 {
		t.Fatalf("terminal state: %+v %v", got, err)
	}
}
