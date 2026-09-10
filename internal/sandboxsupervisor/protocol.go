// Package sandboxsupervisor implements the closed local protocol between an
// unprivileged Worker Node and the privileged Sandbox Supervisor.
package sandboxsupervisor

const (
	RequestSchemaV1  = "sandbox-supervisor-request/v1"
	ResponseSchemaV1 = "sandbox-supervisor-response/v1"

	OperationCreateSandbox      = "create_sandbox"
	OperationExecutePython      = "execute_python"
	OperationDestroySandbox     = "destroy_sandbox"
	OperationGetExecutionResult = "get_execution_result"
)

type ErrorCode string

const (
	ErrorCodeProfileNotFound        ErrorCode = "profile_not_found"
	ErrorCodeUnsupportedVersion     ErrorCode = "unsupported_version"
	ErrorCodeUnknownOperation       ErrorCode = "unknown_operation"
	ErrorCodeMalformedRequest       ErrorCode = "malformed_request"
	ErrorCodeInvalidReference       ErrorCode = "invalid_reference"
	ErrorCodeRequestTooLarge        ErrorCode = "request_too_large"
	ErrorCodeOperationUnavailable   ErrorCode = "operation_unavailable"
	ErrorCodeCreationFailed         ErrorCode = "creation_failed"
	ErrorCodeSandboxExists          ErrorCode = "sandbox_exists"
	ErrorCodeIdentityRangeExhausted ErrorCode = "identity_range_exhausted"
	ErrorCodeSandboxNotFound        ErrorCode = "sandbox_not_found"
	ErrorCodeSandboxBusy            ErrorCode = "sandbox_busy"
	ErrorCodeExecutionFailed        ErrorCode = "execution_failed"
	ErrorCodeCleanupFailed          ErrorCode = "cleanup_failed"
	ErrorCodeExecutionExists        ErrorCode = "execution_exists"
	ErrorCodeResultNotFound         ErrorCode = "execution_result_not_found"
	ErrorCodeResultNotReady         ErrorCode = "execution_result_not_ready"
	ErrorCodeResultCapacity         ErrorCode = "execution_result_capacity"
)

type DestroySandboxRequest struct {
	RequestID string
	SandboxID string
}

type DestroySandboxResponse struct {
	Schema    string                `json:"schema"`
	RequestID string                `json:"request_id"`
	Result    *DestroySandboxResult `json:"result"`
	Error     *ProtocolError        `json:"error"`
}

type DestroySandboxResult struct {
	SandboxID string `json:"sandbox_id"`
}

type ExecutePythonRequest struct {
	RequestID   string
	SandboxID   string
	ExecutionID string
	Source      string
	Stdin       string
	OutputPaths []string
}

type ExecutePythonResponse struct {
	Schema    string               `json:"schema"`
	RequestID string               `json:"request_id"`
	Result    *ExecutePythonResult `json:"result"`
	Error     *ProtocolError       `json:"error"`
}

type GetExecutionResultRequest struct {
	RequestID   string
	SandboxID   string
	ExecutionID string
}

type GetExecutionResultResponse struct {
	Schema    string               `json:"schema"`
	RequestID string               `json:"request_id"`
	Result    *ExecutePythonResult `json:"result"`
	Error     *ProtocolError       `json:"error"`
}

type ExecutePythonResult struct {
	ExecutionID     string                  `json:"execution_id"`
	ExitCode        int                     `json:"exit_code"`
	Stdout          string                  `json:"stdout"`
	Stderr          string                  `json:"stderr"`
	Truncated       bool                    `json:"truncated"`
	StdoutTruncated bool                    `json:"stdout_truncated"`
	StderrTruncated bool                    `json:"stderr_truncated"`
	TerminalReason  ExecutionTerminalReason `json:"terminal_reason"`
	ResourceUsage   ExecutionResourceUsage  `json:"resource_usage"`
	Outputs         []ExtractedOutput       `json:"outputs,omitempty"`
	OutputError     OutputExtractionError   `json:"output_error,omitempty"`
}

// ExtractedOutput is a bounded byte snapshot, not yet a durable Run Artifact.
// JSON transports Content as base64; size and SHA256 describe the decoded bytes.
type ExtractedOutput struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Content []byte `json:"content"`
}

type OutputExtractionError string

const (
	OutputUnsafe      OutputExtractionError = "unsafe_output"
	OutputLimit       OutputExtractionError = "output_limit"
	OutputUnavailable OutputExtractionError = "output_unavailable"
)

type ExecutionTerminalReason string

const (
	ExecutionExited      ExecutionTerminalReason = "exited"
	ExecutionMemoryLimit ExecutionTerminalReason = "memory_limit"
	ExecutionPIDLimit    ExecutionTerminalReason = "pids_limit"
	ExecutionTimedOut    ExecutionTerminalReason = "timed_out"
)

type ExecutionResourceUsage struct {
	OOMEvents           uint64 `json:"oom_events"`
	PIDLimitEvents      uint64 `json:"pid_limit_events"`
	CPUUsec             uint64 `json:"cpu_usec"`
	CPUThrottledPeriods uint64 `json:"cpu_throttled_periods"`
	CPUThrottledUsec    uint64 `json:"cpu_throttled_usec"`
}

type CreateSandboxRequest struct {
	RequestID       string
	SandboxID       string
	ProfileIdentity string
}

type CreateSandboxResponse struct {
	Schema    string               `json:"schema"`
	RequestID string               `json:"request_id"`
	Result    *CreateSandboxResult `json:"result"`
	Error     *ProtocolError       `json:"error"`
}

type CreateSandboxResult struct {
	SandboxID string `json:"sandbox_id"`
}

type ProtocolError struct {
	Code ErrorCode `json:"code"`
}
