package registry

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

type Client struct {
	host  string
	token string
	http  *http.Client
}

func New(host string) *Client {
	return &Client{
		host: host,
		http: &http.Client{},
	}
}

func (c *Client) SetToken(token string) {
	c.token = token
}

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
