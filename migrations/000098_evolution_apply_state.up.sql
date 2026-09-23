-- Transactional apply/rollback bookkeeping for agent evolution suggestions.
-- Additive only (expand phase): existing rows keep working; legacy rows that
-- stored a rollback baseline in parameters._baseline stay readable.
ALTER TABLE agent_evolution_suggestions
    ADD COLUMN IF NOT EXISTS applied_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS applied_by     TEXT,
    ADD COLUMN IF NOT EXISTS rolled_back_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS rolled_back_by TEXT,
    ADD COLUMN IF NOT EXISTS applied_change JSONB,
    ADD COLUMN IF NOT EXISTS state_version  INTEGER NOT NULL DEFAULT 0;

-- Append-only audit trail of every suggestion transition.
CREATE TABLE IF NOT EXISTS agent_evolution_events (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id),
    suggestion_id UUID NOT NULL REFERENCES agent_evolution_suggestions(id) ON DELETE CASCADE,
    agent_id      UUID NOT NULL,
    action        TEXT NOT NULL,
    from_status   TEXT NOT NULL,
    to_status     TEXT NOT NULL,
    actor         TEXT NOT NULL,
    detail        JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_evo_events_suggestion ON agent_evolution_events(suggestion_id, created_at);
CREATE INDEX IF NOT EXISTS idx_evo_events_tenant ON agent_evolution_events(tenant_id);
