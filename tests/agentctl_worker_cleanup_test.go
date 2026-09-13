package workspace_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/migrations"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
	"github.com/jackc/pgx/v5"
)

func TestAgentctlWorkerConfirmsLegacyCleanupOnlyAfterRevocation(t *testing.T) {
	databaseURL := migrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	issued, err := credentials.Provision(ctx, "confirmed-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	queue, err := executionqueue.NewStore(databaseURL, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.ConfigureCapacity(ctx, issued.Token, 3); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"confirmed-leased", "confirmed-completed"} {
		if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: id, AgentRunID: id + "-run", Source: "pass"}); err != nil {
			t.Fatal(err)
		}
	}
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(ctx, `UPDATE control_plane.executions SET state='leased',worker_id='confirmed-worker',generation=1,lease_expires_at=clock_timestamp()+interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	result := sandboxsupervisor.ExecutePythonResult{ExecutionID: "confirmed-completed", TerminalReason: sandboxsupervisor.ExecutionExited, Stdout: "immutable before upgrade"}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `UPDATE control_plane.executions SET state='completed',result=$1 WHERE id='confirmed-completed'`, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.Run(ctx, databaseURL, migrations.Bundled(), false); err != nil {
		t.Fatal(err)
	}
	// Current reservations are distinct from the unknown historical ones. The
	// confirmation must not become a general-purpose capacity reset.
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "current-reservation", AgentRunID: "current-reservation-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	if current, err := queue.Claim(ctx, issued.Token); err != nil || current == nil || current.ExecutionID != "current-reservation" {
		t.Fatalf("current reservation: %+v %v", current, err)
	}
	binary := buildAgentctl(t)
	output, err := runWorkerCLI(t, binary, databaseURL, "confirm-legacy-cleanup", "--id", "confirmed-worker")
	if err == nil || !strings.Contains(output, "revoked") {
		t.Fatalf("active Worker cleanup confirmation: %v %s", err, output)
	}
	if capacity, err := queue.Capacity(ctx, issued.Token); err != nil || capacity.Occupied != 3 {
		t.Fatalf("rejected confirmation changed capacity: %+v %v", capacity, err)
	}
	if err := credentials.Revoke(ctx, "confirmed-worker"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{2, 0} {
		output, err = runWorkerCLI(t, binary, databaseURL, "confirm-legacy-cleanup", "--id", "confirmed-worker")
		if err != nil {
			t.Fatalf("confirm legacy cleanup: %v %s", err, output)
		}
		var confirmed struct {
			ID        string `json:"id"`
			Confirmed int64  `json:"confirmed"`
		}
		if json.Unmarshal([]byte(output), &confirmed) != nil || confirmed.ID != "confirmed-worker" || confirmed.Confirmed != want {
			t.Fatalf("confirmation outcome: %s", output)
		}
	}
	replacement, err := credentials.Rotate(ctx, "confirmed-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if capacity, err := queue.Capacity(ctx, replacement.Token); err != nil || capacity.Occupied != 1 || capacity.Available != 2 {
		t.Fatalf("confirmed upgrade capacity: %+v %v", capacity, err)
	}
	completed, err := queue.Get(ctx, "confirmed-completed")
	if err != nil || completed.State != "completed" || completed.Result == nil || completed.Result.Stdout != "immutable before upgrade" || completed.CleanupUnknown {
		t.Fatalf("confirmation damaged completed result: %+v %v", completed, err)
	}
	unfinished, err := queue.Get(ctx, "confirmed-leased")
	if err != nil || unfinished.State != "worker_lost" || unfinished.CleanupUnknown {
		t.Fatalf("confirmed unfinished state: %+v %v", unfinished, err)
	}
	current, err := queue.Get(ctx, "current-reservation")
	if err != nil || current.State != "leased" {
		t.Fatalf("legacy confirmation changed a current Execution: %+v %v", current, err)
	}
}
