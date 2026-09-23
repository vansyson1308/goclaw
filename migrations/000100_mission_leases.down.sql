DROP TABLE IF EXISTS mission_receipts;
ALTER TABLE missions
    DROP COLUMN IF EXISTS pins,
    DROP COLUMN IF EXISTS usage_incomplete,
    DROP COLUMN IF EXISTS lease_expires_at,
    DROP COLUMN IF EXISTS lease_owner,
    DROP COLUMN IF EXISTS max_attempts,
    DROP COLUMN IF EXISTS attempt;
