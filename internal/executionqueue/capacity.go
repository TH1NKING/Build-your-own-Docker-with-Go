package executionqueue

import (
	"context"
	"errors"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
	"github.com/jackc/pgx/v5"
)

// Capacity reports reservations retained until the Worker confirms cleanup,
// including completed Executions and expired leases.
func (s *Store) Capacity(ctx context.Context, token string) (Capacity, error) {
	var capacity Capacity
	conn, err := s.connect(ctx)
	if err != nil {
		return capacity, err
	}
	defer closeConnection(conn)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return capacity, databaseError(ctx)
	}
	defer rollback(tx)
	worker, err := workercredential.AuthenticateTx(ctx, tx, token)
	if err != nil {
		return capacity, err
	}
	capacity.WorkerID = worker.ID
	if err := tx.QueryRow(ctx, `SELECT sandbox_capacity,(SELECT count(*) FROM control_plane.executions WHERE worker_id=$1 AND NOT capacity_released) FROM control_plane.worker_nodes WHERE id=$1`, worker.ID).Scan(&capacity.Configured, &capacity.Occupied); err != nil {
		return Capacity{}, databaseError(ctx)
	}
	capacity.Available = max(0, capacity.Configured-capacity.Occupied)
	return capacity, nil
}

func (s *Store) ConfigureCapacity(ctx context.Context, token string, capacity int) error {
	if capacity < 1 || capacity > 64 {
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
	var current, occupied int
	if err := tx.QueryRow(ctx, `SELECT sandbox_capacity,(SELECT count(*) FROM control_plane.executions WHERE worker_id=$1 AND NOT capacity_released) FROM control_plane.worker_nodes WHERE id=$1`, worker.ID).Scan(&current, &occupied); err != nil {
		return databaseError(ctx)
	}
	if capacity != current && occupied != 0 {
		return ErrConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE control_plane.worker_nodes SET sandbox_capacity=$2 WHERE id=$1 AND expires_at>clock_timestamp()`, worker.ID, capacity)
	if err != nil {
		return databaseError(ctx)
	}
	if tag.RowsAffected() != 1 {
		return workercredential.ErrUnauthenticated
	}
	if tx.Commit(ctx) != nil {
		return databaseError(ctx)
	}
	return nil
}

// Release acknowledges that the trusted Worker has finished Sandbox cleanup.
// Completion and lease expiry never release capacity on their own.
func (s *Store) Release(ctx context.Context, token, id string, generation int64) error {
	if !reference.MatchString(id) || generation <= 0 {
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
	var owner string
	var current int64
	var cleanupUnknown bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(worker_id,''),generation,cleanup_unknown FROM control_plane.executions WHERE id=$1 FOR UPDATE`, id).Scan(&owner, &current, &cleanupUnknown); errors.Is(err, pgx.ErrNoRows) {
		return ErrLease
	} else if err != nil {
		return databaseError(ctx)
	}
	if owner != worker.ID || current != generation {
		return ErrLease
	}
	if cleanupUnknown {
		return ErrConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE control_plane.executions SET capacity_released=true,state=CASE WHEN state IN ('leased','recovering') THEN 'worker_lost' ELSE state END WHERE id=$1 AND $2::timestamptz>clock_timestamp()`, id, worker.ExpiresAt)
	if err != nil {
		return databaseError(ctx)
	}
	if tag.RowsAffected() != 1 {
		return workercredential.ErrUnauthenticated
	}
	if tx.Commit(ctx) != nil {
		return databaseError(ctx)
	}
	return nil
}
