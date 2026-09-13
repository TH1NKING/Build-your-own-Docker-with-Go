CREATE TABLE control_plane.worker_nodes (
    id text PRIMARY KEY CHECK (id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
    credential_sha256 bytea NOT NULL UNIQUE CHECK (octet_length(credential_sha256) = 32),
    credential_generation bigint NOT NULL DEFAULT 1 CHECK (credential_generation > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
