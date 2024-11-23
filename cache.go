package main

import (
	"sync"

	"github.com/google/go-github/v66/github"
	"github.com/xanzy/go-gitlab"
)

const (
	gitlabUserCacheType uint8 = iota
	githubUserCacheType
)

type objectCache struct {
	mutex *sync.RWMutex
	store map[uint8]map[string]any
}

func newObjectCache() *objectCache {
	store := make(map[uint8]map[string]any)
	store[gitlabUserCacheType] = make(map[string]any)
	store[githubUserCacheType] = make(map[string]any)

	return &objectCache{
		mutex: new(sync.RWMutex),
		store: store,
	}
}

func (c objectCache) getGithubUser(username string) *github.User {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[gitlabUserCacheType][username]; ok {
		return v.(*github.User)
	}
	return nil
}

func (c objectCache) setGithubUser(username string, user *github.User) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[gitlabUserCacheType][username] = user
}

func (c objectCache) getGitlabUser(username string) *gitlab.User {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[githubUserCacheType][username]; ok {
		return v.(*gitlab.User)
	}
	return nil
}

func (c objectCache) setGitlabUser(username string, user *gitlab.User) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[githubUserCacheType][username] = user
}
