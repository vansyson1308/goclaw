DROP TABLE IF EXISTS agent_evolution_events;
ALTER TABLE agent_evolution_suggestions
    DROP COLUMN IF EXISTS applied_at,
    DROP COLUMN IF EXISTS applied_by,
    DROP COLUMN IF EXISTS rolled_back_at,
    DROP COLUMN IF EXISTS rolled_back_by,
    DROP COLUMN IF EXISTS applied_change,
    DROP COLUMN IF EXISTS state_version;
