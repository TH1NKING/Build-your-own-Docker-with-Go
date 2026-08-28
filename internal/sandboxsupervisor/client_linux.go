//go:build linux

package sandboxsupervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
)

type Client struct {
	socketPath string
}

func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

func (client *Client) CreateSandbox(ctx context.Context, request CreateSandboxRequest) (CreateSandboxResponse, error) {
	parameters, err := json.Marshal(createSandboxParameters{
		SandboxID:       request.SandboxID,
		ProfileIdentity: request.ProfileIdentity,
	})
	if err != nil {
		return CreateSandboxResponse{}, fmt.Errorf("encode Sandbox creation parameters: %w", err)
	}
	wireRequest := requestEnvelope{
		Schema:     RequestSchemaV1,
		RequestID:  request.RequestID,
		Operation:  OperationCreateSandbox,
		Parameters: parameters,
	}
	payload, err := json.Marshal(wireRequest)
	if err != nil {
		return CreateSandboxResponse{}, fmt.Errorf("encode Sandbox Supervisor request: %w", err)
	}

	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", client.socketPath)
	if err != nil {
		return CreateSandboxResponse{}, fmt.Errorf("connect to Sandbox Supervisor: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return CreateSandboxResponse{}, fmt.Errorf("set Sandbox Supervisor request deadline: %w", err)
		}
	}
	if err := writeFrame(connection, payload); err != nil {
		return CreateSandboxResponse{}, fmt.Errorf("send Sandbox Supervisor request: %w", err)
	}
	responsePayload, err := readFrame(connection)
	if err != nil {
		return CreateSandboxResponse{}, fmt.Errorf("receive Sandbox Supervisor response: %w", err)
	}
	var response responseEnvelope
	if err := json.Unmarshal(responsePayload, &response); err != nil {
		return CreateSandboxResponse{}, fmt.Errorf("decode Sandbox Supervisor response: %w", err)
	}
	return CreateSandboxResponse{
		Schema:    response.Schema,
		RequestID: response.RequestID,
		Result:    response.Result,
		Error:     response.Error,
	}, nil
}
