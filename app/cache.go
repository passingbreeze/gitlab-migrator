package app

import (
	"sync"

	"github.com/google/go-github/v69/github"
	"github.com/xanzy/go-gitlab"

	"gitlab-migrator/common"
)

// Cache type constants for different object types.
const (
	githubPullRequestCacheType uint8 = iota
	githubSearchResultsCacheType
	githubUserCacheType
	gitlabUserCacheType
)

// objectCache provides thread-safe caching for various GitHub and GitLab objects.
type objectCache struct {
	mutex *sync.RWMutex
	store map[uint8]map[string]any
}

// newObjectCache creates a new object cache with initialized storage maps.
func newObjectCache() *objectCache {
	store := make(map[uint8]map[string]any)
	store[githubPullRequestCacheType] = make(map[string]any)
	store[githubSearchResultsCacheType] = make(map[string]any)
	store[githubUserCacheType] = make(map[string]any)
	store[gitlabUserCacheType] = make(map[string]any)

	return &objectCache{
		mutex: new(sync.RWMutex),
		store: store,
	}
}

// getGithubPullRequest retrieves a cached GitHub pull request.
func (c *objectCache) getGithubPullRequest(query string) *github.PullRequest {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[githubPullRequestCacheType][query]; ok {
		return common.Pointer(v.(github.PullRequest)) // Use common.Pointer
	}
	return nil
}

// setGithubPullRequest stores a GitHub pull request in cache.
func (c *objectCache) setGithubPullRequest(query string, result github.PullRequest) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[githubPullRequestCacheType][query] = result
}

// getGithubSearchResults retrieves cached GitHub search results.
func (c *objectCache) getGithubSearchResults(query string) *github.IssuesSearchResult {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[githubSearchResultsCacheType][query]; ok {
		return common.Pointer(v.(github.IssuesSearchResult)) // Use common.Pointer
	}
	return nil
}

// setGithubSearchResults stores GitHub search results in cache.
func (c *objectCache) setGithubSearchResults(query string, result github.IssuesSearchResult) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[githubSearchResultsCacheType][query] = result
}

// getGithubUser retrieves a cached GitHub user.
func (c *objectCache) getGithubUser(username string) *github.User {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[githubUserCacheType][username]; ok {
		return common.Pointer(v.(github.User)) // Use common.Pointer
	}
	return nil
}

// setGithubUser stores a GitHub user in cache.
func (c *objectCache) setGithubUser(username string, user github.User) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[githubUserCacheType][username] = user
}

// getGitlabUser retrieves a cached GitLab user.
func (c *objectCache) getGitlabUser(username string) *gitlab.User {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[gitlabUserCacheType][username]; ok {
		return common.Pointer(v.(gitlab.User)) // Use common.Pointer
	}
	return nil
}

// setGitlabUser stores a GitLab user in cache.
func (c *objectCache) setGitlabUser(username string, user gitlab.User) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[gitlabUserCacheType][username] = user
}
