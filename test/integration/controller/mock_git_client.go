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

package controller

import (
	"context"
	"sync"
	"time"

	"github.com/bensonphillipsiv/k8sible.git/internal/git"
)

// MockGitClient is a test double that simulates git operations
type MockGitClient struct {
	mu            sync.RWMutex
	CommitSHA     string
	CommitMessage string
	CommitDate    time.Time
}

// Ensure MockGitClient implements git.ClientInterface
var _ git.ClientInterface = (*MockGitClient)(nil)

// NewMockGitClient creates a new MockGitClient with default values
func NewMockGitClient() *MockGitClient {
	return &MockGitClient{
		CommitSHA:     "abc123def456",
		CommitMessage: "test commit",
		CommitDate:    time.Now(),
	}
}

// GetLatestCommit returns the configured commit info
func (m *MockGitClient) GetLatestCommit(ctx context.Context, source git.Source, token string) (*git.CommitInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return &git.CommitInfo{
		SHA:     m.CommitSHA,
		Message: m.CommitMessage,
		Date:    m.CommitDate,
	}, nil
}

// SetCommit updates the mock to return a new commit (simulates new commits)
func (m *MockGitClient) SetCommit(sha, message string, date time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.CommitSHA = sha
	m.CommitMessage = message
	m.CommitDate = date
}

// SimulateNewCommit generates a new commit SHA to simulate a new commit being pushed
func (m *MockGitClient) SimulateNewCommit() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.CommitSHA = "new" + m.CommitSHA[3:]
	m.CommitMessage = "new test commit"
	m.CommitDate = time.Now()
}
