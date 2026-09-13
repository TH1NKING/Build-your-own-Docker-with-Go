package workspace_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/migrations"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
	"github.com/jackc/pgx/v5"
)

func TestWorkerCredentialProvisionAuthenticatesOnlyItsWorker(t *testing.T) {
	store, _ := workerCredentialStore(t)
	ctx := context.Background()
	first, err := store.Provision(ctx, "worker-one", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Provision(ctx, "worker-two", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(first.Token)
	if err != nil || len(decoded) != 32 || first.Token == second.Token {
		t.Fatal("credentials must be independent 256-bit opaque values")
	}
	for _, issued := range []workercredential.Issued{first, second} {
		worker, err := store.Authenticate(ctx, issued.Token)
		if err != nil || worker.ID != issued.Worker.ID {
			t.Fatalf("authenticate Worker identity: %v", err)
		}
	}
}

func TestWorkerCredentialRotationRevocationAndExpiryAreIndependent(t *testing.T) {
	store, _ := workerCredentialStore(t)
	ctx := context.Background()
	first, err := store.Provision(ctx, "worker-one", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Provision(ctx, "worker-two", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := store.Rotate(ctx, first.Worker.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, first.Token); !errors.Is(err, workercredential.ErrUnauthenticated) {
		t.Fatalf("replaced credential accepted: %v", err)
	}
	if _, err := store.Authenticate(ctx, replacement.Token); err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(ctx, first.Worker.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(ctx, first.Worker.ID); err != nil {
		t.Fatal("revocation must be idempotent:", err)
	}
	if _, err := store.Authenticate(ctx, replacement.Token); !errors.Is(err, workercredential.ErrUnauthenticated) {
		t.Fatalf("revoked credential accepted: %v", err)
	}
	if _, err := store.Authenticate(ctx, second.Token); err != nil {
		t.Fatal("other Worker affected:", err)
	}
	workers, err := store.List(ctx)
	if err != nil || len(workers) != 2 {
		t.Fatalf("list Workers: %v", err)
	}
	encoded, _ := json.Marshal(workers)
	if strings.Contains(string(encoded), first.Token) || strings.Contains(string(encoded), "sha256") || strings.Contains(string(encoded), "token") {
		t.Fatal("list leaked credential material")
	}
	expiring, err := store.Provision(ctx, "worker-expiring", time.Now().Add(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := store.Authenticate(ctx, expiring.Token); !errors.Is(err, workercredential.ErrUnauthenticated) {
		t.Fatalf("expired credential accepted: %v", err)
	}
}

func workerCredentialStore(t *testing.T) (*workercredential.Store, string) {
	t.Helper()
	databaseURL := migrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := migrations.Run(ctx, databaseURL, migrations.Bundled(), false); err != nil {
		t.Fatal(err)
	}
	return workercredential.NewStore(databaseURL), databaseURL
}

func TestWorkerCredentialAuthenticationSerializesWithRevocationAndRotation(t *testing.T) {
	for _, operation := range []string{"revoke", "rotate"} {
		t.Run(operation, func(t *testing.T) {
			store, databaseURL := workerCredentialStore(t)
			ctx := context.Background()
			issued, err := store.Provision(ctx, "worker-locked", time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			connection, err := pgx.Connect(ctx, databaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close(ctx)
			tx, err := connection.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := workercredential.AuthenticateTx(ctx, tx, issued.Token); err != nil {
				t.Fatal(err)
			}
			waiting, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
			defer cancel()
			if operation == "revoke" {
				err = store.Revoke(waiting, issued.Worker.ID)
			} else {
				_, err = store.Rotate(waiting, issued.Worker.ID, time.Now().Add(time.Hour))
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("credential mutation bypassed active authorization transaction: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Authenticate(ctx, issued.Token); err != nil {
				t.Fatal("cancelled mutation changed credential:", err)
			}
			if operation == "revoke" {
				err = store.Revoke(ctx, issued.Worker.ID)
			} else {
				_, err = store.Rotate(ctx, issued.Worker.ID, time.Now().Add(time.Hour))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Authenticate(ctx, issued.Token); !errors.Is(err, workercredential.ErrUnauthenticated) {
				t.Fatalf("completed mutation left old credential valid: %v", err)
			}
		})
	}
}

func TestWorkerCredentialDatabaseRetainsOnlyVerifier(t *testing.T) {
	store, databaseURL := workerCredentialStore(t)
	ctx := context.Background()
	issued, err := store.Provision(ctx, "worker-stored", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(ctx)
	// At-rest confidentiality is a PostgreSQL persistence contract, so inspect
	// the complete durable row, not an API projection that could hide a raw copy.
	var row string
	var verifierLength int
	if err := connection.QueryRow(ctx, `SELECT row_to_json(w)::text, octet_length(credential_sha256)
		FROM control_plane.worker_nodes w WHERE id=$1`, issued.Worker.ID).Scan(&row, &verifierLength); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, issued.Token) || verifierLength != 32 {
		t.Fatal("durable Worker record must retain a SHA-256 verifier without the raw credential")
	}
}

func TestWorkerCredentialRejectsInvalidIssuanceAndUnknownCredentials(t *testing.T) {
	store, _ := workerCredentialStore(t)
	ctx := context.Background()
	for _, id := range []string{"", "../../worker", strings.Repeat("x", 65)} {
		if _, err := store.Provision(ctx, id, time.Now().Add(time.Hour)); !errors.Is(err, workercredential.ErrInvalid) {
			t.Fatalf("invalid Worker ID accepted: %v", err)
		}
	}
	if _, err := store.Provision(ctx, "expired", time.Now().Add(-time.Hour)); !errors.Is(err, workercredential.ErrInvalid) {
		t.Fatalf("past expiry accepted: %v", err)
	}
	if _, err := store.Rotate(ctx, "missing", time.Now().Add(time.Hour)); !errors.Is(err, workercredential.ErrNotFound) {
		t.Fatalf("unknown Worker rotated: %v", err)
	}
	if err := store.Revoke(ctx, "missing"); !errors.Is(err, workercredential.ErrNotFound) {
		t.Fatalf("unknown Worker revoked: %v", err)
	}
	if _, err := store.Authenticate(ctx, strings.Repeat("A", 43)); !errors.Is(err, workercredential.ErrUnauthenticated) {
		t.Fatalf("unknown credential accepted: %v", err)
	}
	if _, err := store.Authenticate(ctx, strings.Repeat("A", 4096)); !errors.Is(err, workercredential.ErrUnauthenticated) {
		t.Fatalf("oversized credential accepted: %v", err)
	}
	first, err := store.Provision(ctx, "duplicate", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Provision(ctx, "duplicate", time.Now().Add(time.Hour)); !errors.Is(err, workercredential.ErrConflict) {
		t.Fatalf("duplicate Worker replaced its identity: %v", err)
	}
	if _, err := store.Authenticate(ctx, first.Token); err != nil {
		t.Fatal("duplicate issuance invalidated original credential:", err)
	}
}
