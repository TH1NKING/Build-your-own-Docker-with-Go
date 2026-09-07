//go:build linux

package sandboxsupervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type executePythonParameters struct {
	SandboxID   string `json:"sandbox_id"`
	ExecutionID string `json:"execution_id"`
	Source      string `json:"source"`
	Stdin       string `json:"stdin"`
}

func (service *server) executePythonResponse(operation *controlOperation, requestID string, raw json.RawMessage) responseEnvelope {
	var parameters executePythonParameters
	if err := decodeStrictJSON(raw, &parameters, "sandbox_id", "execution_id", "source", "stdin"); err != nil || parameters.Source == "" || strings.ContainsRune(parameters.Source, 0) || len(parameters.Source) > 32<<10 || len(parameters.Stdin) > 8<<10 {
		return protocolErrorResponse(requestID, ErrorCodeMalformedRequest)
	}
	if !validOpaqueIdentifier(parameters.SandboxID) || !validOpaqueIdentifier(parameters.ExecutionID) {
		return protocolErrorResponse(requestID, ErrorCodeInvalidReference)
	}
	if !service.creator.enabled() {
		return protocolErrorResponse(requestID, ErrorCodeOperationUnavailable)
	}
	result, code := service.creator.execute(operation, parameters)
	if code != "" {
		return protocolErrorResponse(requestID, code)
	}
	return responseEnvelope{Schema: ResponseSchemaV1, RequestID: requestID, Result: result}
}

func (creator *sandboxCreator) execute(operation *controlOperation, parameters executePythonParameters) (*ExecutePythonResult, ErrorCode) {
	creator.mu.Lock()
	sandbox := creator.active[parameters.SandboxID]
	if sandbox == nil || sandbox.process == nil || sandbox.terminating || sandbox.ctx.Err() != nil {
		creator.mu.Unlock()
		return nil, ErrorCodeSandboxNotFound
	}
	locked := sandbox.execution.TryLock()
	creator.mu.Unlock()
	if !locked {
		return nil, ErrorCodeSandboxBusy
	}
	defer sandbox.execution.Unlock()
	operation.own(sandbox)
	select {
	case <-sandbox.exited:
		return nil, ErrorCodeSandboxNotFound
	default:
	}
	group, err := newCgroup(sandbox.cgroups.budget.group)
	sandbox.cgroups.execution = group
	complete := false
	defer func() {
		if !complete {
			// PID 1 loss tears down the whole PID namespace. No next Execution
			// is allowed after an uncertain handshake or failed cleanup.
			_ = sandbox.process.Kill()
			<-sandbox.exited
		}
		// Successful completion explicitly removes the Execution cgroup below.
		// Failed or uncertain cleanup stays owned by the Sandbox teardown.
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Execution cgroup:", err)
		return nil, ErrorCodeOperationUnavailable
	}
	if err := group.set("memory.oom.group", "1"); err != nil {
		return nil, ErrorCodeOperationUnavailable
	}
	before, err := sandbox.cgroups.budget.resourceUsage()
	if err != nil {
		return nil, ErrorCodeOperationUnavailable
	}
	// A bounded transport guard until the configurable Execution deadline
	// contract is implemented. This is never renewed by Workload activity.
	deadline := time.Now().Add(60 * time.Second)
	_ = sandbox.requests.SetWriteDeadline(deadline)
	_ = sandbox.responses.SetReadDeadline(deadline)
	if err := sendInitRequest(sandbox.requests, initRequest{Action: "start", Source: parameters.Source, Stdin: parameters.Stdin}); err != nil {
		return nil, ErrorCodeExecutionFailed
	}
	started, err := receiveInitResponse(sandbox.responses, "started")
	if err != nil || started.PID <= 1 {
		return nil, ErrorCodeExecutionFailed
	}
	hostPID, err := hostChildPID(sandbox.process.Pid, started.PID)
	if err != nil {
		return nil, ErrorCodeExecutionFailed
	}
	if err := group.attach(hostPID); err != nil {
		return nil, ErrorCodeExecutionFailed
	}
	if err := setProcessOOMScore(hostPID, 0); err != nil {
		return nil, ErrorCodeExecutionFailed
	}
	if err := sendInitRequest(sandbox.requests, initRequest{Action: "run"}); err != nil {
		return nil, ErrorCodeExecutionFailed
	}
	waitErr := waitBudgetedExecution(sandbox, group, before)
	if err := group.killAndWait(); err != nil {
		return nil, ErrorCodeExecutionFailed
	}
	after, err := sandbox.cgroups.budget.resourceUsage()
	if err != nil {
		return nil, ErrorCodeExecutionFailed
	}
	usage, err := after.since(before)
	if err != nil {
		return nil, ErrorCodeExecutionFailed
	}
	reason := ExecutionExited
	if usage.OOMEvents != 0 {
		reason = ExecutionMemoryLimit
	} else if usage.PIDLimitEvents != 0 {
		reason = ExecutionPIDLimit
	}
	if waitErr != nil {
		return incompleteResourceResult(parameters.ExecutionID, reason, usage)
	}
	if err := sendInitRequest(sandbox.requests, initRequest{Action: "reap"}); err != nil {
		return incompleteResourceResult(parameters.ExecutionID, reason, usage)
	}
	result, err := receiveInitResponse(sandbox.responses, "ready")
	if err != nil {
		return incompleteResourceResult(parameters.ExecutionID, reason, usage)
	}
	if err := group.close(); err != nil {
		return nil, ErrorCodeExecutionFailed
	}
	sandbox.cgroups.execution = nil
	complete = true
	return &ExecutePythonResult{ExecutionID: parameters.ExecutionID, ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr, Truncated: result.Truncated, TerminalReason: reason, ResourceUsage: usage}, ""
}

func incompleteResourceResult(executionID string, reason ExecutionTerminalReason, usage ExecutionResourceUsage) (*ExecutePythonResult, ErrorCode) {
	if reason == ExecutionExited {
		return nil, ErrorCodeExecutionFailed
	}
	// Host kernel evidence survives loss of Init, but its exit status and
	// captured output do not. Never manufacture an exit code from an OOM.
	// The caller's failed-handshake teardown invalidates this Sandbox.
	return &ExecutePythonResult{ExecutionID: executionID, ExitCode: -1, Truncated: true, TerminalReason: reason, ResourceUsage: usage}, ""
}

func setProcessOOMScore(pid, score int) error {
	path := filepath.Join("/proc", strconv.Itoa(pid), "oom_score_adj")
	if err := os.WriteFile(path, []byte(strconv.Itoa(score)), 0); err != nil {
		return fmt.Errorf("set process OOM adjustment: %w", err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(actual)) != strconv.Itoa(score) {
		return errors.New("kernel did not confirm process OOM adjustment")
	}
	return nil
}

func waitBudgetedExecution(sandbox *createdSandbox, group *cgroup, before ExecutionResourceUsage) error {
	exited := make(chan error, 1)
	go func() {
		_, err := receiveInitResponse(sandbox.responses, "exited")
		exited <- err
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	terminated := false
	for {
		select {
		case err := <-exited:
			return err
		case <-ticker.C:
			after, err := sandbox.cgroups.budget.resourceEvents()
			if err != nil || after.OOMEvents < before.OOMEvents || after.PIDLimitEvents < before.PIDLimitEvents {
				return errors.New("cannot observe Execution Resource Budget")
			}
			if (after.OOMEvents > before.OOMEvents || after.PIDLimitEvents > before.PIDLimitEvents) && !terminated {
				if err := group.killAndWait(); err != nil {
					return err
				}
				terminated = true
			}
		}
	}
}

func sendInitRequest(file *os.File, request initRequest) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return writeFrame(file, payload)
}

func receiveInitResponse(file *os.File, phase string) (initResponse, error) {
	payload, err := readFrame(file)
	if err != nil {
		return initResponse{}, err
	}
	var response initResponse
	if err := decodeStrictJSON(payload, &response, "phase", "pid", "exit_code", "stdout", "stderr", "truncated"); err != nil {
		return initResponse{}, err
	}
	if response.Phase != phase {
		return initResponse{}, errors.New("unexpected Sandbox Init phase")
	}
	return response, nil
}

// The child is a trusted launcher blocked on a private pipe. Resolve only a
// direct child of this Init and verify its namespace-local PID before moving
// it into the cgroup; the gate prevents forks or exit before this assignment.
func hostChildPID(initPID, namespacePID int) (int, error) {
	tasks, err := filepath.Glob(filepath.Join("/proc", strconv.Itoa(initPID), "task", "*", "children"))
	if err != nil {
		return 0, err
	}
	for _, task := range tasks {
		children, err := os.ReadFile(task)
		if err != nil {
			continue
		}
		for _, child := range strings.Fields(string(children)) {
			status, err := os.ReadFile(filepath.Join("/proc", child, "status"))
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(status), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 3 && fields[0] == "NSpid:" && fields[len(fields)-1] == strconv.Itoa(namespacePID) {
					return strconv.Atoi(child)
				}
			}
		}
	}
	return 0, errors.New("gated Workload is not a live direct child of Sandbox Init")
}
