package workerapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

type Client struct {
	baseURL, token string
	client         *http.Client
}

func NewClient(baseURL, token string, client *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(token, "\r\n") || token == "" {
		return nil, errors.New("Worker API requires an HTTPS origin and a credential")
	}
	if client == nil {
		client = &http.Client{Timeout: 35 * time.Second}
	}
	copyClient := *client
	if copyClient.Timeout == 0 {
		copyClient.Timeout = 35 * time.Second
	}
	transportSource := copyClient.Transport
	if transportSource == nil {
		transportSource = http.DefaultTransport
	}
	transport, ok := transportSource.(*http.Transport)
	if !ok || transport.DialTLS != nil || transport.DialTLSContext != nil || (transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify) {
		return nil, errors.New("Worker API requires a certificate-validating HTTP transport")
	}
	transport = transport.Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		if transport.TLSClientConfig.RootCAs != nil {
			transport.TLSClientConfig.RootCAs = transport.TLSClientConfig.RootCAs.Clone()
		}
		transport.TLSClientConfig.MinVersion = max(transport.TLSClientConfig.MinVersion, tls.VersionTLS12)
	}
	copyClient.Transport = transport
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: &copyClient}, nil
}

func (c *Client) Claim(ctx context.Context, wait time.Duration) (*Lease, error) {
	if wait < 0 || wait > 25*time.Second {
		return nil, errors.New("claim wait must be between 0 and 25s")
	}
	var lease Lease
	status, err := c.post(ctx, "/worker/v1/claim", claimRequest{wait.Milliseconds()}, &lease)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	if lease.ExecutionID == "" || lease.AgentRunID == "" || lease.Generation <= 0 || lease.ExpiresAt.IsZero() || lease.Source == "" {
		return nil, errors.New("Worker API returned an incomplete Execution Lease")
	}
	return &lease, nil
}
func (c *Client) Validate(ctx context.Context, lease Lease) error {
	_, err := c.post(ctx, "/worker/v1/validate", leaseReference{lease.ExecutionID, lease.Generation}, nil)
	return err
}
func (c *Client) Release(ctx context.Context, lease Lease) error {
	_, err := c.post(ctx, "/worker/v1/release", leaseReference{lease.ExecutionID, lease.Generation}, nil)
	return err
}
func (c *Client) Capacity(ctx context.Context) (Capacity, error) {
	var capacity Capacity
	status, err := c.post(ctx, "/worker/v1/capacity", capacityRequest{}, &capacity)
	if err != nil {
		return Capacity{}, err
	}
	if status != http.StatusOK || capacity.WorkerID == "" || capacity.Configured < 1 || capacity.Configured > 64 || capacity.Occupied < 0 || capacity.Available != max(0, capacity.Configured-capacity.Occupied) {
		return Capacity{}, errors.New("Worker API returned invalid Sandbox capacity")
	}
	return capacity, nil
}
func (c *Client) ConfigureCapacity(ctx context.Context, capacity int) error {
	_, err := c.post(ctx, "/worker/v1/configure-capacity", configureCapacityRequest{Capacity: capacity}, nil)
	return err
}
func (c *Client) BindSandbox(ctx context.Context, lease Lease, sandboxID string) error {
	_, err := c.post(ctx, "/worker/v1/bind-sandbox", sandboxReference{lease.ExecutionID, lease.Generation, sandboxID}, nil)
	return err
}
func (c *Client) Outstanding(ctx context.Context) ([]OutstandingExecution, error) {
	var entries []OutstandingExecution
	status, err := c.post(ctx, "/worker/v1/outstanding", capacityRequest{}, &entries)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || entries == nil || len(entries) > 64 {
		return nil, errors.New("Worker API returned invalid outstanding Executions")
	}
	for _, entry := range entries {
		if entry.Lease.ExecutionID == "" || entry.Lease.AgentRunID == "" || entry.Lease.Generation <= 0 || entry.Lease.ExpiresAt.IsZero() || entry.Lease.Source != "" || entry.Lease.Stdin != "" || len(entry.Lease.OutputPaths) != 0 {
			return nil, errors.New("Worker API returned invalid outstanding identity")
		}
		switch entry.State {
		case "leased", "recovering", "worker_lost", "completed":
		default:
			return nil, errors.New("Worker API returned invalid outstanding state")
		}
	}
	return entries, nil
}
func (c *Client) Heartbeat(ctx context.Context, lease Lease, sandboxID string) (Authority, error) {
	return c.authority(ctx, "/worker/v1/heartbeat", lease, sandboxID)
}
func (c *Client) Recover(ctx context.Context, lease Lease, sandboxID string) (Authority, error) {
	return c.authority(ctx, "/worker/v1/recover", lease, sandboxID)
}
func (c *Client) authority(ctx context.Context, route string, lease Lease, sandboxID string) (Authority, error) {
	var authority Authority
	status, err := c.post(ctx, route, sandboxReference{lease.ExecutionID, lease.Generation, sandboxID}, &authority)
	if err != nil {
		return Authority{}, err
	}
	if status != http.StatusOK || authority.Lease.ExecutionID != lease.ExecutionID || authority.Lease.AgentRunID == "" || authority.Lease.Generation != lease.Generation || authority.ServerTime.IsZero() || !authority.RecoveryUntil.After(authority.Lease.ExpiresAt) || (authority.State != "leased" && authority.State != "completed") || (authority.State == "leased" && !authority.Lease.ExpiresAt.After(authority.ServerTime)) {
		return Authority{}, errors.New("Worker API returned invalid Execution authority")
	}
	return authority, nil
}
func (c *Client) Complete(ctx context.Context, lease Lease, result sandboxsupervisor.ExecutePythonResult) error {
	_, err := c.post(ctx, "/worker/v1/complete", completeRequest{lease.ExecutionID, lease.Generation, result}, nil)
	return err
}
func (c *Client) post(ctx context.Context, route string, value, target any) (int, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return 0, errors.New("cannot encode Worker request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+route, bytes.NewReader(raw))
	if err != nil {
		return 0, errors.New("cannot prepare Worker request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		var certificate *tls.CertificateVerificationError
		var unknown x509.UnknownAuthorityError
		if errors.As(err, &certificate) || errors.As(err, &unknown) {
			return 0, errors.New("Worker API server certificate verification failed")
		}
		return 0, ErrTransport
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return response.StatusCode, &HTTPError{StatusCode: response.StatusCode, Code: http.StatusText(response.StatusCode)}
	}
	if response.StatusCode == http.StatusNoContent {
		return response.StatusCode, nil
	}
	if target == nil {
		return 0, errors.New("Worker API returned an unexpected acknowledgement status")
	}
	if target != nil {
		// Read one byte beyond the public envelope limit before decoding. A
		// LimitReader's synthetic EOF must never turn an oversized response
		// into a retryable truncated transport response.
		const maximumResponseBytes = 64 << 10
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if len(body) > maximumResponseBytes {
			return 0, errors.New("Worker API response exceeded its envelope limit")
		}
		if readErr != nil {
			var networkError net.Error
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) || errors.As(readErr, &networkError) {
				return 0, ErrTransport
			}
			return 0, errors.New("cannot read Worker API response")
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, ErrTransport
			}
			return 0, errors.New("invalid Worker API response")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return 0, errors.New("invalid Worker API response trailing content")
		}
	}
	return response.StatusCode, nil
}
