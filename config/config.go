package config

import (
	"fmt"
	"strings"
)

// Config contains all configuration from flags and environment variables.
type Config struct {
	// Behavior flags
	Loop                bool
	Report              bool
	DeleteExistingRepos bool
	EnablePullRequests  bool
	RenameMasterToMain  bool

	// Domain configuration
	GithubDomain string
	GitlabDomain string

	// Repository configuration
	GithubRepo    string
	GithubUser    string
	GitlabProject string

	// File configuration
	ProjectsCsvPath string

	// Performance configuration
	MaxConcurrency int

	// Authentication
	GithubToken string
	GitlabToken string
}

// Validate validates the configuration and returns an error if invalid.
func (c *Config) Validate() error {
	if c.GithubToken == "" {
		return fmt.Errorf("missing environment variable: GITHUB_TOKEN")
	}

	if c.GitlabToken == "" {
		return fmt.Errorf("missing environment variable: GITLAB_TOKEN")
	}

	if c.GithubUser == "" {
		return fmt.Errorf("must specify GitHub user via -github-user flag or GITHUB_USER environment variable")
	}

	if err := c.validateProjectConfiguration(); err != nil {
		return err
	}

	if err := c.validateProjectPaths(); err != nil {
		return err
	}

	return nil
}

// validateProjectConfiguration ensures either CSV or inline project configuration is provided.
func (c *Config) validateProjectConfiguration() error {
	repoSpecifiedInline := c.GithubRepo != "" && c.GitlabProject != ""

	if repoSpecifiedInline && c.ProjectsCsvPath != "" {
		return fmt.Errorf("cannot specify -projects-csv and either -github-repo or -gitlab-project at the same time")
	}

	if !repoSpecifiedInline && c.ProjectsCsvPath == "" {
		return fmt.Errorf("must specify either -projects-csv or both of -github-repo and -gitlab-project")
	}

	return nil
}

// validateProjectPaths validates the format of project paths.
func (c *Config) validateProjectPaths() error {
	if c.GitlabProject != "" {
		if err := validateProjectPath(c.GitlabProject); err != nil {
			return fmt.Errorf("invalid GitLab project path: %w", err)
		}
	}

	if c.GithubRepo != "" {
		if err := validateProjectPath(c.GithubRepo); err != nil {
			return fmt.Errorf("invalid GitHub repository path: %w", err)
		}
	}

	return nil
}

// validateProjectPath validates that a project path has the correct format.
func validateProjectPath(path string) error {
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid project path format, expected 'group/project': %s", path)
	}
	return nil
}

// GetMigrationConfig converts Config to MigrationConfig for the service layer.
func (c *Config) GetMigrationConfig() *MigrationConfig {
	return &MigrationConfig{
		GithubDomain:        c.GithubDomain,
		GitlabDomain:        c.GitlabDomain,
		GithubUser:          c.GithubUser,
		MaxConcurrency:      c.MaxConcurrency,
		DeleteExistingRepos: c.DeleteExistingRepos,
		EnablePullRequests:  c.EnablePullRequests,
		RenameMasterToMain:  c.RenameMasterToMain,
		Loop:                c.Loop,
		Report:              c.Report,
	}
}

// MigrationConfig holds all configuration for the migration service.
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
