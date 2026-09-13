//go:build linux

package sandboxsupervisor

import (
	"encoding/json"
	"os"
	"syscall"
)

type inspectSandboxParameters struct {
	SandboxID string `json:"sandbox_id"`
}

func (service *server) inspectSandboxResponse(requestID string, raw json.RawMessage) responseEnvelope {
	var parameters inspectSandboxParameters
	if err := decodeStrictJSON(raw, &parameters, "sandbox_id"); err != nil || parameters.SandboxID == "" {
		return protocolErrorResponse(requestID, ErrorCodeMalformedRequest)
	}
	if !validOpaqueIdentifier(parameters.SandboxID) {
		return protocolErrorResponse(requestID, ErrorCodeInvalidReference)
	}
	if !service.creator.enabled() {
		return protocolErrorResponse(requestID, ErrorCodeOperationUnavailable)
	}
	if code := service.creator.inspect(parameters.SandboxID); code != "" {
		return protocolErrorResponse(requestID, code)
	}
	return responseEnvelope{Schema: ResponseSchemaV1, RequestID: requestID, Result: &InspectSandboxResult{SandboxID: parameters.SandboxID}}
}

func (creator *sandboxCreator) inspect(id string) ErrorCode {
	creator.mu.Lock()
	defer creator.mu.Unlock()
	sandbox := creator.active[id]
	if sandbox == nil {
		return ErrorCodeSandboxNotFound
	}
	if sandbox.cleanupErr != nil {
		return ErrorCodeCleanupFailed
	}
	if sandbox.process == nil || sandbox.requests == nil || sandbox.responses == nil || sandbox.terminating || sandbox.ctx.Err() != nil {
		return ErrorCodeSandboxNotFound
	}
	select {
	case <-sandbox.exited:
		return ErrorCodeSandboxNotFound
	default:
	}
	if err := sandbox.process.Signal(syscall.Signal(0)); err != nil {
		return ErrorCodeSandboxNotFound
	}
	info, err := creator.root.Lstat(id)
	if err != nil || sandbox.directory == nil || !info.IsDir() || !os.SameFile(info, sandbox.directory) {
		return ErrorCodeSandboxNotFound
	}
	// This read does not acquire the Execution lock, contact Init, or own a
	// controlOperation's cancellation rights. It remains usable while Python
	// runs, and losing the inspection connection cannot destroy that Sandbox.
	return ""
}
