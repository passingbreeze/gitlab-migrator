package main

import (
	"context"
	"fmt"

	"github.com/google/go-github/v69/github"
	"github.com/hashicorp/go-hclog"
	"github.com/xanzy/go-gitlab"
)

// MigrationService contains all dependencies needed for migration operations
type MigrationService struct {
	logger      hclog.Logger
	cache       *objectCache
	authManager *AuthManager
	githubClient *github.Client
	gitlabClient *gitlab.Client
	config      *MigrationConfig
	errorTracker *ErrorTracker
}

// MigrationConfig holds all configuration for the migration
type MigrationConfig struct {
	GithubDomain        string
	GitlabDomain        string
	GithubUser          string
	MaxConcurrency      int
	DeleteExistingRepos bool
	EnablePullRequests  bool
	RenameMasterToMain  bool
	Loop                bool
	Report              bool
}

// ErrorTracker handles error counting and reporting
type ErrorTracker struct {
	count  int
	logger hclog.Logger
}

func NewErrorTracker(logger hclog.Logger) *ErrorTracker {
	return &ErrorTracker{
		logger: logger,
	}
}

func (e *ErrorTracker) SendError(err error) {
	e.count++
	e.logger.Error(err.Error())
}

func (e *ErrorTracker) GetCount() int {
	return e.count
}

func (e *ErrorTracker) HasErrors() bool {
	return e.count > 0
}

// NewMigrationService creates a new migration service with all dependencies
func NewMigrationService(
	logger hclog.Logger,
	githubClient *github.Client,
	gitlabClient *gitlab.Client,
	authManager *AuthManager,
	config *MigrationConfig,
) *MigrationService {
	return &MigrationService{
		logger:       logger,
		cache:        newObjectCache(),
		authManager:  authManager,
		githubClient: githubClient,
		gitlabClient: gitlabClient,
		config:       config,
		errorTracker: NewErrorTracker(logger),
	}
}

// GetGithubUser retrieves a GitHub user with caching
func (s *MigrationService) GetGithubUser(ctx context.Context, username string) (*github.User, error) {
	user := s.cache.getGithubUser(username)
	if user == nil {
		s.logger.Debug("retrieving user details", "username", username)
		var err error
		if user, _, err = s.githubClient.Users.Get(ctx, username); err != nil {
			return nil, err
		}

		if user == nil {
			return nil, fmt.Errorf("nil user was returned: %s", username)
		}

		s.logger.Trace("caching GitHub user", "username", username)
		s.cache.setGithubUser(username, *user)
	}

	if user.Type == nil {
		return nil, fmt.Errorf("unable to determine whether owner is a user or organisation: %s", username)
	}

	return user, nil
}

// GetGitlabUser retrieves a GitLab user with caching
func (s *MigrationService) GetGitlabUser(username string) (*gitlab.User, error) {
	user := s.cache.getGitlabUser(username)
	if user == nil {
		s.logger.Debug("retrieving user details", "username", username)
		users, _, err := s.gitlabClient.Users.ListUsers(&gitlab.ListUsersOptions{Username: &username})
		if err != nil {
			return nil, err
		}

		for _, user = range users {
			if user != nil && user.Username == username {
				s.logger.Trace("caching GitLab user", "username", username)
				s.cache.setGitlabUser(username, *user)
				return user, nil
			}
		}

		return nil, fmt.Errorf("GitLab user not found: %s", username)
	}

	return user, nil
}

// GetGithubPullRequest retrieves a GitHub pull request with caching
func (s *MigrationService) GetGithubPullRequest(ctx context.Context, org, repo string, prNumber int) (*github.PullRequest, error) {
	cacheToken := fmt.Sprintf("%s/%s/%d", org, repo, prNumber)
	pullRequest := s.cache.getGithubPullRequest(cacheToken)
	if pullRequest == nil {
		s.logger.Debug("retrieving pull request details", "owner", org, "repo", repo, "pr_number", prNumber)
		var err error
		pullRequest, _, err = s.githubClient.PullRequests.Get(ctx, org, repo, prNumber)
		if err != nil {
			return nil, fmt.Errorf("retrieving pull request: %v", err)
		}

		if pullRequest == nil {
			return nil, fmt.Errorf("nil pull request was returned: %d", prNumber)
		}

		s.logger.Trace("caching pull request details", "owner", org, "repo", repo, "pr_number", prNumber)
		s.cache.setGithubPullRequest(cacheToken, *pullRequest)
	}

	return pullRequest, nil
}

// GetGithubSearchResults retrieves GitHub search results with caching
func (s *MigrationService) GetGithubSearchResults(ctx context.Context, query string) (*github.IssuesSearchResult, error) {
	result := s.cache.getGithubSearchResults(query)
	if result == nil {
		s.logger.Debug("performing search", "query", query)
		var err error
		result, _, err = s.githubClient.Search.Issues(ctx, query, nil)
		if err != nil {
			return nil, fmt.Errorf("performing issue search: %v", err)
		}

		if result == nil {
			return nil, fmt.Errorf("nil search result was returned for query: %s", query)
		}

		s.logger.Trace("caching GitHub search result", "query", query)
		s.cache.setGithubSearchResults(query, *result)
	}

	return result, nil
}

// SendError records an error and logs it
func (s *MigrationService) SendError(err error) {
	s.errorTracker.SendError(err)
}

// GetErrorCount returns the current error count
func (s *MigrationService) GetErrorCount() int {
	return s.errorTracker.GetCount()
}

// HasErrors returns true if any errors have been recorded
func (s *MigrationService) HasErrors() bool {
	return s.errorTracker.HasErrors()
}