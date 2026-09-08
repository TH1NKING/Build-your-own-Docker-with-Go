// Package migrations applies the Control Plane's reviewed PostgreSQL schema changes.
package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
)

//go:embed sql/*.sql
var bundled embed.FS

// Bundled returns the migration source shipped with this version of agentctl.
func Bundled() fs.FS {
	source, _ := fs.Sub(bundled, "sql")
	return source
}

type Result struct {
	Version int
	Applied int
}

type migration struct {
	version int
	name    string
	sql     string
	digest  string
}

var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

// Run owns one connection for the entire migration operation.
func Run(ctx context.Context, databaseURL string, source fs.FS, status bool) (Result, error) {
	var result Result
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return result, errors.New("invalid AGENT_DATABASE_URL: check the PostgreSQL connection configuration")
	}
	files, err := load(source)
	if err != nil {
		return result, err
	}
	config.RuntimeParams["application_name"] = "agentctl-migrate"
	config.RuntimeParams["search_path"] = "public"
	config.BuildContextWatcherHandler = func(connection *pgconn.PgConn) ctxwatch.Handler {
		// Merely closing a socket may leave a long query holding the session lock.
		// Ask PostgreSQL to cancel first, with a bounded network fallback.
		return &pgconn.CancelRequestContextWatcherHandler{Conn: connection, DeadlineDelay: 2 * time.Second}
	}
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return result, databaseError(ctx, "connect to PostgreSQL", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = connection.Close(closeCtx)
	}()
	// A session lock spans bootstrap, history validation, and all file transactions.
	// Closing this dedicated connection releases it even on errors or cancellation.
	const migrationLock int64 = 0x4147454e5443544c // "AGENTCTL", scoped by PostgreSQL to this database.
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return result, databaseError(ctx, "acquire migration lock", err)
	}
	if status {
		var exists bool
		if err := connection.QueryRow(ctx, "SELECT to_regclass('public.agentctl_schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
			return result, databaseError(ctx, "inspect migration history", err)
		}
		if !exists {
			return result, nil
		}
		if err := connection.QueryRow(ctx, "SELECT COALESCE(MAX(version), 0) FROM public.agentctl_schema_migrations").Scan(&result.Version); err != nil {
			return result, databaseError(ctx, "read schema version", err)
		}
		return result, nil
	}
	if _, err := connection.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.agentctl_schema_migrations (
        version integer PRIMARY KEY CHECK (version > 0),
        name text NOT NULL,
        sha256 text NOT NULL CHECK (length(sha256) = 64),
        applied_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
    )`); err != nil {
		return result, databaseError(ctx, "initialize migration history", err)
	}
	rows, err := connection.Query(ctx, "SELECT version, name, sha256 FROM public.agentctl_schema_migrations ORDER BY version")
	if err != nil {
		return result, databaseError(ctx, "read migration history", err)
	}
	for rows.Next() {
		var version int
		var name, digest string
		if err := rows.Scan(&version, &name, &digest); err != nil {
			rows.Close()
			return result, databaseError(ctx, "read migration version", err)
		}
		if version != result.Version+1 || version > len(files) {
			rows.Close()
			return result, fmt.Errorf("migration history mismatch at version %d: restore the complete matching migration source", version)
		}
		file := files[version-1]
		if name != file.name || digest != file.digest {
			rows.Close()
			return result, fmt.Errorf("migration history mismatch at version %d: applied filename or SHA-256 has changed", version)
		}
		result.Version = version
	}
	if err := rows.Err(); err != nil {
		return result, databaseError(ctx, "read migration history", err)
	}
	for _, file := range files {
		if file.version <= result.Version {
			continue
		}
		if err := apply(ctx, connection, file); err != nil {
			return result, fmt.Errorf("migration %s failed; last confirmed schema version %d: %w", file.name, result.Version, err)
		}
		result.Version = file.version
		result.Applied++
	}
	return result, nil
}

func load(source fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return nil, errors.New("cannot read migration source directory")
	}
	var files []migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		match := migrationName.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("invalid migration filename %q; expected 0001_name.sql", entry.Name())
		}
		version, _ := strconv.Atoi(match[1])
		if version != len(files)+1 {
			return nil, fmt.Errorf("migration versions must be contiguous from 1: expected %04d, found %s", len(files)+1, entry.Name())
		}
		contents, err := fs.ReadFile(source, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("cannot read migration %s", entry.Name())
		}
		if strings.TrimSpace(string(contents)) == "" {
			return nil, fmt.Errorf("migration %s is empty", entry.Name())
		}
		files = append(files, migration{version: version, name: entry.Name(), sql: string(contents), digest: fmt.Sprintf("%x", sha256.Sum256(contents))})
	}
	if len(files) == 0 {
		return nil, errors.New("migration source contains no SQL files")
	}
	return files, nil
}

func apply(ctx context.Context, connection *pgx.Conn, file migration) error {
	transaction, err := connection.Begin(ctx)
	if err != nil {
		return databaseError(ctx, "begin transaction", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = transaction.Rollback(rollbackCtx)
	}()
	// Execute the entire reviewed file: splitting on ';' would break SQL bodies.
	if _, err := transaction.Exec(ctx, file.sql); err != nil {
		return databaseError(ctx, "execute SQL", err)
	}
	if _, err := transaction.Exec(ctx, "INSERT INTO public.agentctl_schema_migrations (version, name, sha256) VALUES ($1, $2, $3)", file.version, file.name, file.digest); err != nil {
		return databaseError(ctx, "record schema version", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return databaseError(ctx, "commit transaction (outcome may be unknown; reconnect and run --status)", err)
	}
	return nil
}

func databaseError(ctx context.Context, stage string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", stage, ctx.Err())
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return fmt.Errorf("%s: PostgreSQL SQLSTATE %s (inspect the database server log for details)", stage, postgresError.Code)
	}
	// Driver errors can contain connection strings and server-provided data.
	return fmt.Errorf("%s: database connection or protocol failure; check reachability, TLS, and credentials", stage)
}
