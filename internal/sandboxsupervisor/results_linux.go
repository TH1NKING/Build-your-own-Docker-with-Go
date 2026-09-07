//go:build linux

package sandboxsupervisor

import (
	"encoding/json"
	"sync"
)

// Reservations bound both metadata and retained output across all Sandboxes,
// including results whose Init has been lost. No old result is silently evicted.
const (
	maximumRetainedExecutionResults = 64
	maximumRetainedOutputBytes      = 128 << 20
)

type executionResultReference struct {
	SandboxID   string `json:"sandbox_id"`
	ExecutionID string `json:"execution_id"`
}

type executionResultSlot struct {
	reservedBytes int
	result        *ExecutePythonResult // nil until the complete immutable snapshot is published
}

type executionResultStore struct {
	mu            sync.Mutex
	slots         map[executionResultReference]executionResultSlot
	reservedBytes int
}

func (store *executionResultStore) reserve(reference executionResultReference, bytes int) ErrorCode {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.slots[reference]; exists {
		return ErrorCodeExecutionExists
	}
	if len(store.slots) >= maximumRetainedExecutionResults || bytes > maximumRetainedOutputBytes-store.reservedBytes {
		return ErrorCodeResultCapacity
	}
	if store.slots == nil {
		store.slots = make(map[executionResultReference]executionResultSlot)
	}
	store.slots[reference] = executionResultSlot{reservedBytes: bytes}
	store.reservedBytes += bytes
	return ""
}

func (store *executionResultStore) finish(reference executionResultReference, result *ExecutePythonResult) {
	store.mu.Lock()
	defer store.mu.Unlock()
	slot, exists := store.slots[reference]
	if !exists {
		return
	}
	if result == nil {
		store.reservedBytes -= slot.reservedBytes
		delete(store.slots, reference)
		return
	}
	snapshot := *result
	slot.result = &snapshot
	store.slots[reference] = slot
}

func (store *executionResultStore) read(reference executionResultReference) (*ExecutePythonResult, ErrorCode) {
	store.mu.Lock()
	defer store.mu.Unlock()
	slot, exists := store.slots[reference]
	if !exists {
		return nil, ErrorCodeResultNotFound
	}
	if slot.result == nil {
		return nil, ErrorCodeResultNotReady
	}
	snapshot := *slot.result
	return &snapshot, ""
}

func (store *executionResultStore) removeSandbox(sandboxID string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for reference, slot := range store.slots {
		if reference.SandboxID == sandboxID {
			store.reservedBytes -= slot.reservedBytes
			delete(store.slots, reference)
		}
	}
}

func (service *server) getExecutionResultResponse(requestID string, raw json.RawMessage) responseEnvelope {
	var reference executionResultReference
	if err := decodeStrictJSON(raw, &reference, "sandbox_id", "execution_id"); err != nil || reference.SandboxID == "" || reference.ExecutionID == "" {
		return protocolErrorResponse(requestID, ErrorCodeMalformedRequest)
	}
	if !validOpaqueIdentifier(reference.SandboxID) || !validOpaqueIdentifier(reference.ExecutionID) {
		return protocolErrorResponse(requestID, ErrorCodeInvalidReference)
	}
	if !service.creator.enabled() {
		return protocolErrorResponse(requestID, ErrorCodeOperationUnavailable)
	}
	// Reads neither contact Init nor acquire operation.own cancellation rights.
	result, code := service.creator.results.read(reference)
	if code != "" {
		return protocolErrorResponse(requestID, code)
	}
	return responseEnvelope{Schema: ResponseSchemaV1, RequestID: requestID, Result: result}
}
