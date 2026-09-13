package executionqueue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConfirmLegacyCleanup is a trusted administration boundary, never a Worker API
// operation. The Platform Operator must first stop and clean the old execution
// plane. Revocation prevents a live credential from racing that confirmation.
func (s *Store) ConfirmLegacyCleanup(ctx context.Context, workerID string) (int64, error) {
	if workerID == "" {
		return 0, ErrInvalid
	}
	conn, err := s.connect(ctx)
	if err != nil {
		return 0, err
	}
	defer closeConnection(conn)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, databaseError(ctx)
	}
	defer rollback(tx)
	var revokedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT revoked_at FROM control_plane.worker_nodes WHERE id=$1 FOR UPDATE`, workerID).Scan(&revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, databaseError(ctx)
	}
	if revokedAt == nil {
		return 0, fmt.Errorf("Worker must be revoked before confirming legacy cleanup: %w", ErrConflict)
	}
	tag, err := tx.Exec(ctx, `UPDATE control_plane.executions SET capacity_released=true,cleanup_unknown=false,state=CASE WHEN state IN ('leased','recovering') THEN 'worker_lost' ELSE state END WHERE worker_id=$1 AND cleanup_unknown`, workerID)
	if err != nil {
		return 0, databaseError(ctx)
	}
	if tx.Commit(ctx) != nil {
		return 0, databaseError(ctx)
	}
	return tag.RowsAffected(), nil
}
