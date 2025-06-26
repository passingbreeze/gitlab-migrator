package client

import (
	"fmt"

	"github.com/xanzy/go-gitlab"

	"gitlab-migrator/common" // Import common
)

// GitLabClientConfig contains configuration for creating a GitLab client.
type GitLabClientConfig struct {
	Token  string
	Domain string
}

// NewGitLabClient creates a new GitLab client with proper configuration.
func NewGitLabClient(config GitLabClientConfig) (*gitlab.Client, error) {
	if config.Token == "" {
		return nil, fmt.Errorf("GitLab token is required")
	}

	gitlabOpts := make([]gitlab.ClientOptionFunc, 0)

	// Configure custom domain if not using gitlab.com
	if config.Domain != common.DefaultGitlabDomain { // Use common.DefaultGitlabDomain
		gitlabURL := common.BuildURL(config.Domain, "", "") // Use common.BuildURL
		gitlabOpts = append(gitlabOpts, gitlab.WithBaseURL(gitlabURL))
	}

	gl, err := gitlab.NewClient(config.Token, gitlabOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitLab client: %w", err)
	}

	return gl, nil
}
