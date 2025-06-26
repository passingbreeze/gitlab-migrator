package client

import (
	"context"
	"fmt"

	"github.com/gofri/go-github-pagination/githubpagination"
	"github.com/google/go-github/v69/github"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"

	"gitlab-migrator/common" // Import common
)

const (
	DefaultPerPage = 100
)

// GitHubClientConfig contains configuration for creating a GitHub client.
type GitHubClientConfig struct {
	Token  string
	Domain string
	Logger hclog.Logger
}

// NewGitHubClient creates a new GitHub client with retry logic and rate limit handling.
func NewGitHubClient(config GitHubClientConfig) (*github.Client, error) {
	if config.Token == "" {
		return nil, fmt.Errorf("GitHub token is required")
	}

	retryClient := NewRetryableHTTPClient(config.Logger)
	httpClient := githubpagination.NewClient(
		&retryablehttp.RoundTripper{Client: retryClient},
		githubpagination.WithPerPage(DefaultPerPage),
	)

	var gh *github.Client
	if config.Domain == common.DefaultGithubDomain { // Use common.DefaultGithubDomain
		gh = github.NewClient(httpClient).WithAuthToken(config.Token)
	} else {
		githubURL := common.BuildURL(config.Domain, "", "") // Use common.BuildURL
		var err error
		if gh, err = github.NewClient(httpClient).WithAuthToken(config.Token).WithEnterpriseURLs(githubURL, githubURL); err != nil {
			return nil, fmt.Errorf("failed to create GitHub enterprise client: %w", err)
		}
	}

	return gh, nil
}

// SetupGitHubContext sets up context with rate limit bypass for GitHub client.
func SetupGitHubContext(ctx context.Context) context.Context {
	// Bypass pre-emptive rate limit checks in the GitHub client
	return context.WithValue(ctx, github.BypassRateLimitCheck, true)
}
