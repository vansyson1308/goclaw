package cmd

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// evolutionCronLockID is a PG advisory lock ID for preventing duplicate cron runs.
// Only one gateway instance should run evolution analysis at a time.
const evolutionCronLockID int64 = 0x65766F6C // "evol"

// evolutionRunHours defines when analysis runs each day (server local time).
var evolutionRunHours = []int{3, 9, 15, 21}

// runEvolutionCron runs the v3 evolution suggestion engine (every 6 hours at
// 3:00/9:00/15:00/21:00 server-local time) as a background goroutine.
// Designed to be called with `go runEvolutionCron(...)`.
//
// There is no automatic metric-driven rollback: the only auto-applied type
// used to be "threshold", whose metric could not justify the change (see
// docs/mission-control/DECISIONS.md D5). Applied changes are rolled back
// explicitly via the API, which restores the exact prior state.
func runEvolutionCron(stores *store.Stores, engine *agent.SuggestionEngine) {
	// Wait 1 minute after startup for warm-up, then run first analysis.
	time.Sleep(1 * time.Minute)
	runSuggestionAnalysis(stores, engine)

	for {
		next := nextScheduledRun(evolutionRunHours)

		timer := time.NewTimer(time.Until(next))
		<-timer.C
		timer.Stop()

		runSuggestionAnalysis(stores, engine)
	}
}

// nextScheduledRun returns the earliest future time matching one of the given hours.
// Uses AddDate to handle DST transitions correctly (calendar day, not 24h).
func nextScheduledRun(hours []int) time.Time {
	now := time.Now()
	var best time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !now.Before(t) {
			t = t.AddDate(0, 0, 1) // already passed today, try tomorrow
		}
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	return best
}

// tryAdvisoryLock acquires a PG session-level advisory lock on a pinned connection.
// Returns the pinned connection (caller must close) and whether the lock was acquired.
// No-op (returns nil, true) when db is nil (SQLite/desktop edition runs single-instance).
func tryAdvisoryLock(ctx context.Context, db *sql.DB) (*sql.Conn, bool) {
	if db == nil {
		return nil, true
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		slog.Debug("evolution.cron.conn_failed", "error", err)
		return nil, false
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", evolutionCronLockID).Scan(&acquired); err != nil {
		conn.Close()
		slog.Debug("evolution.cron.advisory_lock_failed", "error", err)
		return nil, false
	}
	if !acquired {
		conn.Close()
		return nil, false
	}
	return conn, true
}

// releaseAdvisoryLock releases a PG advisory lock on the pinned connection and closes it.
func releaseAdvisoryLock(ctx context.Context, conn *sql.Conn) {
	if conn == nil {
		return
	}
	_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", evolutionCronLockID)
	conn.Close()
}

// runSuggestionAnalysis lists agents with evolution enabled and runs analysis.
// Agents are listed per tenant: a bare-context Agents.List fails closed and
// returns nothing, which silently disabled this job.
func runSuggestionAnalysis(stores *store.Stores, engine *agent.SuggestionEngine) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Acquire advisory lock on a pinned connection to prevent duplicate runs across instances.
	conn, acquired := tryAdvisoryLock(ctx, stores.DB)
	if !acquired {
		slog.Debug("evolution.cron.skipped_lock_held")
		return
	}
	defer releaseAdvisoryLock(ctx, conn)

	agents, err := agent.ListEvolutionAgents(ctx, stores.Tenants, stores.Agents)
	if err != nil {
		slog.Warn("evolution.cron.list_agents_failed", "error", err)
		return
	}

	var count int
	for _, ag := range agents {
		agentCtx := store.WithTenantID(ctx, ag.TenantID)
		if _, err := engine.Analyze(agentCtx, ag.ID); err != nil {
			slog.Debug("evolution.cron.analyze_failed", "agent", ag.ID, "error", err)
		}
		count++
	}

	if count > 0 {
		slog.Info("evolution.cron.analysis_complete", "agents", count)
	}
}
