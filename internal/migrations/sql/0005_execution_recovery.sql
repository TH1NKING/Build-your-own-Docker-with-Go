ALTER TABLE control_plane.executions
    ADD COLUMN sandbox_id text NOT NULL DEFAULT '' CHECK (sandbox_id = '' OR sandbox_id ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    ADD COLUMN recovery_until timestamptz,
    ADD COLUMN cleanup_unknown boolean NOT NULL DEFAULT false;

-- Pre-binding Workers may already have created a Sandbox whose identity was
-- never persisted. An empty ID on these rows is not proof of no local effects.
UPDATE control_plane.executions SET cleanup_unknown=true WHERE worker_id IS NOT NULL AND NOT capacity_released;
ALTER TABLE control_plane.executions ADD CONSTRAINT executions_unknown_cleanup
    CHECK (NOT cleanup_unknown OR (worker_id IS NOT NULL AND NOT capacity_released AND sandbox_id=''));

-- Existing outstanding work keeps its original lease and a finite recovery
-- boundary; deploying a new Control Plane never starts a new recovery window.
UPDATE control_plane.executions SET recovery_until=lease_expires_at+interval '30 seconds' WHERE lease_expires_at IS NOT NULL;
ALTER TABLE control_plane.executions ADD CONSTRAINT executions_recovery_deadline
    CHECK ((lease_expires_at IS NULL AND recovery_until IS NULL)
        OR (lease_expires_at IS NOT NULL AND recovery_until IS NOT NULL AND recovery_until>lease_expires_at));

ALTER TABLE control_plane.executions DROP CONSTRAINT executions_state_check, DROP CONSTRAINT executions_check;
ALTER TABLE control_plane.executions
    ADD CONSTRAINT executions_state_check CHECK (state IN ('queued','leased','recovering','worker_lost','completed')),
    ADD CONSTRAINT executions_owned_state CHECK (
        (state='queued' AND worker_id IS NULL AND generation=0 AND lease_expires_at IS NULL AND result IS NULL)
        OR (state IN ('leased','recovering','worker_lost') AND worker_id IS NOT NULL AND generation>0 AND lease_expires_at IS NOT NULL AND result IS NULL)
        OR (state='completed' AND worker_id IS NOT NULL AND generation>0 AND lease_expires_at IS NOT NULL AND result IS NOT NULL));
CREATE INDEX executions_recovery_due ON control_plane.executions(lease_expires_at,recovery_until)
    WHERE state IN ('leased','recovering');

-- T27 could confirm cleanup without recording an Execution result. Such a
-- Sandbox has already gone and cannot become recoverable after this upgrade.
UPDATE control_plane.executions SET state='worker_lost' WHERE state='leased' AND capacity_released;
