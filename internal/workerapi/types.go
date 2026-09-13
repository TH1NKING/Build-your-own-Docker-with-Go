// Package workerapi is the versioned HTTPS boundary for execution-plane nodes.
package workerapi

import (
	"errors"
	"fmt"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

type Lease = executionqueue.Lease
type claimRequest struct {
	WaitMilliseconds int64 `json:"wait_ms"`
}
type leaseReference struct {
	ExecutionID string `json:"execution_id"`
	Generation  int64  `json:"generation"`
}
type completeRequest struct {
	ExecutionID string                                `json:"execution_id"`
	Generation  int64                                 `json:"generation"`
	Result      sandboxsupervisor.ExecutePythonResult `json:"result"`
}

type HTTPError struct {
	StatusCode int
	Code       string
}

var ErrTransport = errors.New("Worker API transport failed")

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Worker API HTTP %d: %s", e.StatusCode, e.Code)
}
func Retryable(err error) bool {
	var status *HTTPError
	if errors.As(err, &status) {
		return status.StatusCode == 429 || status.StatusCode >= 500
	}
	return errors.Is(err, ErrTransport)
}
