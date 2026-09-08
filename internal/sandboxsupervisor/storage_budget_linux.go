//go:build linux

package sandboxsupervisor

import (
	"errors"
	"math"
	"os"
)

// StorageBudget bounds the writable filesystems shared by a Sandbox's Executions.
// It is trusted startup policy, never part of a Worker request.
type StorageBudget struct {
	WorkspaceBytes int64 `json:"workspace_bytes"`
	WorkspaceFiles int64 `json:"workspace_files"`
	TemporaryBytes int64 `json:"temporary_bytes"`
	TemporaryFiles int64 `json:"temporary_files"`
}

func (budget StorageBudget) validate() error {
	// tmpfs rounds size upward to whole pages; size=0 removes its limit.
	page := int64(os.Getpagesize())
	if budget.WorkspaceBytes <= 0 || budget.WorkspaceBytes%page != 0 || budget.TemporaryBytes <= 0 || budget.TemporaryBytes%page != 0 {
		return errors.New("Resource Budget storage bytes must be positive and in whole memory pages")
	}
	// Leave room for trusted directories and the kernel's inode accounting
	// (1024 metadata bytes per reserved inode). Zero disables the inode limit.
	const maximumFiles = math.MaxInt64/1024 - 3
	if budget.WorkspaceFiles <= 0 || budget.WorkspaceFiles > maximumFiles || budget.TemporaryFiles <= 0 || budget.TemporaryFiles > maximumFiles {
		return errors.New("Resource Budget storage file counts must be positive and fit kernel inode accounting")
	}
	return nil
}
