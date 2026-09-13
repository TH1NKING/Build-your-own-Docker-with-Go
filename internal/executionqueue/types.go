// Package executionqueue owns durable Execution transitions in PostgreSQL.
// Only the trusted Control Plane opens this store; Workers use the Worker API.
package executionqueue

import (
	"errors"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

var (
	ErrInvalid  = errors.New("invalid Execution request")
	ErrConflict = errors.New("Execution conflicts with durable state")
	ErrNotFound = errors.New("Execution not found")
	ErrLease    = errors.New("Execution Lease is not current")
)

type Execution struct {
	ExecutionID string   `json:"execution_id"`
	AgentRunID  string   `json:"agent_run_id"`
	Source      string   `json:"source"`
	Stdin       string   `json:"stdin"`
	OutputPaths []string `json:"output_paths,omitempty"`
}

type Lease struct {
	ExecutionID string    `json:"execution_id"`
	AgentRunID  string    `json:"agent_run_id"`
	Generation  int64     `json:"generation"`
	ExpiresAt   time.Time `json:"expires_at"`
	Source      string    `json:"source"`
	Stdin       string    `json:"stdin"`
	OutputPaths []string  `json:"output_paths,omitempty"`
}

type Record struct {
	Execution  Execution
	State      string
	WorkerID   string
	Generation int64
	ExpiresAt  *time.Time
	Result     *sandboxsupervisor.ExecutePythonResult
}
