package executionqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Store struct {
	databaseURL   string
	leaseDuration time.Duration
}

func NewStore(databaseURL string, leaseDuration time.Duration) (*Store, error) {
	if leaseDuration == 0 {
		leaseDuration = 2 * time.Minute
	}
	if leaseDuration < 100*time.Millisecond || leaseDuration > time.Hour {
		return nil, errors.New("lease duration must be between 100ms and 1h")
	}
	if _, err := pgx.ParseConfig(databaseURL); err != nil || databaseURL == "" {
		return nil, errors.New("invalid Control Plane database configuration")
	}
	return &Store{databaseURL: databaseURL, leaseDuration: leaseDuration}, nil
}

func (s *Store) connect(ctx context.Context) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(s.databaseURL)
	if err != nil {
		return nil, databaseError(ctx)
	}
	cfg.ConnectTimeout = 5 * time.Second
	cfg.RuntimeParams["application_name"] = "execution-queue"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, databaseError(ctx)
	}
	return conn, nil
}

func closeConnection(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func databaseError(ctx context.Context) error {
	if ctx.Err() != nil {
		return fmt.Errorf("Execution database operation: %w", ctx.Err())
	}
	return errors.New("Execution database operation failed; check Control Plane database health")
}

var reference = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func validExecution(e Execution) bool {
	if !reference.MatchString(e.ExecutionID) || !reference.MatchString(e.AgentRunID) || len(e.Source) == 0 || len(e.Source) > 32<<10 || strings.ContainsRune(e.Source, 0) || len(e.Stdin) > 8<<10 || len(e.OutputPaths) > 16 {
		return false
	}
	seen := make(map[string]bool)
	for _, p := range e.OutputPaths {
		if p == "" || len(p) > 1024 || p == "." || strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.ContainsAny(p, "\\\x00") || p == ".." || strings.HasPrefix(p, "../") || seen[p] {
			return false
		}
		seen[p] = true
	}
	// Respect the Supervisor's existing 64 KiB request envelope ceiling even
	// when JSON escaping expands Python, stdin, or declared paths.
	raw, err := json.Marshal(e)
	return err == nil && len(raw) <= 60<<10
}

// Enqueue is the trusted setup/dispatch boundary for future Tool Call admission.
// It is deliberately not exposed on the Worker HTTP listener.
func (s *Store) Enqueue(ctx context.Context, e Execution) error {
	if !validExecution(e) {
		return ErrInvalid
	}
	raw, _ := json.Marshal(e)
	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConnection(conn)
	_, err = conn.Exec(ctx, `INSERT INTO control_plane.executions(id,agent_run_id,workload) VALUES($1,$2,$3)`, e.ExecutionID, e.AgentRunID, raw)
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return ErrConflict
		}
		return databaseError(ctx)
	}
	return nil
}

// Claim holds the authenticated Worker row until its Execution transition
// commits, so a successful credential revocation orders before or after it.
func (s *Store) Claim(ctx context.Context, token string) (*Lease, error) {
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
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM control_plane.executions WHERE state='queued' ORDER BY queued_order FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, databaseError(ctx)
	}
	var raw []byte
	var lease Lease
	err = tx.QueryRow(ctx, `UPDATE control_plane.executions SET state='leased',worker_id=$2,generation=generation+1,lease_expires_at=clock_timestamp()+$3::bigint*interval '1 millisecond' WHERE id=$1 AND state='queued' AND $4::timestamptz>clock_timestamp() RETURNING workload,generation,lease_expires_at`, id, worker.ID, s.leaseDuration.Milliseconds(), worker.ExpiresAt).Scan(&raw, &lease.Generation, &lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, workercredential.ErrUnauthenticated
	}
	if err != nil {
		return nil, databaseError(ctx)
	}
	var e Execution
	if json.Unmarshal(raw, &e) != nil {
		return nil, databaseError(ctx)
	}
	lease.ExecutionID = e.ExecutionID
	lease.AgentRunID = e.AgentRunID
	lease.Source = e.Source
	lease.Stdin = e.Stdin
	lease.OutputPaths = e.OutputPaths
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(ctx)
	}
	return &lease, nil
}

// Get exposes immutable domain outcomes to the trusted Control Plane. Expiry
// is visible but never requeues uncertain work; T31 owns recovery transitions.
func (s *Store) Get(ctx context.Context, id string) (Record, error) {
	var r Record
	conn, err := s.connect(ctx)
	if err != nil {
		return r, err
	}
	defer closeConnection(conn)
	var workload, result []byte
	err = conn.QueryRow(ctx, `SELECT workload,CASE WHEN state='leased' AND lease_expires_at<=clock_timestamp() THEN 'lease_expired' ELSE state END,COALESCE(worker_id,''),generation,lease_expires_at,result FROM control_plane.executions WHERE id=$1`, id).Scan(&workload, &r.State, &r.WorkerID, &r.Generation, &r.ExpiresAt, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, databaseError(ctx)
	}
	if json.Unmarshal(workload, &r.Execution) != nil {
		return r, databaseError(ctx)
	}
	if result != nil && json.Unmarshal(result, &r.Result) != nil {
		return r, databaseError(ctx)
	}
	return r, nil
}
