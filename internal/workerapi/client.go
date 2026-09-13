package workerapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
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
		decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(target) != nil {
			return 0, errors.New("invalid Worker API response")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return 0, errors.New("invalid Worker API response trailing content")
		}
	}
	return response.StatusCode, nil
}
