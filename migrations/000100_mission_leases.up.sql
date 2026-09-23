-- Missions phase D: attempts, lease-based fencing and write-ahead tool receipts
-- (docs/mission-control/MISSIONS.md "Durability").
ALTER TABLE missions
    ADD COLUMN IF NOT EXISTS attempt          INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_attempts     INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS lease_owner      TEXT,
    ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS usage_incomplete BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS mission_receipts (
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    mission_id   UUID NOT NULL REFERENCES missions(id) ON DELETE CASCADE,
    attempt      INTEGER NOT NULL,
    seq          INTEGER NOT NULL,
    tool         TEXT NOT NULL,
    action_class TEXT NOT NULL,
    status       TEXT NOT NULL,
    reason       TEXT,
    args_digest  TEXT NOT NULL,
    duration_ms  BIGINT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (mission_id, attempt, seq),
    CONSTRAINT chk_mission_receipts_status CHECK (status IN ('denied','started','ok','error'))
);

CREATE INDEX IF NOT EXISTS idx_mission_receipts_tenant ON mission_receipts(tenant_id);
