package executionqueue

import (
	"context"
	"errors"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
	"github.com/jackc/pgx/v5"
)

// BindSandbox persists the private Sandbox identity before creation can have
// local effects. A lost acknowledgement can only retry the same binding.
func (s *Store) BindSandbox(ctx context.Context, token, id string, generation int64, sandboxID string) error {
	if !reference.MatchString(id) || generation <= 0 || !reference.MatchString(sandboxID) {
		return ErrInvalid
	}
	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConnection(conn)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return databaseError(ctx)
	}
	defer rollback(tx)
	worker, err := workercredential.AuthenticateTx(ctx, tx, token)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE control_plane.executions SET sandbox_id=$4 WHERE id=$1 AND worker_id=$2 AND generation=$3 AND state='leased' AND NOT capacity_released AND NOT cleanup_unknown AND lease_expires_at>clock_timestamp() AND $5::timestamptz>clock_timestamp() AND (sandbox_id='' OR sandbox_id=$4)`, id, worker.ID, generation, sandboxID, worker.ExpiresAt)
	if err != nil {
		return databaseError(ctx)
	}
	if tag.RowsAffected() != 1 {
		return ErrLease
	}
	if tx.Commit(ctx) != nil {
		return databaseError(ctx)
	}
	return nil
}

// SweepRecovery durably advances a bounded batch without granting execution
// rights or reclaiming capacity. Deadlines are the ones persisted with a lease.
func (s *Store) SweepRecovery(ctx context.Context) error {
	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConnection(conn)
	_, err = conn.Exec(ctx, `WITH due AS (SELECT id FROM control_plane.executions WHERE (state='leased' AND lease_expires_at<=clock_timestamp()) OR (state='recovering' AND recovery_until<=clock_timestamp()) ORDER BY lease_expires_at FOR UPDATE SKIP LOCKED LIMIT 256) UPDATE control_plane.executions AS e SET state=CASE WHEN e.recovery_until<=clock_timestamp() THEN 'worker_lost' ELSE 'recovering' END FROM due WHERE e.id=due.id`)
	if err != nil {
		return databaseError(ctx)
	}
	return nil
}

// Outstanding returns a bounded inventory for cleanup, never Workloads that
// another Worker process could mistake for permission to execute again.
func (s *Store) Outstanding(ctx context.Context, token string) ([]OutstandingExecution, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConnection(conn)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, databaseError(ctx)
	}
	defer rollback(tx)
	worker, err := workercredential.AuthenticateTx(ctx, tx, token)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id,agent_run_id,generation,lease_expires_at,sandbox_id,CASE WHEN state IN ('leased','recovering') AND recovery_until<=clock_timestamp() THEN 'worker_lost' WHEN state='leased' AND lease_expires_at<=clock_timestamp() THEN 'recovering' ELSE state END,cleanup_unknown FROM control_plane.executions WHERE worker_id=$1 AND NOT capacity_released ORDER BY queued_order LIMIT 64`, worker.ID)
	if err != nil {
		return nil, databaseError(ctx)
	}
	defer rows.Close()
	entries := make([]OutstandingExecution, 0)
	for rows.Next() {
		var entry OutstandingExecution
		if rows.Scan(&entry.Lease.ExecutionID, &entry.Lease.AgentRunID, &entry.Lease.Generation, &entry.Lease.ExpiresAt, &entry.SandboxID, &entry.State, &entry.CleanupUnknown) != nil {
			return nil, databaseError(ctx)
		}
		entries = append(entries, entry)
	}
	if rows.Err() != nil {
		return nil, databaseError(ctx)
	}
	return entries, nil
}

func (s *Store) Heartbeat(ctx context.Context, token, id string, generation int64, sandboxID string) (Authority, error) {
	return s.renew(ctx, token, id, generation, sandboxID, false)
}

func (s *Store) Recover(ctx context.Context, token, id string, generation int64, sandboxID string) (Authority, error) {
	return s.renew(ctx, token, id, generation, sandboxID, true)
}

func (s *Store) renew(ctx context.Context, token, id string, generation int64, sandboxID string, recovering bool) (Authority, error) {
	var authority Authority
	if !reference.MatchString(id) || generation <= 0 || !reference.MatchString(sandboxID) {
		return authority, ErrInvalid
	}
	conn, err := s.connect(ctx)
	if err != nil {
		return authority, err
	}
	defer closeConnection(conn)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return authority, databaseError(ctx)
	}
	defer rollback(tx)
	worker, err := workercredential.AuthenticateTx(ctx, tx, token)
	if err != nil {
		return authority, err
	}
	// Completion may have committed while its HTTPS acknowledgement was lost.
	// Monitoring may observe it, but must not renew or overwrite that outcome.
	err = tx.QueryRow(ctx, `SELECT id,agent_run_id,generation,lease_expires_at,recovery_until,clock_timestamp() FROM control_plane.executions WHERE id=$1 AND worker_id=$2 AND generation=$3 AND sandbox_id=$4 AND state='completed' AND $5::timestamptz>clock_timestamp()`, id, worker.ID, generation, sandboxID, worker.ExpiresAt).Scan(&authority.Lease.ExecutionID, &authority.Lease.AgentRunID, &authority.Lease.Generation, &authority.Lease.ExpiresAt, &authority.RecoveryUntil, &authority.ServerTime)
	if err == nil {
		authority.State = "completed"
		return authority, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Authority{}, databaseError(ctx)
	}
	err = tx.QueryRow(ctx, `UPDATE control_plane.executions SET state='leased',lease_expires_at=clock_timestamp()+$5::bigint*interval '1 millisecond',recovery_until=clock_timestamp()+$6::bigint*interval '1 millisecond' WHERE id=$1 AND worker_id=$2 AND generation=$3 AND sandbox_id=$4 AND NOT capacity_released AND ((NOT $8 AND state='leased' AND lease_expires_at>clock_timestamp()) OR ($8 AND state IN ('leased','recovering') AND recovery_until>clock_timestamp())) AND $7::timestamptz>clock_timestamp() RETURNING id,agent_run_id,generation,lease_expires_at,recovery_until,clock_timestamp()`, id, worker.ID, generation, sandboxID, s.leaseDuration.Milliseconds(), (s.leaseDuration+s.recoveryWindow).Milliseconds(), worker.ExpiresAt, recovering).Scan(&authority.Lease.ExecutionID, &authority.Lease.AgentRunID, &authority.Lease.Generation, &authority.Lease.ExpiresAt, &authority.RecoveryUntil, &authority.ServerTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return Authority{}, ErrLease
	}
	if err != nil {
		return Authority{}, databaseError(ctx)
	}
	authority.State = "leased"
	if tx.Commit(ctx) != nil {
		return Authority{}, databaseError(ctx)
	}
	return authority, nil
}
