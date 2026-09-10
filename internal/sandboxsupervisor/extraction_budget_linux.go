//go:build linux

package sandboxsupervisor

import "errors"

const maximumExtractionBytes = 32 << 20

// ExtractionBudget bounds decoded file bytes independently of tmpfs allocation
// and stdio. One Sandbox belongs to one Agent Run; its cumulative charge lasts
// until destruction, including repeated declarations of the same file.
type ExtractionBudget struct {
	FileBytes      int64
	ExecutionBytes int64
	SandboxBytes   int64
	Files          int
}

func (budget ExtractionBudget) validate() error {
	if budget.FileBytes <= 0 || budget.FileBytes > maximumExtractionBytes ||
		budget.ExecutionBytes <= 0 || budget.ExecutionBytes > maximumExtractionBytes ||
		budget.SandboxBytes <= 0 || budget.Files <= 0 || budget.Files > maximumExtractedFiles {
		return errors.New("Resource Budget extraction requires positive limits: file and Execution bytes <= 32 MiB, files <= 16")
	}
	return nil
}

func (budget ExtractionBudget) allowance(paths []string, used int64) int64 {
	return min(budget.ExecutionBytes, budget.SandboxBytes-used, int64(len(paths))*budget.FileBytes)
}
