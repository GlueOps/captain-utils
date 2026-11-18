// Package github lists release tags through the GitHub REST API (what
// `gh release list` did for dev mode). A token is optional: GH_TOKEN or
// GITHUB_TOKEN, else the one gh keeps in ~/.config/gh/hosts.yml; without one
// the anonymous rate limit (60 requests an hour) applies.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.yaml.in/yaml/v3"
)

// Client talks to api.github.com (BaseURL is overridable for tests).
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Token   string
}

// New builds a client with the token from the environment or gh's config.
func New() *Client {
	return &Client{BaseURL: "https://api.github.com", HTTP: &http.Client{Timeout: 30 * time.Second}, Token: token()}
}

func token() string {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	dir := os.Getenv("GH_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config", "gh")
	}
	b, err := os.ReadFile(filepath.Join(dir, "hosts.yml"))
	if err != nil {
		return ""
	}
	var hosts map[string]struct {
		OAuthToken string `yaml:"oauth_token"`
	}
	if yaml.Unmarshal(b, &hosts) != nil {
		return ""
	}
	return hosts["github.com"].OAuthToken
}

// ReleaseTags returns the tag names of the newest n published releases of
// owner/repo, newest first (drafts left out).
func (c *Client) ReleaseTags(ctx context.Context, repo string, n int) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/releases?per_page=%d", c.BaseURL, repo, n), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "captain-utils")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list releases of %s: %w", repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list releases of %s: GitHub answered %s", repo, resp.Status)
	}
	var rels []struct {
		TagName string `json:"tag_name"`
		Draft   bool   `json:"draft"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, fmt.Errorf("list releases of %s: %w", repo, err)
	}
	var tags []string
	for _, r := range rels {
		if !r.Draft {
			tags = append(tags, r.TagName)
		}
	}
	return tags, nil
}
