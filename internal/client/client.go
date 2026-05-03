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

type Client struct {
	socketPath string
	httpClient *http.Client
}

const dummyHost = "http://unix"

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

type PingResult struct {
	APIVersion     string
	OSType         string
	BuilderVersion string
}

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
