//go:build linux

package sandboxsupervisor

import (
	"errors"
	"math"
	"os"
	"time"
)

// A finite protocol ceiling, independent of the operator's lower capture budget.
const maximumOutputBytes = 8 << 20

// ResourceBudget is trusted Supervisor startup policy. Workloads and Worker
// requests cannot select or override these finite Sandbox allowances.
type ResourceBudget struct {
	CPUMillis        int64
	MemoryBytes      int64
	SwapBytes        int64
	PIDs             int64
	ExecutionTimeout time.Duration
	StdoutBytes      int
	StderrBytes      int
}

func DefaultResourceBudget() ResourceBudget {
	return ResourceBudget{CPUMillis: 2000, MemoryBytes: 1 << 30, SwapBytes: 0, PIDs: 64, ExecutionTimeout: 60 * time.Second, StdoutBytes: 1 << 20, StderrBytes: 1 << 20}
}

func (budget ResourceBudget) validate() error {
	// A fixed 100 ms period needs at least the kernel's 1 ms minimum quota.
	// The conversion below must not overflow. Memory controllers round bytes
	// to pages, so reject unaligned policies rather than silently relaxing them.
	if budget.CPUMillis < 10 || budget.CPUMillis > math.MaxInt64/100 {
		return errors.New("Resource Budget CPU must be at least 10 millicores and fit a 100 ms quota period")
	}
	page := int64(os.Getpagesize())
	if budget.MemoryBytes <= 0 || budget.MemoryBytes%page != 0 || budget.SwapBytes < 0 || budget.SwapBytes%page != 0 {
		return errors.New("Resource Budget memory must be positive and swap nonnegative, both in whole memory pages")
	}
	if budget.PIDs <= 0 {
		return errors.New("Resource Budget PIDs must be positive")
	}
	if budget.ExecutionTimeout <= 0 {
		return errors.New("Resource Budget Execution timeout must be positive")
	}
	if !validOutputBudget(budget.StdoutBytes) || !validOutputBudget(budget.StderrBytes) {
		return errors.New("Resource Budget stdout and stderr must each be between 1 byte and 8 MiB")
	}
	return nil
}

func validOutputBudget(bytes int) bool { return bytes > 0 && bytes <= maximumOutputBytes }
