package client

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"

	"gitlab-migrator/common" // Import common package for utility functions
)

const (
	// HTTP retry configuration
	DefaultRetryMax        = 16
	DefaultRetryWaitMin    = 30 * time.Second
	DefaultRetryWaitMax    = 300 * time.Second
	DefaultRateLimitWait   = 60 * time.Second
	DefaultClockSkewBuffer = 30 * time.Second
)

// NewRetryableHTTPClient creates an HTTP client with retry logic and rate limit handling.
func NewRetryableHTTPClient(logger hclog.Logger) *retryablehttp.Client {
	retryClient := &retryablehttp.Client{
		HTTPClient:   cleanhttp.DefaultPooledClient(),
		Logger:       nil, // Disable retryablehttp's own logging
		RetryMax:     DefaultRetryMax,
		RetryWaitMin: DefaultRetryWaitMin,
		RetryWaitMax: DefaultRetryWaitMax,
	}

	retryClient.Backoff = createBackoffStrategy()
	retryClient.CheckRetry = createRetryChecker()

	return retryClient
}

// createBackoffStrategy creates a custom backoff strategy with rate limit awareness.
func createBackoffStrategy() retryablehttp.Backoff {
	return func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
		// Handle GitHub-style rate limiting with Retry-After header
		if resp != nil {
			if s, ok := resp.Header["Retry-After"]; ok {
				if retryAfter, err := strconv.ParseInt(s[0], 10, 64); err == nil {
					return time.Second * time.Duration(retryAfter)
				}
			}

			// Handle GitHub X-RateLimit headers
			if v, ok := resp.Header["X-Ratelimit-Remaining"]; ok {
				if remaining, err := strconv.ParseInt(v[0], 10, 64); err == nil && remaining == 0 {
					if w, ok := resp.Header["X-Ratelimit-Reset"]; ok {
						if recoveryEpoch, err := strconv.ParseInt(w[0], 10, 64); err == nil {
							sleep := common.RoundDuration( // Use common.RoundDuration
								time.Until(time.Unix(recoveryEpoch+int64(DefaultClockSkewBuffer.Seconds()), 0)),
								time.Second,
							)
							return sleep
						}
					}
					return DefaultRateLimitWait
				}
			}
		}

		// Exponential backoff with jitter
		mult := math.Pow(2, float64(attemptNum)) * float64(min)
		sleep := time.Duration(mult)
		if float64(sleep) != mult || sleep > max {
			sleep = max
		}
		return sleep
	}
}

// createRetryChecker creates a custom retry checker for rate limiting and server errors.
func createRetryChecker() retryablehttp.CheckRetry {
	return func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		// Don't retry on context cancellation or non-HTTP errors
		if err != nil {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			default:
				return false, err
			}
		}

		if resp == nil {
			return true, nil // Potential connection reset
		}

		// Retry on specific HTTP status codes
		retryableStatuses := []int{
			http.StatusTooManyRequests,     // 429 - Rate limiting
			http.StatusForbidden,           // 403 - May indicate rate limiting
			http.StatusRequestTimeout,      // 408 - Request timeout
			http.StatusFailedDependency,    // 424 - Failed dependency
			http.StatusInternalServerError, // 500 - Internal server error
			http.StatusBadGateway,          // 502 - Bad gateway
			http.StatusServiceUnavailable,  // 503 - Service unavailable
			http.StatusGatewayTimeout,      // 504 - Gateway timeout
		}

		for _, status := range retryableStatuses {
			if resp.StatusCode == status {
				return true, nil
			}
		}

		return false, nil
	}
}
