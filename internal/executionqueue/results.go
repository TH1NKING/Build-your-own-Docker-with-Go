package executionqueue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
	"github.com/jackc/pgx/v5"
)

// The Supervisor allows 8 MiB per output stream and 32 MiB extracted data.
// JSON may escape each captured byte into six bytes and base64 encodes files.
const MaximumReportBytes int64 = 162 << 20

func (s *Store) Authenticate(ctx context.Context, token string) error {
	_, err := workercredential.NewStore(s.databaseURL).Authenticate(ctx, token)
	return err
}

func (s *Store) Validate(ctx context.Context, token, id string, generation int64) error {
	return s.transition(ctx, token, id, generation, nil)
}
func (s *Store) Complete(ctx context.Context, token, id string, generation int64, result sandboxsupervisor.ExecutePythonResult) error {
	if result.ExecutionID != id || !validResult(result) {
		return ErrInvalid
	}
	return s.transition(ctx, token, id, generation, &result)
}

func (s *Store) transition(ctx context.Context, token, id string, generation int64, result *sandboxsupervisor.ExecutePythonResult) error {
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
	var owner, state string
	var current int64
	var expires time.Time
	var released bool
	var stored, workload []byte
	err = tx.QueryRow(ctx, `SELECT COALESCE(worker_id,''),state,generation,COALESCE(lease_expires_at,'epoch'::timestamptz),result,workload,capacity_released FROM control_plane.executions WHERE id=$1 FOR UPDATE`, id).Scan(&owner, &state, &current, &expires, &stored, &workload, &released)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLease
	}
	if err != nil {
		return databaseError(ctx)
	}
	if owner != worker.ID || generation != current {
		return ErrLease
	}
	var now time.Time
	if tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now) != nil {
		return databaseError(ctx)
	}
	if !worker.ExpiresAt.After(now) {
		return workercredential.ErrUnauthenticated
	}
	if result == nil {
		if state != "leased" || released || !expires.After(now) {
			return ErrLease
		}
		return nil
	}
	var e Execution
	if json.Unmarshal(workload, &e) != nil {
		return databaseError(ctx)
	}
	if result.OutputError != "" {
		if len(e.OutputPaths) == 0 {
			return ErrInvalid
		}
	} else {
		if len(result.Outputs) != len(e.OutputPaths) {
			return ErrInvalid
		}
		for i, o := range result.Outputs {
			if o.Path != e.OutputPaths[i] {
				return ErrInvalid
			}
		}
	}
	raw, err := json.Marshal(result)
	if err != nil || int64(len(raw)) > MaximumReportBytes-1024 {
		return ErrInvalid
	}
	if state == "completed" {
		// Both requests are decoded into the same typed value and re-encoded,
		// so whitespace/key order cannot change the immutable result identity.
		if !bytes.Equal(stored, raw) {
			return ErrConflict
		}
		return nil
	}
	if state != "leased" || released || !expires.After(now) {
		return ErrLease
	}
	tag, err := tx.Exec(ctx, `UPDATE control_plane.executions SET state='completed',result=$4 WHERE id=$1 AND worker_id=$2 AND generation=$3 AND state='leased' AND NOT capacity_released AND lease_expires_at>clock_timestamp() AND $5::timestamptz>clock_timestamp()`, id, worker.ID, generation, raw, worker.ExpiresAt)
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

func validResult(r sandboxsupervisor.ExecutePythonResult) bool {
	// Invalid UTF-8 captured by the Supervisor expands to U+FFFD on JSON decode.
	if len(r.Stdout) > 24<<20 || len(r.Stderr) > 24<<20 || r.ExitCode < -1 || r.ExitCode > 255 || len(r.Outputs) > 16 {
		return false
	}
	if r.Truncated != (r.StdoutTruncated || r.StderrTruncated) {
		return false
	}
	switch r.TerminalReason {
	case sandboxsupervisor.ExecutionExited, sandboxsupervisor.ExecutionMemoryLimit, sandboxsupervisor.ExecutionPIDLimit, sandboxsupervisor.ExecutionTimedOut:
	default:
		return false
	}
	switch r.OutputError {
	case "":
	case sandboxsupervisor.OutputUnsafe, sandboxsupervisor.OutputLimit, sandboxsupervisor.OutputUnavailable:
		if len(r.Outputs) != 0 {
			return false
		}
	default:
		return false
	}
	var total int64
	for _, o := range r.Outputs {
		if o.Size < 0 || o.Size > 32<<20 || o.Size != int64(len(o.Content)) || o.SHA256 != fmt.Sprintf("%x", sha256.Sum256(o.Content)) {
			return false
		}
		total += o.Size
		if total > 32<<20 {
			return false
		}
	}
	return true
}
