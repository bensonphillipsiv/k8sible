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
	"fmt"
	"io"
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

// FetchFile fetches a file directly from the repository's raw URL
func (c *Client) FetchFile(ctx context.Context, source Source) (string, error) {
	rawURL, err := buildRawFileURL(source.Repository, source.Path, source.Reference)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch file: HTTP %d", resp.StatusCode)
	}

	contents, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	return string(contents), nil
}

// buildRawFileURL constructs the raw file URL based on the git provider
func buildRawFileURL(repository, path, reference string) (string, error) {
	parsed, err := url.Parse(repository)
	if err != nil {
		return "", fmt.Errorf("failed to parse repository URL: %w", err)
	}

	// Default reference to main if not specified
	if reference == "" {
		reference = "main"
	}

	// Remove .git suffix if present
	repoPath := strings.TrimSuffix(parsed.Path, ".git")

	host := strings.ToLower(parsed.Host)

	switch {
	case strings.Contains(host, "github.com"):
		// GitHub: https://raw.githubusercontent.com/{owner}/{repo}/{ref}/{path}
		return fmt.Sprintf("https://raw.githubusercontent.com%s/%s/%s", repoPath, reference, path), nil

	case strings.Contains(host, "gitlab.com"):
		// GitLab: https://gitlab.com/{owner}/{repo}/-/raw/{ref}/{path}
		return fmt.Sprintf("https://gitlab.com%s/-/raw/%s/%s", repoPath, reference, path), nil

	case strings.Contains(host, "bitbucket.org"):
		// Bitbucket: https://bitbucket.org/{owner}/{repo}/raw/{ref}/{path}
		return fmt.Sprintf("https://bitbucket.org%s/raw/%s/%s", repoPath, reference, path), nil

	default:
		// For self-hosted or unknown providers, try a generic GitLab-style URL
		return fmt.Sprintf("%s://%s%s/-/raw/%s/%s", parsed.Scheme, parsed.Host, repoPath, reference, path), nil
	}
}
