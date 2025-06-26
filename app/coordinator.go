package app

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-hclog"

	"gitlab-migrator/client"
	"gitlab-migrator/config"
)

// Coordinator manages the overall migration process.
type Coordinator struct {
	logger  hclog.Logger
	config  *config.Config
	service *MigrationService
}

// NewCoordinator creates a new migration coordinator.
func NewCoordinator(logger hclog.Logger, cfg *config.Config) (*Coordinator, error) {
	service, err := setupMigrationService(logger, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to setup migration service: %w", err)
	}

	return &Coordinator{
		logger:  logger,
		config:  cfg,
		service: service,
	}, nil
}

// Run executes the migration process according to configuration.
func (c *Coordinator) Run(ctx context.Context) error {
	projects, err := LoadProjects(c.config.ProjectsCsvPath, c.config.GitlabProject, c.config.GithubRepo)
	if err != nil {
		c.service.SendError(err)
		return err
	}

	if c.config.Report {
		return c.generateReport(ctx, projects)
	}

	return c.performMigration(ctx, projects)
}

// generateReport generates and prints a migration report.
func (c *Coordinator) generateReport(ctx context.Context, projects []Project) error {
	reporter := NewReporter(c.logger, c.service)
	reporter.PrintReport(ctx, projects)
	return nil
}

// performMigration executes the actual migration process.
func (c *Coordinator) performMigration(ctx context.Context, projects []Project) error {
	runner := NewRunner(c.logger, c.service)
	if err := runner.PerformMigration(ctx, projects); err != nil {
		c.service.SendError(err)
	}

	if c.service.HasErrors() {
		return fmt.Errorf("encountered %d errors during migration, review log output for details", c.service.GetErrorCount())
	}

	return nil
}

// setupMigrationService creates and configures the migration service with all dependencies.
func setupMigrationService(logger hclog.Logger, cfg *config.Config) (*MigrationService, error) {
	// Create GitHub client
	gh, err := client.NewGitHubClient(client.GitHubClientConfig{
		Token:  cfg.GithubToken,
		Domain: cfg.GithubDomain,
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create GitHub client: %w", err)
	}

	// Create GitLab client
	gl, err := client.NewGitLabClient(client.GitLabClientConfig{
		Token:  cfg.GitlabToken,
		Domain: cfg.GitlabDomain,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create GitLab client: %w", err)
	}

	// Create authentication manager
	authManager := NewAuthManager(cfg.GithubToken, cfg.GitlabToken, cfg.GithubUser)

	// Create migration service
	migrationConfig := cfg.GetMigrationConfig()
	service := NewMigrationService(logger, gh, gl, authManager, migrationConfig)

	return service, nil
}
