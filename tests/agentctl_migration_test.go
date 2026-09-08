package workspace_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestAgentctlMigrationRequiresExplicitDatabase(t *testing.T) {
	command := exec.Command("go", "run", "./cmd/agentctl", "migrate")
	command.Dir = profileBundleRepositoryRoot(t)
	command.Env = migrationEnvironment("")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "AGENT_DATABASE_URL is required") {
		t.Fatalf("migration without a configured database: %v\n%s", err, output)
	}
}

func TestAgentctlMigrationRejectsInvalidConnectionWithoutLeakingIt(t *testing.T) {
	binary := buildAgentctl(t)
	const secret = "t17-synthetic-password"
	output, err := runMigration(t, binary, "postgres://operator:"+secret+"@localhost:invalid/database")
	if err == nil || !strings.Contains(output, "invalid AGENT_DATABASE_URL") || strings.Contains(output, secret) {
		t.Fatalf("invalid connection must fail without printing its contents: %v\n%s", err, output)
	}
}

func TestAgentctlMigrationConnectionFailureIsDiagnostic(t *testing.T) {
	binary := buildAgentctl(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	const secret = "t17-synthetic-password"
	databaseURL := "postgres://operator:" + secret + "@" + listener.Addr().String() + "/database?sslmode=disable"
	output, err := runMigration(t, binary, databaseURL, "--timeout=200ms")
	if err == nil || !strings.Contains(output, "connect to PostgreSQL") || !strings.Contains(output, "deadline exceeded") || strings.Contains(output, secret) {
		t.Fatalf("stalled connection must report a bounded, sanitized error: %v\n%s", err, output)
	}
}

func TestAgentctlMigrationInitializesAndRepeats(t *testing.T) {
	databaseURL := migrationDatabase(t)
	binary := buildAgentctl(t)
	for index, want := range []string{"schema_version=1 applied=1", "schema_version=1 applied=0"} {
		output, err := runMigration(t, binary, databaseURL)
		if err != nil || strings.TrimSpace(output) != want {
			t.Fatalf("migration invocation %d: %v\n%s\nwant %s", index+1, err, output, want)
		}
	}
}

func TestAgentctlMigrationStatusWorksOnReadOnlyConnections(t *testing.T) {
	databaseURL := migrationDatabase(t)
	binary := buildAgentctl(t)
	readOnlyURL, _ := url.Parse(databaseURL)
	query := readOnlyURL.Query()
	query.Set("default_transaction_read_only", "on")
	readOnlyURL.RawQuery = query.Encode()
	for _, want := range []string{"schema_version=0 applied=0", "schema_version=1 applied=0"} {
		output, err := runMigration(t, binary, readOnlyURL.String(), "--status")
		if err != nil || strings.TrimSpace(output) != want {
			t.Fatalf("read-only migration status: %v\n%s\nwant %s", err, output, want)
		}
		if _, err := runMigration(t, binary, databaseURL); err != nil {
			t.Fatal("apply migration between status reads:", err)
		}
	}
}

func TestAgentctlMigrationFailureRollsBackOnlyTheFailedVersion(t *testing.T) {
	databaseURL := migrationDatabase(t)
	binary := buildAgentctl(t)
	files := map[string]string{
		"0001_initial.sql": "CREATE TABLE migration_probe (id integer PRIMARY KEY); INSERT INTO migration_probe VALUES (1);",
		"0002_change.sql":  "CREATE TABLE partial_change (id integer); INSERT INTO migration_probe VALUES (2); SELECT 1 / 0;",
		"0003_verify.sql": `DO $$ BEGIN
            IF (SELECT count(*) FROM migration_probe) <> 2 THEN
                RAISE EXCEPTION 'earlier committed data must remain usable';
            END IF;
        END $$;`,
	}
	directory := migrationDirectory(t, files)
	output, err := runMigration(t, binary, databaseURL, "--migrations-dir", directory)
	if err == nil || !strings.Contains(output, "0002_change.sql") || !strings.Contains(output, "schema version 1") || !strings.Contains(output, "SQLSTATE 22012") {
		t.Fatalf("failed migration must identify its file, version and SQLSTATE: %v\n%s", err, output)
	}
	output, err = runMigration(t, binary, databaseURL, "--status")
	if err != nil || strings.TrimSpace(output) != "schema_version=1 applied=0" {
		t.Fatalf("failed migration advanced the schema version: %v\n%s", err, output)
	}
	// Repeating the CREATE and INSERT succeeds only if both changes rolled back.
	files["0002_change.sql"] = "CREATE TABLE partial_change (id integer); INSERT INTO migration_probe VALUES (2);"
	directory = migrationDirectory(t, files)
	for _, want := range []string{"schema_version=3 applied=2", "schema_version=3 applied=0"} {
		output, err = runMigration(t, binary, databaseURL, "--migrations-dir", directory)
		if err != nil || strings.TrimSpace(output) != want {
			t.Fatalf("resume after correcting the unapplied migration: %v\n%s\nwant %s", err, output, want)
		}
	}
}

func TestAgentctlMigrationRejectsChangedOrMissingHistory(t *testing.T) {
	for _, change := range []string{"changed", "renamed", "missing"} {
		t.Run(change, func(t *testing.T) {
			databaseURL := migrationDatabase(t)
			binary := buildAgentctl(t)
			files := map[string]string{
				"0001_initial.sql": "CREATE TABLE migration_probe (id integer PRIMARY KEY);",
				"0002_change.sql":  "INSERT INTO migration_probe VALUES (1);",
			}
			if output, err := runMigration(t, binary, databaseURL, "--migrations-dir", migrationDirectory(t, files)); err != nil {
				t.Fatalf("apply original release: %v\n%s", err, output)
			}
			switch change {
			case "changed":
				files["0001_initial.sql"] += " -- edited after deployment"
			case "renamed":
				files["0001_renamed.sql"] = files["0001_initial.sql"]
				delete(files, "0001_initial.sql")
			case "missing":
				delete(files, "0002_change.sql")
			}
			output, err := runMigration(t, binary, databaseURL, "--migrations-dir", migrationDirectory(t, files))
			if err == nil || !strings.Contains(output, "migration history mismatch") {
				t.Fatalf("%s history must fail: %v\n%s", change, err, output)
			}
		})
	}
}

func TestAgentctlMigrationConcurrentInvocationsApplyOnce(t *testing.T) {
	databaseURL := migrationDatabase(t)
	binary := buildAgentctl(t)
	directory := migrationDirectory(t, map[string]string{
		"0001_initial.sql": "SELECT pg_sleep(0.3); CREATE TABLE migration_probe (id integer PRIMARY KEY); INSERT INTO migration_probe VALUES (1);",
	})
	type invocation struct {
		output string
		err    error
	}
	completed := make(chan invocation, 2)
	for range 2 {
		go func() {
			output, err := runMigration(t, binary, databaseURL, "--migrations-dir", directory)
			completed <- invocation{output, err}
		}()
	}
	first, second := <-completed, <-completed
	counts := map[string]int{}
	for _, result := range []invocation{first, second} {
		if result.err != nil {
			t.Fatalf("both concurrent invocations must succeed: %v\n%s", result.err, result.output)
		}
		counts[strings.TrimSpace(result.output)]++
	}
	if counts["schema_version=1 applied=1"] != 1 || counts["schema_version=1 applied=0"] != 1 {
		t.Fatalf("concurrent migration results = %v, want exactly one application and one no-op", counts)
	}
}

func TestAgentctlMigrationTimeoutRollsBackAndReleasesTheLock(t *testing.T) {
	databaseURL := migrationDatabase(t)
	binary := buildAgentctl(t)
	files := map[string]string{
		"0001_initial.sql": "CREATE TABLE migration_probe (id integer PRIMARY KEY); INSERT INTO migration_probe VALUES (1);",
	}
	if output, err := runMigration(t, binary, databaseURL, "--migrations-dir", migrationDirectory(t, files)); err != nil {
		t.Fatalf("initialize prior version: %v\n%s", err, output)
	}
	files["0002_slow.sql"] = "INSERT INTO migration_probe VALUES (2); SELECT pg_sleep(30);"
	output, err := runMigration(t, binary, databaseURL, "--migrations-dir", migrationDirectory(t, files), "--timeout=500ms")
	if err == nil || !strings.Contains(output, "schema version 1") || !strings.Contains(output, "deadline exceeded") {
		t.Fatalf("slow SQL must time out with prior version: %v\n%s", err, output)
	}
	files["0002_slow.sql"] = "INSERT INTO migration_probe VALUES (2);"
	output, err = runMigration(t, binary, databaseURL, "--migrations-dir", migrationDirectory(t, files))
	if err != nil || strings.TrimSpace(output) != "schema_version=2 applied=1" {
		t.Fatalf("timeout must roll back the INSERT and release its lock: %v\n%s", err, output)
	}
}

func TestAgentctlMigrationLockWaitHonorsTheDeadline(t *testing.T) {
	databaseURL := migrationDatabase(t)
	binary := buildAgentctl(t)
	files := map[string]string{"0001_initial.sql": "CREATE TABLE migration_probe (id integer PRIMARY KEY);"}
	if output, err := runMigration(t, binary, databaseURL, "--migrations-dir", migrationDirectory(t, files)); err != nil {
		t.Fatalf("initialize prior version: %v\n%s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	blocker, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(context.Background())
	transaction, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(context.Background())
	if _, err := transaction.Exec(ctx, "LOCK TABLE migration_probe IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	const blockedSQL = "INSERT INTO migration_probe VALUES (1);"
	files["0002_insert.sql"] = blockedSQL
	directory := migrationDirectory(t, files)
	first := make(chan string, 1)
	go func() {
		output, err := runMigration(t, binary, databaseURL, "--migrations-dir", directory)
		first <- fmt.Sprintf("%v:%s", err, strings.TrimSpace(output))
	}()
	// Observe the owned database fault boundary, not a sleep or runner internals.
	for {
		if _, err := transaction.Exec(ctx, "SELECT pg_stat_clear_snapshot()"); err != nil {
			t.Fatal("refresh PostgreSQL activity observation:", err)
		}
		var waiting bool
		err := transaction.QueryRow(ctx, `SELECT EXISTS (
            SELECT 1 FROM pg_stat_activity
            WHERE datname = current_database() AND state = 'active'
                AND wait_event_type = 'Lock' AND query = $1
        )`, blockedSQL).Scan(&waiting)
		if err != nil {
			t.Fatal("observe blocked fixture SQL:", err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	output, err := runMigration(t, binary, databaseURL, "--migrations-dir", directory, "--timeout=300ms")
	if err == nil || !strings.Contains(output, "acquire migration lock") || !strings.Contains(output, "deadline exceeded") {
		t.Fatalf("second migrator must time out waiting for the migration lock: %v\n%s", err, output)
	}
	if err := transaction.Rollback(ctx); err != nil {
		t.Fatal("release owned fixture lock:", err)
	}
	if output := <-first; output != "<nil>:schema_version=2 applied=1" {
		t.Fatalf("first migrator did not finish after lock release: %s", output)
	}
}

func TestAgentctlMigrationRejectsInvalidSourcesBeforeConnecting(t *testing.T) {
	binary := buildAgentctl(t)
	for _, files := range []map[string]string{
		{},
		{"0002_gap.sql": "SELECT 1;"},
		{"0001_first.sql": "SELECT 1;", "0001_duplicate.sql": "SELECT 1;"},
		{"1_invalid.sql": "SELECT 1;"},
		{"0001_empty.sql": "   \n"},
	} {
		output, err := runMigration(t, binary, "postgres://localhost:1/missing?sslmode=disable", "--migrations-dir", migrationDirectory(t, files))
		if err == nil || !strings.Contains(output, "migration") || strings.Contains(output, "connect to PostgreSQL") {
			t.Fatalf("invalid source must fail before connecting: %v\n%s", err, output)
		}
	}
}

func migrationDatabase(t *testing.T) string {
	t.Helper()
	adminURL := os.Getenv("AGENT_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("real PostgreSQL acceptance requires AGENT_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal("connect to test PostgreSQL:", err)
	}
	defer admin.Close(context.Background())
	name := "t17_" + strings.ToLower(rand.Text())
	identifier := pgx.Identifier{name}.Sanitize()
	// Each test owns a fresh database and a non-superuser database owner.
	password := rand.Text()
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", identifier, password)); err != nil {
		t.Fatal("create test database owner:", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		connection, err := pgx.Connect(cleanupCtx, adminURL)
		if err != nil {
			t.Error("connect for test database cleanup:", err)
			return
		}
		defer connection.Close(context.Background())
		if _, err := connection.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)"); err != nil {
			t.Error("drop owned test database:", err)
		}
		if _, err := connection.Exec(cleanupCtx, "DROP ROLE "+identifier); err != nil {
			t.Error("drop owned test role:", err)
		}
	})
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier+" OWNER "+identifier); err != nil {
		t.Fatal("create test database:", err)
	}
	config, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	config.User = url.UserPassword(name, password)
	config.Path = "/" + name
	return config.String()
}

func buildAgentctl(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "agentctl.exe")
	command := exec.Command("go", "build", "-o", binary, "./cmd/agentctl")
	command.Dir = profileBundleRepositoryRoot(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build public agentctl command: %v\n%s", err, output)
	}
	return binary
}

func runMigration(t *testing.T, binary, databaseURL string, arguments ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, append([]string{"migrate"}, arguments...)...)
	command.Env = migrationEnvironment(databaseURL)
	output, err := command.CombinedOutput()
	return string(output), err
}

func migrationEnvironment(databaseURL string) []string {
	var environment []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(value), "AGENT_DATABASE_URL=") {
			environment = append(environment, value)
		}
	}
	return append(environment, "AGENT_DATABASE_URL="+databaseURL)
}

func migrationDirectory(t *testing.T, files map[string]string) string {
	t.Helper()
	directory := t.TempDir()
	for name, sql := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(sql), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}
