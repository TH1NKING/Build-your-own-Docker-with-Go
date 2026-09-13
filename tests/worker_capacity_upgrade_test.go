package workspace_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/migrations"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
	"github.com/jackc/pgx/v5"
)

func TestWorkerAPIRecoveryUpgradeRetainsUnknownLegacyCleanup(t *testing.T) {
	databaseURL := migrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	legacy := fstest.MapFS{}
	for _, name := range []string{"0001_control_plane.sql", "0002_worker_credentials.sql", "0003_executions.sql", "0004_worker_sandbox_capacity.sql"} {
		raw, err := fs.ReadFile(migrations.Bundled(), name)
		if err != nil {
			t.Fatal(err)
		}
		legacy[name] = &fstest.MapFile{Data: raw}
	}
	if _, err := migrations.Run(ctx, databaseURL, legacy, false); err != nil {
		t.Fatal(err)
	}
	credentials := workercredential.NewStore(databaseURL)
	issued, err := credentials.Provision(ctx, "legacy-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	queue, err := executionqueue.NewStore(databaseURL, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy-leased", "legacy-completed", "legacy-released"} {
		if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: id, AgentRunID: id + "-run", Source: "pass"}); err != nil {
			t.Fatal(err)
		}
	}
	// Historical rows are migration input: the old protocol did not persist a
	// Sandbox ID. SQL prepares that old schema; all post-upgrade assertions use
	// the public Worker API or trusted domain reads.
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(ctx, `UPDATE control_plane.executions SET state='leased',worker_id='legacy-worker',generation=1,lease_expires_at=clock_timestamp()+interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	result := sandboxsupervisor.ExecutePythonResult{ExecutionID: "legacy-completed", TerminalReason: sandboxsupervisor.ExecutionExited, Stdout: "retained legacy result"}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `UPDATE control_plane.executions SET state='completed',result=$1 WHERE id='legacy-completed'`, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `UPDATE control_plane.executions SET capacity_released=true WHERE id='legacy-released'`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.Run(ctx, databaseURL, migrations.Bundled(), false); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy-leased", "legacy-completed"} {
		requireWorkerStatus(t, client.Release(ctx, workerapi.Lease{ExecutionID: id, Generation: 1}), http.StatusConflict)
	}
	if capacity, err := client.Capacity(ctx); err != nil || capacity.Occupied != 2 || capacity.Available != 0 {
		t.Fatalf("legacy unknown cleanup lost reservations: %+v %v", capacity, err)
	}
	var entries []struct {
		Lease          workerapi.Lease `json:"lease"`
		CleanupUnknown bool            `json:"cleanup_unknown"`
	}
	if code := capacityAPIPost(t, ctx, server, issued.Token, "outstanding", `{}`, &entries); code != http.StatusOK || len(entries) != 2 {
		t.Fatalf("legacy outstanding: HTTP %d %+v", code, entries)
	}
	for _, entry := range entries {
		if !entry.CleanupUnknown {
			t.Fatalf("legacy unknown Sandbox treated as never created: %+v", entry)
		}
	}
	if err := client.Complete(ctx, workerapi.Lease{ExecutionID: "legacy-completed", Generation: 1}, result); err != nil {
		t.Fatalf("upgrade lost immutable result acknowledgement: %v", err)
	}
	if err := client.Release(ctx, workerapi.Lease{ExecutionID: "legacy-released", Generation: 1}); err != nil {
		t.Fatalf("already confirmed cleanup stopped being idempotent: %v", err)
	}
	released, err := queue.Get(ctx, "legacy-released")
	if err != nil || released.State != "worker_lost" {
		t.Fatalf("legacy released unfinished state: %+v %v", released, err)
	}
}
