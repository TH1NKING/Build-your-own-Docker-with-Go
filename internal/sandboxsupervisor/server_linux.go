//go:build linux

package sandboxsupervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"sync"
	"time"
)

type ServerConfig struct {
	SocketPath   string
	ProfileStore string
	SandboxRoot  string
	SubUIDStart  uint
	SubGIDStart  uint
	SubIDCount   uint
	CgroupRoot   string
}

type requestEnvelope struct {
	Schema     string          `json:"schema"`
	RequestID  string          `json:"request_id"`
	Operation  string          `json:"operation"`
	Parameters json.RawMessage `json:"parameters"`
}

type createSandboxParameters struct {
	SandboxID       string `json:"sandbox_id"`
	ProfileIdentity string `json:"profile_identity"`
}

type responseEnvelope struct {
	Schema    string         `json:"schema"`
	RequestID string         `json:"request_id"`
	Result    any            `json:"result,omitempty"`
	Error     *ProtocolError `json:"error,omitempty"`
}

type server struct {
	profileStore *os.Root
	creator      *sandboxCreator
}

const maximumConcurrentControlConnections = 16

func Serve(ctx context.Context, config ServerConfig) error {
	if err := sealInheritedDescriptors(); err != nil {
		return err
	}
	if err := validateSubordinateIDs(config); err != nil {
		return err
	}
	if err := validateTrustedPaths(config); err != nil {
		return err
	}
	profileStore, err := openRuntimeOwnedRoot("Profile store", config.ProfileStore)
	if err != nil {
		return err
	}
	defer profileStore.Close()
	sandboxRoot, err := openRuntimeOwnedRoot("Sandbox root", config.SandboxRoot)
	if err != nil {
		return err
	}
	defer sandboxRoot.Close()
	creator := newSandboxCreator(ctx, config, sandboxRoot)
	defer creator.close()

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: config.SocketPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on Sandbox Supervisor socket: %w", err)
	}
	listener.SetUnlinkOnClose(true)
	defer listener.Close()
	if err := os.Chmod(config.SocketPath, 0o660); err != nil {
		return fmt.Errorf("restrict Sandbox Supervisor socket permissions: %w", err)
	}

	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stopped:
		}
	}()

	service := &server{profileStore: profileStore, creator: creator}
	connectionSlots := make(chan struct{}, maximumConcurrentControlConnections)
	var handlers sync.WaitGroup
	defer handlers.Wait()
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept Sandbox Supervisor connection: %w", err)
		}
		select {
		case connectionSlots <- struct{}{}:
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer func() { <-connectionSlots }()
				service.handle(connection)
			}()
		default:
			_ = connection.Close()
		}
	}
}

func (service *server) handle(connection *net.UnixConn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))

	payload, err := readFrame(connection)
	if err != nil {
		if errors.Is(err, errControlMessageTooLarge) {
			service.writeProtocolError(connection, "", ErrorCodeRequestTooLarge)
		} else {
			service.writeProtocolError(connection, "", ErrorCodeMalformedRequest)
		}
		return
	}
	var request requestEnvelope
	if err := decodeStrictJSON(payload, &request, "schema", "request_id", "operation", "parameters"); err != nil {
		service.writeProtocolError(connection, "", ErrorCodeMalformedRequest)
		return
	}
	if request.Operation == OperationExecutePython {
		_ = connection.SetWriteDeadline(time.Now().Add(65 * time.Second))
	}
	service.writeResponse(connection, service.responseForRequest(request))
}

func (service *server) responseForRequest(request requestEnvelope) responseEnvelope {
	if request.Schema == "" || !validOpaqueIdentifier(request.RequestID) || request.Operation == "" || len(request.Parameters) == 0 {
		return protocolErrorResponse(request.RequestID, ErrorCodeMalformedRequest)
	}
	if request.Schema != RequestSchemaV1 {
		return protocolErrorResponse(request.RequestID, ErrorCodeUnsupportedVersion)
	}
	switch request.Operation {
	case OperationCreateSandbox:
		return service.createSandboxResponse(request.RequestID, request.Parameters)
	case OperationExecutePython:
		return service.executePythonResponse(request.RequestID, request.Parameters)
	default:
		return protocolErrorResponse(request.RequestID, ErrorCodeUnknownOperation)
	}
}

func (service *server) createSandboxResponse(requestID string, rawParameters json.RawMessage) responseEnvelope {
	var parameters createSandboxParameters
	if err := decodeStrictJSON(rawParameters, &parameters, "sandbox_id", "profile_identity"); err != nil || parameters.SandboxID == "" || parameters.ProfileIdentity == "" {
		return protocolErrorResponse(requestID, ErrorCodeMalformedRequest)
	}
	if !validOpaqueIdentifier(parameters.SandboxID) {
		return protocolErrorResponse(requestID, ErrorCodeInvalidReference)
	}
	digest, ok := profileDigest(parameters.ProfileIdentity)
	if !ok {
		return protocolErrorResponse(requestID, ErrorCodeInvalidReference)
	}
	profilePath := path.Join("sha256", digest)
	profileInfo, err := service.profileStore.Lstat(profilePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return protocolErrorResponse(requestID, ErrorCodeProfileNotFound)
		}
		return protocolErrorResponse(requestID, ErrorCodeInvalidReference)
	}
	if profileInfo.Mode()&os.ModeSymlink != 0 || !profileInfo.IsDir() {
		return protocolErrorResponse(requestID, ErrorCodeInvalidReference)
	}
	profile, err := service.profileStore.Open(profilePath)
	if err != nil {
		return protocolErrorResponse(requestID, ErrorCodeInvalidReference)
	}
	_ = profile.Close()

	if !service.creator.enabled() {
		return protocolErrorResponse(requestID, ErrorCodeOperationUnavailable)
	}
	rootfs, err := openProfileRootFilesystem(service.profileStore, digest)
	if err != nil {
		return protocolErrorResponse(requestID, ErrorCodeInvalidReference)
	}
	defer rootfs.Close()
	if err := validateSandboxInit(service.profileStore, digest); err != nil {
		return protocolErrorResponse(requestID, ErrorCodeInvalidReference)
	}
	if code := service.creator.create(parameters.SandboxID, rootfs); code != "" {
		return protocolErrorResponse(requestID, code)
	}
	return responseEnvelope{
		Schema:    ResponseSchemaV1,
		RequestID: requestID,
		Result:    &CreateSandboxResult{SandboxID: parameters.SandboxID},
	}
}

func (service *server) writeResponse(connection *net.UnixConn, response responseEnvelope) {
	payload, err := json.Marshal(response)
	if err != nil {
		return
	}
	_ = writeFrame(connection, payload)
}

func (service *server) writeProtocolError(connection *net.UnixConn, requestID string, code ErrorCode) {
	service.writeResponse(connection, protocolErrorResponse(requestID, code))
}

func protocolErrorResponse(requestID string, code ErrorCode) responseEnvelope {
	return responseEnvelope{
		Schema:    ResponseSchemaV1,
		RequestID: requestID,
		Error:     &ProtocolError{Code: code},
	}
}
