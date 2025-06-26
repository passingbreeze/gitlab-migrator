package common

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
)

// Pointer returns a pointer to the given value (generic helper function).
func Pointer[T any](v T) *T {
	return &v
}

// RoundDuration rounds duration d to the nearest multiple of r.
func RoundDuration(d, r time.Duration) time.Duration {
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

// BuildURL constructs a URL from domain, path, and optional protocol override.
func BuildURL(domain, path string, forceProtocol string) string {
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

// LogAndWrapError logs an error with context and wraps it with additional information.
func LogAndWrapError(err error, logger hclog.Logger, msg string, keysAndValues ...interface{}) error {
	if err == nil {
		return nil
	}
	logger.Error(msg, append(keysAndValues, "error", err)...)
	return fmt.Errorf("%s: %w", msg, err)
}
