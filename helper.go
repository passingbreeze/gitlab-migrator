package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/xanzy/go-gitlab"
)

func getGithubPullRequestWithRetry(ctx context.Context, service *MigrationService, org, repo string, prNumber int) (*github.PullRequest, error) {
	var pr *github.PullRequest
	var err error

	backoff := time.Second
	maxRetries := DefaultMaxRetries

	for i := 0; i < maxRetries; i++ {
		pr, err = service.GetGithubPullRequest(ctx, org, repo, prNumber)
		if err == nil {
			return pr, nil
		}

		service.logger.Warn("failed to get PR, retrying", "attempt", i+1, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
			backoff *= 2 // exponential backoff
		}
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", maxRetries, err)
}




func pointer[T any](v T) *T {
	return &v
}

func roundDuration(d, r time.Duration) time.Duration {
	if r <= 0 {
		return d
	}
	neg := d < 0
	if neg {
		d = -d
	}
	if m := d % r; m+m < r {
		d = d - m
	} else {
		d = d + r - m
	}
	if neg {
		return -d
	}
	return d
}

func getProtocolFromURL(urlStr string) string {
	if strings.HasPrefix(urlStr, "http://") {
		return "http"
	}
	return "https" // default is https
}

// URL builder function
func buildURL(domain, path string, forceProtocol string) string {
	protocol := "https" // default value

	// Extract protocol if already included in domain
	if strings.HasPrefix(domain, "http://") {
		domain = strings.TrimPrefix(domain, "http://")
		protocol = "http"
	} else if strings.HasPrefix(domain, "https://") {
		domain = strings.TrimPrefix(domain, "https://")
		protocol = "https"
	}

	// Use forced protocol if specified
	if forceProtocol != "" {
		protocol = forceProtocol
	}

	if path == "" {
		return fmt.Sprintf("%s://%s", protocol, domain)
	}
	return fmt.Sprintf("%s://%s/%s", protocol, domain, path)
}
