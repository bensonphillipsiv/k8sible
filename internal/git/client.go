/*
Copyright 2025 bensonphillipsiv.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package git

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Source represents a git repository source
type Source struct {
	Repository string
	Path       string
	Reference  string
}

// CommitInfo contains information about a commit
type CommitInfo struct {
	SHA     string
	Message string
	Date    time.Time
}

// Client handles git operations
type Client struct {
	httpClient *http.Client
}

// ClientOption is a functional option for configuring the Client
type ClientOption func(*Client)

// WithHTTPClient sets a custom HTTP client
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = httpClient
	}
}

// NewClient creates a new git client
func NewClient(opts ...ClientOption) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// GetLatestCommit fetches the latest commit SHA for a specific path in the repository
func (c *Client) GetLatestCommit(ctx context.Context, source Source, token string) (*CommitInfo, error) {
	apiURL, err := buildGitHubCommitAPIURL(source)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "k8sible-controller")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch commit info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch commit info: HTTP %d", resp.StatusCode)
	}

	var commits []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&commits); err != nil {
		return nil, fmt.Errorf("failed to decode GitHub response: %w", err)
	}

	if len(commits) == 0 {
		return nil, fmt.Errorf("no commits found for path")
	}

	return &CommitInfo{
		SHA:     commits[0].SHA,
		Message: commits[0].Commit.Message,
		Date:    commits[0].Commit.Author.Date,
	}, nil
}

// buildGitHubCommitAPIURL constructs the GitHub API URL to fetch commit info for a path
func buildGitHubCommitAPIURL(source Source) (string, error) {
	parsed, err := url.Parse(source.Repository)
	if err != nil {
		return "", fmt.Errorf("failed to parse repository URL: %w", err)
	}

	reference := source.Reference
	if reference == "" {
		reference = "main"
	}

	// Remove .git suffix if present
	repoPath := strings.TrimSuffix(parsed.Path, ".git")

	// GitHub API: GET /repos/{owner}/{repo}/commits?path={path}&sha={ref}&per_page=1
	return fmt.Sprintf("https://api.github.com/repos%s/commits?path=%s&sha=%s&per_page=1",
		repoPath, url.QueryEscape(source.Path), url.QueryEscape(reference)), nil
}
