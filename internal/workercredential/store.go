// Package workercredential manages identities scoped to the Worker API.
package workercredential

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrUnauthenticated = errors.New("Worker Credential is invalid, expired, or revoked")
	ErrInvalid         = errors.New("invalid Worker Credential request")
	ErrNotFound        = errors.New("Worker Node does not exist")
	ErrConflict        = errors.New("Worker Node already exists")
)

type Worker struct {
	ID         string     `json:"id"`
	Generation int64      `json:"generation"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Issued is returned only by issuance operations; the raw Token is never stored.
type Issued struct {
	Worker Worker `json:"worker"`
	Token  string `json:"token"`
}

// Store opens a separate connection per operation and is safe for concurrent use.
type Store struct{ databaseURL string }

func NewStore(databaseURL string) *Store { return &Store{databaseURL: databaseURL} }

var workerID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (store *Store) Provision(ctx context.Context, id string, expiresAt time.Time) (Issued, error) {
	if !workerID.MatchString(id) || !expiresAt.After(time.Now()) {
		return Issued{}, ErrInvalid
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return Issued{}, errors.New("cannot generate Worker Credential")
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	digest := sha256.Sum256([]byte(token))
	connection, err := store.connect(ctx)
	if err != nil {
		return Issued{}, err
	}
	defer closeConnection(connection)
	tx, err := connection.Begin(ctx)
	if err != nil {
		return Issued{}, databaseError(ctx, err)
	}
	defer rollback(tx)
	var worker Worker
	err = tx.QueryRow(ctx, `INSERT INTO control_plane.worker_nodes (id, credential_sha256, expires_at)
		VALUES ($1,$2,$3) RETURNING id, credential_generation, created_at, expires_at, revoked_at`, id, digest[:], expiresAt).Scan(
		&worker.ID, &worker.Generation, &worker.CreatedAt, &worker.ExpiresAt, &worker.RevokedAt)
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return Issued{}, ErrConflict
		}
		return Issued{}, databaseError(ctx, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Issued{}, databaseError(ctx, err)
	}
	return Issued{Worker: worker, Token: token}, nil
}

func (store *Store) Authenticate(ctx context.Context, token string) (Worker, error) {
	connection, err := store.connect(ctx)
	if err != nil {
		return Worker{}, err
	}
	defer closeConnection(connection)
	tx, err := connection.Begin(ctx)
	if err != nil {
		return Worker{}, databaseError(ctx, err)
	}
	defer rollback(tx)
	worker, err := AuthenticateTx(ctx, tx, token)
	if err != nil {
		return Worker{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Worker{}, databaseError(ctx, err)
	}
	return worker, nil
}

// Rotate atomically replaces only this Worker's verifier, including when its
// previous credential has expired or was revoked by the Platform Operator.
func (store *Store) Rotate(ctx context.Context, id string, expiresAt time.Time) (Issued, error) {
	if !workerID.MatchString(id) || !expiresAt.After(time.Now()) {
		return Issued{}, ErrInvalid
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return Issued{}, errors.New("cannot generate Worker Credential")
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	digest := sha256.Sum256([]byte(token))
	connection, err := store.connect(ctx)
	if err != nil {
		return Issued{}, err
	}
	defer closeConnection(connection)
	tx, err := connection.Begin(ctx)
	if err != nil {
		return Issued{}, databaseError(ctx, err)
	}
	defer rollback(tx)
	var worker Worker
	err = tx.QueryRow(ctx, `UPDATE control_plane.worker_nodes SET credential_sha256=$2,
		credential_generation=credential_generation+1, expires_at=$3, revoked_at=NULL WHERE id=$1
		RETURNING id, credential_generation, created_at, expires_at, revoked_at`, id, digest[:], expiresAt).Scan(
		&worker.ID, &worker.Generation, &worker.CreatedAt, &worker.ExpiresAt, &worker.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Issued{}, ErrNotFound
	}
	if err != nil {
		return Issued{}, databaseError(ctx, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Issued{}, databaseError(ctx, err)
	}
	return Issued{Worker: worker, Token: token}, nil
}

func (store *Store) Revoke(ctx context.Context, id string) error {
	if !workerID.MatchString(id) {
		return ErrInvalid
	}
	connection, err := store.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConnection(connection)
	tx, err := connection.Begin(ctx)
	if err != nil {
		return databaseError(ctx, err)
	}
	defer rollback(tx)
	result, err := tx.Exec(ctx, `UPDATE control_plane.worker_nodes
		SET revoked_at=COALESCE(revoked_at, clock_timestamp()) WHERE id=$1`, id)
	if err != nil {
		return databaseError(ctx, err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(ctx, err)
	}
	return nil
}

// List returns lifecycle metadata only; neither raw values nor verifiers escape.
func (store *Store) List(ctx context.Context) ([]Worker, error) {
	connection, err := store.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConnection(connection)
	rows, err := connection.Query(ctx, `SELECT id, credential_generation, created_at, expires_at, revoked_at
		FROM control_plane.worker_nodes ORDER BY id`)
	if err != nil {
		return nil, databaseError(ctx, err)
	}
	defer rows.Close()
	workers := []Worker{}
	for rows.Next() {
		var worker Worker
		if err := rows.Scan(&worker.ID, &worker.Generation, &worker.CreatedAt, &worker.ExpiresAt, &worker.RevokedAt); err != nil {
			return nil, databaseError(ctx, err)
		}
		workers = append(workers, worker)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(ctx, err)
	}
	return workers, nil
}

// AuthenticateTx locks the Worker row until the caller commits or rolls back.
// Perform the authorized state change in this same transaction, so credential
// revocation and rotation cannot race between authentication and the mutation.
// A failed authentication must not be followed by any authorized operation.
func AuthenticateTx(ctx context.Context, tx pgx.Tx, token string) (Worker, error) {
	if !validToken(token) {
		return Worker{}, ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	var worker Worker
	err := tx.QueryRow(ctx, `SELECT id, credential_generation, created_at, expires_at, revoked_at
		FROM control_plane.worker_nodes WHERE credential_sha256=$1 FOR UPDATE`, digest[:]).Scan(
		&worker.ID, &worker.Generation, &worker.CreatedAt, &worker.ExpiresAt, &worker.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Worker{}, ErrUnauthenticated
	}
	if err != nil {
		return Worker{}, databaseError(ctx, err)
	}
	// Read database wall time after acquiring the lock; a transaction may have
	// waited until after expiry, and transaction_timestamp would be stale.
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return Worker{}, databaseError(ctx, err)
	}
	if worker.RevokedAt != nil || !worker.ExpiresAt.After(now) {
		return Worker{}, ErrUnauthenticated
	}
	return worker, nil
}

func validToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil && len(decoded) == 32
}

func (store *Store) connect(ctx context.Context) (*pgx.Conn, error) {
	if store.databaseURL == "" {
		return nil, errors.New("AGENT_DATABASE_URL is required")
	}
	config, err := pgx.ParseConfig(store.databaseURL)
	if err != nil {
		return nil, errors.New("invalid AGENT_DATABASE_URL: check the PostgreSQL connection configuration")
	}
	config.RuntimeParams["application_name"] = "agent-worker-credential"
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, databaseError(ctx, err)
	}
	return connection, nil
}

func closeConnection(connection *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = connection.Close(ctx)
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func databaseError(ctx context.Context, _ error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("Worker Credential database operation failed; check PostgreSQL connectivity and migrations")
}
