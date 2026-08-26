// Package sandboxsupervisor implements the closed local protocol between an
// unprivileged Worker Node and the privileged Sandbox Supervisor.
package sandboxsupervisor

const (
	RequestSchemaV1  = "sandbox-supervisor-request/v1"
	ResponseSchemaV1 = "sandbox-supervisor-response/v1"

	OperationCreateSandbox = "create_sandbox"
)

type ErrorCode string

const (
	ErrorCodeProfileNotFound      ErrorCode = "profile_not_found"
	ErrorCodeUnsupportedVersion   ErrorCode = "unsupported_version"
	ErrorCodeUnknownOperation     ErrorCode = "unknown_operation"
	ErrorCodeMalformedRequest     ErrorCode = "malformed_request"
	ErrorCodeInvalidReference     ErrorCode = "invalid_reference"
	ErrorCodeRequestTooLarge      ErrorCode = "request_too_large"
	ErrorCodeOperationUnavailable ErrorCode = "operation_unavailable"
)

type CreateSandboxRequest struct {
	RequestID       string
	SandboxID       string
	ProfileIdentity string
}

type CreateSandboxResponse struct {
	Schema    string
	RequestID string
	Result    *CreateSandboxResult
	Error     *ProtocolError
}

type CreateSandboxResult struct{}

type ProtocolError struct {
	Code ErrorCode `json:"code"`
}
