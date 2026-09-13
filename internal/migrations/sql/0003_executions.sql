-- T24's first tracer has one Execution per Agent Run. T28 owns multi-step runs.
CREATE TABLE control_plane.executions (
    id text PRIMARY KEY,
    agent_run_id text NOT NULL UNIQUE,
    queued_order bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    -- Canonical JSON bytes preserve NUL stdin/stdout; PostgreSQL jsonb rejects it.
    workload bytea NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'leased', 'completed')),
    worker_id text REFERENCES control_plane.worker_nodes(id),
    generation bigint NOT NULL DEFAULT 0 CHECK (generation >= 0),
    lease_expires_at timestamptz,
    result bytea,
    CHECK ((state = 'queued' AND worker_id IS NULL AND generation = 0 AND lease_expires_at IS NULL AND result IS NULL)
        OR (state = 'leased' AND worker_id IS NOT NULL AND generation > 0 AND lease_expires_at IS NOT NULL AND result IS NULL)
        OR (state = 'completed' AND worker_id IS NOT NULL AND generation > 0 AND lease_expires_at IS NOT NULL AND result IS NOT NULL))
);
CREATE INDEX executions_pending ON control_plane.executions(queued_order) WHERE state = 'queued';
