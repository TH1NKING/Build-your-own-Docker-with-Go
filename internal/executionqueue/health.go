package executionqueue

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
)

// CheckReady is a read-only startup gate. It never initializes or migrates a
// database; the Platform Operator must first run the reviewed agentctl migration.
func (s *Store) CheckReady(ctx context.Context) error {
	conn, err := s.connect(ctx)
	if err != nil {
		return errors.New("Control Plane database is unavailable; check its connection configuration")
	}
	defer closeConnection(conn)
	notReady := errors.New("Control Plane database schema is not ready; run agentctl migrate")
	var version int
	if err := conn.QueryRow(ctx, `SELECT COALESCE(MAX(version),0) FROM public.agentctl_schema_migrations`).Scan(&version); err != nil || version < 3 {
		return notReady
	}
	workers, err := conn.Query(ctx, `SELECT credential_sha256,id,credential_generation,created_at,expires_at,revoked_at FROM control_plane.worker_nodes LIMIT 0`)
	if err != nil {
		return notReady
	}
	workerFields := workers.FieldDescriptions()
	workerReady := len(workerFields) == 6 && workerFields[0].DataTypeOID == pgtype.ByteaOID
	workers.Close()
	if !workerReady || workers.Err() != nil {
		return notReady
	}
	executions, err := conn.Query(ctx, `SELECT workload,result,id,agent_run_id,queued_order,state,worker_id,generation,lease_expires_at FROM control_plane.executions LIMIT 0`)
	if err != nil {
		return notReady
	}
	fields := executions.FieldDescriptions()
	ready := len(fields) == 9 && fields[0].DataTypeOID == pgtype.ByteaOID && fields[1].DataTypeOID == pgtype.ByteaOID
	executions.Close()
	if !ready || executions.Err() != nil {
		return notReady
	}
	return nil
}
