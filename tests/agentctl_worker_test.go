package workspace_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
)

func TestAgentctlWorkerShowsCredentialOnlyWhenIssued(t *testing.T) {
	store, databaseURL := workerCredentialStore(t)
	binary := buildAgentctl(t)
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	output, err := runWorkerCLI(t, binary, databaseURL, "provision", "--id", "cli-worker", "--expires-at", expires)
	if err != nil {
		t.Fatalf("provision: %v %s", err, output)
	}
	var issued workercredential.Issued
	if err := json.Unmarshal([]byte(output), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Token == "" || strings.Count(output, issued.Token) != 1 {
		t.Fatal("new credential must appear exactly once")
	}
	if _, err := store.Authenticate(context.Background(), issued.Token); err != nil {
		t.Fatal(err)
	}
	output, err = runWorkerCLI(t, binary, databaseURL, "list")
	if err != nil || strings.Contains(output, issued.Token) || strings.Contains(output, "token") || !strings.Contains(output, "cli-worker") {
		t.Fatalf("metadata-only list: %v %s", err, output)
	}
	output, err = runWorkerCLI(t, binary, databaseURL, "rotate", "--id", "cli-worker", "--expires-at", expires)
	if err != nil || strings.Contains(output, issued.Token) {
		t.Fatalf("rotation must show only replacement: %v", err)
	}
	var replacement workercredential.Issued
	if err := json.Unmarshal([]byte(output), &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.Token == "" || strings.Count(output, replacement.Token) != 1 {
		t.Fatal("replacement must appear once")
	}
	output, err = runWorkerCLI(t, binary, databaseURL, "revoke", "--id", "cli-worker")
	if err != nil || strings.Contains(output, replacement.Token) {
		t.Fatalf("revocation: %v", err)
	}
}

func runWorkerCLI(t *testing.T, binary, databaseURL string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, append([]string{"worker"}, args...)...)
	command.Env = migrationEnvironment(databaseURL)
	output, err := command.CombinedOutput()
	return string(output), err
}

func TestAgentctlWorkerErrorsNeverEchoCredentialOrDatabaseSecrets(t *testing.T) {
	binary := buildAgentctl(t)
	const secret = "t22-synthetic-database-secret"
	for _, args := range [][]string{
		{"list"},
		{"list", "--unknown=" + secret},
		{"provision", "--id", "worker", "--expires-at", secret},
	} {
		output, err := runWorkerCLI(t, binary, "postgres://operator:"+secret+"@localhost:invalid/database", args...)
		if err == nil || strings.Contains(output, secret) {
			t.Fatalf("invalid command must fail without echoing secrets: %v", err)
		}
	}
}
