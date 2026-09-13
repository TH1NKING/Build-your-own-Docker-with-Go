//go:build !linux

package workspace_test

import (
	"strings"
	"testing"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
)

func TestWorkerCredentialFileRejectsUnsupportedPermissionChecks(t *testing.T) {
	if _, err := workercredential.LoadFile("private-token"); err == nil || !strings.Contains(err.Error(), "Linux") {
		t.Fatalf("unsupported host must fail explicitly: %v", err)
	}
}
