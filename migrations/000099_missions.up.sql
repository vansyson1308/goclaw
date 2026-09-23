-- Missions: durable, verifiable units of agent work (docs/mission-control/MISSIONS.md).
CREATE TABLE IF NOT EXISTS missions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    owner_id        TEXT NOT NULL,
    agent_key       TEXT NOT NULL,
    title           TEXT NOT NULL,
    contract        JSONB NOT NULL,
    contract_digest TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'planned',
    status_reason   TEXT,
    executor        TEXT,
    workspace_path  TEXT,
    base_revision   TEXT,
    verification    JSONB,
    diff            TEXT,
    diff_truncated  BOOLEAN NOT NULL DEFAULT false,
    changed_files   JSONB,
    summary         TEXT,
    input_tokens    BIGINT NOT NULL DEFAULT 0,
    output_tokens   BIGINT NOT NULL DEFAULT 0,
    cost_usd        NUMERIC(14, 6),
    iterations      INTEGER NOT NULL DEFAULT 0,
    state_version   INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    CONSTRAINT chk_missions_status CHECK (status IN
        ('planned','preparing','running','verifying','succeeded','partial','failed','blocked','cancelled'))
);

CREATE INDEX IF NOT EXISTS idx_missions_tenant_created ON missions(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_missions_active ON missions(status) WHERE status IN ('planned','preparing','running','verifying');

CREATE TABLE IF NOT EXISTS mission_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    mission_id  UUID NOT NULL REFERENCES missions(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    from_status TEXT,
    to_status   TEXT,
    actor       TEXT NOT NULL,
    message     TEXT,
    detail      JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS idx_mission_events_mission ON mission_events(mission_id, created_at);
