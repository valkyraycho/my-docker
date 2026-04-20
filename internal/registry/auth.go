package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var challengeParamRE = regexp.MustCompile(`(\w+)="([^"]*)"`)

type Challenge struct {
	Realm   string
	Service string
	Scope   string
}

func parseChallenge(header string) (*Challenge, error) {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, "Bearer ") {
		return nil, fmt.Errorf("unsupported auth scheme (expected Bearer): %q", header)
	}

	matches := challengeParamRE.FindAllStringSubmatch(header, -1)

	ch := &Challenge{}
	for _, m := range matches {
		key, val := m[1], m[2]
		switch key {
		case "realm":
			ch.Realm = val
		case "service":
			ch.Service = val
		case "scope":
			ch.Scope = val
		}
	}
	if ch.Realm == "" {
		return nil, errors.New("realm is empty")
	}
	return ch, nil
}

func fetchToken(httpClient *http.Client, ch *Challenge) (string, error) {
	u, err := url.Parse(ch.Realm)
	if err != nil {
		return "", fmt.Errorf("parse realm %s: %w", ch.Realm, err)
	}

	q := u.Query()

	if ch.Service != "" {
		q.Set("service", ch.Service)
	}
	if ch.Scope != "" {
		q.Set("scope", ch.Scope)
	}
	u.RawQuery = q.Encode()

	resp, err := httpClient.Get(u.String())
	if err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("fetch token returned %s for %s: %s", resp.Status, u.String(), string(b))
	}

	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}

	token := tr.Token
	if token == "" {
		token = tr.AccessToken
	}
	if token == "" {
		return "", errors.New("token endpoint returned no token")
	}
	return token, nil
}
