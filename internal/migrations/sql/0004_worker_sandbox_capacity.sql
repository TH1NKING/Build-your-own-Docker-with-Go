ALTER TABLE control_plane.worker_nodes
    ADD COLUMN sandbox_capacity integer NOT NULL DEFAULT 2 CHECK (sandbox_capacity BETWEEN 1 AND 64);

ALTER TABLE control_plane.executions
    ADD COLUMN capacity_released boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT executions_capacity_requires_owner CHECK (NOT capacity_released OR worker_id IS NOT NULL);

CREATE INDEX executions_occupied_worker ON control_plane.executions(worker_id)
    WHERE worker_id IS NOT NULL AND NOT capacity_released;
