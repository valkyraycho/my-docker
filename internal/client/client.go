// Package client is the Go library the CLI uses to talk to the mydocker daemon.
// It wraps each daemon HTTP endpoint in a typed method so callers never
// construct raw HTTP requests directly.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/valkyraycho/my-docker/internal/api"
)

// Client communicates with the mydocker daemon over a UNIX socket.
// All methods accept a context so callers can apply deadlines or cancellation.
type Client struct {
	socketPath string
	httpClient *http.Client
}

// dummyHost is a placeholder base URL used when constructing http.Request URLs.
// The HTTP transport ignores the host entirely and dials the UNIX socket instead.
const dummyHost = "http://unix"

// New returns a Client that dials socketPath for every request.
// The underlying http.Transport is configured to ignore the host in the URL
// and connect over the UNIX socket instead.
func New(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{Transport: transport},
	}
}

// PingResult holds the capability fields extracted from a GET /_ping response.
// Docker returns these as HTTP headers, not a JSON body; we surface them here
// as a typed struct so the CLI can branch on APIVersion or OSType.
type PingResult struct {
	APIVersion     string
	OSType         string
	BuilderVersion string
}

// Ping calls GET /_ping and returns the daemon's API version, OS type, and
// builder version. It is used at startup to verify the daemon is reachable.
func (c *Client) Ping(ctx context.Context) (*PingResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dummyHost+"/_ping", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("daemon returned %s: %s", resp.Status, string(b))
	}

	var result PingResult

	result.APIVersion = resp.Header.Get("Api-Version")
	result.OSType = resp.Header.Get("Ostype")
	result.BuilderVersion = resp.Header.Get("Builder-Version")

	return &result, nil
}

// ContainerCreate calls POST /containers/create with req as the JSON body.
// On success (201 Created) it returns the new container's ID and any warnings.
// Non-201 responses are decoded as an ErrorResponse and returned as an error.
func (c *Client) ContainerCreate(ctx context.Context, req *api.ContainerCreateRequest) (*api.ContainerCreateResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, dummyHost+"/containers/create", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		var errBody api.ErrorResponse
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&errBody)

		return nil, fmt.Errorf("daemon returned %s: %s", resp.Status, errBody.Message)
	}

	var result api.ContainerCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return &result, nil
}

func (c *Client) ContainerStart(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dummyHost+"/containers/"+id+"/start", nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotModified:
		return nil
	default:
		var errBody api.ErrorResponse
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&errBody)
		return fmt.Errorf("daemon returned %s: %s", resp.Status, errBody.Message)
	}
}

func (c *Client) ContainerList(ctx context.Context, all bool) ([]api.ContainerSummary, error) {
	url := dummyHost + "/containers/json"
	if all {
		url += "?all=1"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody api.ErrorResponse
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&errBody)
		return nil, fmt.Errorf("daemon returned %s: %s", resp.Status, errBody.Message)
	}

	var result []api.ContainerSummary
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return result, nil
}

func (c *Client) ContainerInspect(ctx context.Context, id string) (*api.ContainerInspect, error) {
	url := dummyHost + "/containers/" + id + "/json"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody api.ErrorResponse
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&errBody)
		return nil, fmt.Errorf("daemon returned %s: %s", resp.Status, errBody.Message)
	}

	var result api.ContainerInspect
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return &result, nil
}
