package registry

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Client is an HTTP client for the OCI Distribution API. It caches the bearer
// token across requests and re-authenticates automatically when the registry
// returns a 401.
type Client struct {
	host  string
	token string
	http  *http.Client
}

// New returns a Client configured to talk to the given registry host
// (e.g. "registry-1.docker.io").
func New(host string) *Client {
	return &Client{
		host: host,
		http: &http.Client{},
	}
}

// SetToken pre-seeds the bearer token, skipping the initial 401 round-trip.
func (c *Client) SetToken(token string) {
	c.token = token
}

// GetManifest fetches the manifest (or index) for the given repo and reference
// (tag or digest). Returns the Content-Type media type and raw JSON bytes so
// the caller can decide whether it received a Manifest or an Index.
func (c *Client) GetManifest(repo, reference string) (string, []byte, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", c.host, repo, reference)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", strings.Join([]string{MediaTypeOCIManifest, MediaTypeOCIIndex, MediaTypeDockerManifest, MediaTypeDockerIndex}, ", "))

	resp, err := c.doAuthed(req)
	if err != nil {
		return "", nil, fmt.Errorf("get manifest %s/%s: %w", repo, reference, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("registry returned %s for %s: %s", resp.Status, url, string(b))
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("read body: %w", err)
	}

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return "", nil, fmt.Errorf("parse content-type: %w", err)
	}
	return mediaType, b, nil
}

// GetBlob streams the blob identified by digest from the registry. The caller
// must close the returned ReadCloser.
func (c *Client) GetBlob(repo, digest string) (io.ReadCloser, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", c.host, repo, digest)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", "*/*")

	resp, err := c.doAuthed(req)
	if err != nil {
		return nil, fmt.Errorf("get blob %s/%s: %w", repo, digest, err)
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("registry returned %s for %s: %s", resp.Status, url, string(b))
	}

	return resp.Body, nil
}

// doAuthed sends req with the cached bearer token. On a 401 it parses the
// WWW-Authenticate challenge, fetches a fresh token, stores it, and retries
// the request exactly once.
func (c *Client) doAuthed(req *http.Request) (*http.Response, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	challengeHeader := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	ch, err := parseChallenge(challengeHeader)
	if err != nil {
		return nil, fmt.Errorf("parse challenge: %w", err)
	}

	token, err := fetchToken(c.http, ch)
	if err != nil {
		return nil, fmt.Errorf("fetch token: %w", err)
	}
	c.token = token

	req = req.Clone(req.Context())

	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.http.Do(req)
}
