//go:build linux

package sandboxsupervisor

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"time"
)

// One socket carries one operation. Only a request that acquired a Sandbox
// may cancel it; rejected duplicates and busy requests own no cleanup rights.
type controlOperation struct {
	ctx              context.Context
	cancel           context.CancelFunc
	mu               sync.Mutex
	finished         bool
	sandbox          *createdSandbox
	stopCancellation func() bool
}

func beginControlOperation(parent context.Context, connection *net.UnixConn) *controlOperation {
	ctx, cancel := context.WithCancel(parent)
	operation := &controlOperation{ctx: ctx, cancel: cancel}
	// The five-second framing deadline is not an Execution deadline.
	_ = connection.SetReadDeadline(time.Time{})
	go func() {
		var extra [1]byte
		_, _ = connection.Read(extra[:]) // EOF, error, or extra request bytes abandon this operation.
		operation.mu.Lock()
		defer operation.mu.Unlock()
		if !operation.finished {
			operation.cancel()
		}
	}()
	return operation
}

func (operation *controlOperation) own(sandbox *createdSandbox) {
	operation.sandbox = sandbox
	operation.stopCancellation = context.AfterFunc(operation.ctx, sandbox.cancel)
}

func (operation *controlOperation) complete(delivered bool) {
	if operation.stopCancellation != nil {
		operation.stopCancellation()
	}
	if operation.sandbox != nil && (!delivered || operation.ctx.Err() != nil) {
		operation.sandbox.cancel()
		<-operation.sandbox.done
	}
	operation.cancel()
}

type destroySandboxParameters struct {
	SandboxID string `json:"sandbox_id"`
}

func (service *server) destroySandboxResponse(requestID string, raw json.RawMessage) responseEnvelope {
	var parameters destroySandboxParameters
	if err := decodeStrictJSON(raw, &parameters, "sandbox_id"); err != nil || parameters.SandboxID == "" {
		return protocolErrorResponse(requestID, ErrorCodeMalformedRequest)
	}
	if !validOpaqueIdentifier(parameters.SandboxID) {
		return protocolErrorResponse(requestID, ErrorCodeInvalidReference)
	}
	if !service.creator.enabled() {
		return protocolErrorResponse(requestID, ErrorCodeOperationUnavailable)
	}
	if code := service.creator.destroy(parameters.SandboxID); code != "" {
		return protocolErrorResponse(requestID, code)
	}
	return responseEnvelope{Schema: ResponseSchemaV1, RequestID: requestID, Result: &DestroySandboxResult{SandboxID: parameters.SandboxID}}
}

func (creator *sandboxCreator) destroy(id string) ErrorCode {
	creator.mu.Lock()
	sandbox := creator.active[id]
	if sandbox == nil {
		// Keep absent-Sandbox cleanup atomic with creation of a fresh ID.
		creator.results.removeSandbox(id)
		creator.mu.Unlock()
		return "" // An already absent Sandbox has nothing left to destroy.
	}
	sandbox.terminating = true
	creator.mu.Unlock()
	sandbox.cancel()
	<-sandbox.done
	creator.mu.Lock()
	defer creator.mu.Unlock()
	if creator.active[id] == sandbox && sandbox.cleanupErr != nil {
		creator.releaseLocked(id, sandbox, true)
	}
	if sandbox.cleanupErr != nil {
		return ErrorCodeCleanupFailed
	}
	creator.results.removeSandbox(id)
	return ""
}
