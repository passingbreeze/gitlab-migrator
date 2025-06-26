package config

import (
	"flag"
	"os"
)

const (
	// Default values
	DefaultGithubDomain = "github.com"
	DefaultGitlabDomain = "gitlab.com"
	DefaultConcurrency  = 4
)

// ParseFlags parses command line flags and environment variables into a Config struct.
func ParseFlags() (*Config, error) {
	cfg := &Config{}

	// Parse command-line flags
	flag.BoolVar(&cfg.Loop, "loop", false, "continue migrating until canceled")
	flag.BoolVar(&cfg.Report, "report", false, "report on primitives to be migrated instead of beginning migration")
	flag.BoolVar(&cfg.DeleteExistingRepos, "delete-existing-repos", false, "whether existing repositories should be deleted before migrating")
	flag.BoolVar(&cfg.EnablePullRequests, "migrate-pull-requests", false, "whether pull requests should be migrated")
	flag.BoolVar(&cfg.RenameMasterToMain, "rename-master-to-main", false, "rename master branch to main and update pull requests")

	flag.StringVar(&cfg.GithubDomain, "github-domain", DefaultGithubDomain, "specifies the GitHub domain to use")
	flag.StringVar(&cfg.GithubRepo, "github-repo", "", "the GitHub repository to migrate to")
	flag.StringVar(&cfg.GithubUser, "github-user", os.Getenv("GITHUB_USER"), "specifies the GitHub user to use, who will author any migrated PRs (required)")
	flag.StringVar(&cfg.GitlabDomain, "gitlab-domain", DefaultGitlabDomain, "specifies the GitLab domain to use")
	flag.StringVar(&cfg.GitlabProject, "gitlab-project", "", "the GitLab project to migrate")
	flag.StringVar(&cfg.ProjectsCsvPath, "projects-csv", "", "specifies the path to a CSV file describing projects to migrate (incompatible with -gitlab-project and -github-repo)")

	flag.IntVar(&cfg.MaxConcurrency, "max-concurrency", DefaultConcurrency, "how many projects to migrate in parallel")

	flag.Parse()

	// Load environment variables
	cfg.GithubToken = os.Getenv("GITHUB_TOKEN")
	cfg.GitlabToken = os.Getenv("GITLAB_TOKEN")

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
