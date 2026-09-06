//go:build linux

package sandboxsupervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
)

type Client struct{ socketPath string }

func NewClient(socketPath string) *Client { return &Client{socketPath: socketPath} }

func (client *Client) DestroySandbox(ctx context.Context, request DestroySandboxRequest) (DestroySandboxResponse, error) {
	var response DestroySandboxResponse
	err := client.exchange(ctx, request.RequestID, OperationDestroySandbox, destroySandboxParameters{SandboxID: request.SandboxID}, &response)
	return response, err
}

func (client *Client) CreateSandbox(ctx context.Context, request CreateSandboxRequest) (CreateSandboxResponse, error) {
	var response CreateSandboxResponse
	err := client.exchange(ctx, request.RequestID, OperationCreateSandbox, createSandboxParameters{
		SandboxID: request.SandboxID, ProfileIdentity: request.ProfileIdentity,
	}, &response)
	return response, err
}

func (client *Client) ExecutePython(ctx context.Context, request ExecutePythonRequest) (ExecutePythonResponse, error) {
	var response ExecutePythonResponse
	err := client.exchange(ctx, request.RequestID, OperationExecutePython, executePythonParameters{
		SandboxID: request.SandboxID, ExecutionID: request.ExecutionID, Source: request.Source, Stdin: request.Stdin,
	}, &response)
	return response, err
}

func (client *Client) exchange(ctx context.Context, requestID, operation string, parameters any, target any) error {
	raw, err := json.Marshal(parameters)
	if err != nil {
		return fmt.Errorf("encode Sandbox Supervisor parameters: %w", err)
	}
	payload, err := json.Marshal(requestEnvelope{Schema: RequestSchemaV1, RequestID: requestID, Operation: operation, Parameters: raw})
	if err != nil {
		return fmt.Errorf("encode Sandbox Supervisor request: %w", err)
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", client.socketPath)
	if err != nil {
		return fmt.Errorf("connect to Sandbox Supervisor: %w", err)
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return err
		}
	}
	if err := writeFrame(connection, payload); err != nil {
		return fmt.Errorf("send Sandbox Supervisor request: %w", err)
	}
	responsePayload, err := readFrame(connection)
	if err != nil {
		return fmt.Errorf("receive Sandbox Supervisor response: %w", err)
	}
	var envelope struct {
		Schema    string          `json:"schema"`
		RequestID string          `json:"request_id"`
		Result    json.RawMessage `json:"result"`
		Error     *ProtocolError  `json:"error"`
	}
	if err := decodeStrictJSON(responsePayload, &envelope, "schema", "request_id", "result", "error"); err != nil {
		return err
	}
	hasResult := len(envelope.Result) > 0 && string(envelope.Result) != "null"
	if envelope.Schema != ResponseSchemaV1 || envelope.RequestID != requestID || hasResult == (envelope.Error != nil) {
		return fmt.Errorf("invalid Sandbox Supervisor response identity or outcome")
	}
	if err := json.Unmarshal(responsePayload, target); err != nil {
		return fmt.Errorf("decode Sandbox Supervisor response: %w", err)
	}
	return nil
}
