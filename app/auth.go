package app

import (
	"context"
	"fmt"
	"net/url"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// AuthManager manages authentication for GitLab and GitHub APIs.
type AuthManager struct {
	githubToken string
	gitlabToken string
	githubUser  string
}

// NewAuthManager creates a new authentication manager with the provided tokens and user.
func NewAuthManager(githubToken, gitlabToken, githubUser string) *AuthManager {
	return &AuthManager{
		githubToken: githubToken,
		gitlabToken: gitlabToken,
		githubUser:  githubUser,
	}
}

// GetGitlabAuth returns HTTP basic auth credentials for GitLab.
func (a *AuthManager) GetGitlabAuth() *http.BasicAuth {
	return &http.BasicAuth{
		Username: "oauth2",
		Password: a.gitlabToken,
	}
}

// GetGithubAuth returns HTTP basic auth credentials for GitHub.
func (a *AuthManager) GetGithubAuth() *http.BasicAuth {
	return &http.BasicAuth{
		Username: a.githubUser,
		Password: a.githubToken,
	}
}

// PushToGithub pushes changes to GitHub repository with proper authentication.
func (a *AuthManager) PushToGithub(ctx context.Context, repo *git.Repository, remoteName string, options *git.PushOptions) error {
	if options == nil {
		options = &git.PushOptions{}
	}

	options.Auth = a.GetGithubAuth()
	options.RemoteName = remoteName

	return repo.PushContext(ctx, options)
}

// AddGithubRemote adds a GitHub remote to the repository.
func (a *AuthManager) AddGithubRemote(repo *git.Repository, remoteName, owner, repoName, domain string) error {
	githubURL := fmt.Sprintf("https://%s/%s/%s", domain, owner, repoName)

	_, err := repo.CreateRemote(&config.RemoteConfig{
		Name: remoteName,
		URLs: []string{githubURL},
	})

	return err
}

// GetSafeCloneURL returns a clone URL with embedded authentication credentials.
func (a *AuthManager) GetSafeCloneURL(originalURL string) (string, error) {
	parsedURL, err := url.Parse(originalURL)
	if err != nil {
		return "", fmt.Errorf("parsing clone URL: %v", err)
	}

	parsedURL.User = url.UserPassword("oauth2", a.gitlabToken)
	return parsedURL.String(), nil
}
