package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/hashicorp/go-hclog"

	"gitlab-migrator/app"
	"gitlab-migrator/client"
	"gitlab-migrator/config"
)

// main is the entry point for the GitLab to GitHub migration tool.
func main() {
	logger := hclog.New(&hclog.LoggerOptions{
		Name:  "gitlab-migrator",
		Level: hclog.LevelFromString(os.Getenv("LOG_LEVEL")),
	})

	cfg, err := config.ParseFlags()
	if err != nil {
		logger.Error("Configuration error", "error", err)
		os.Exit(1)
	}

	// Set up context for cancellation via Ctrl-C
	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	defer func() {
		signal.Stop(c)
		cancel()
	}()
	go func() {
		select {
		case <-c:
			logger.Info("Shutdown signal received, canceling operations...")
			cancel()
		case <-ctx.Done():
		}
	}()

	// Set up GitHub context with rate limit bypass
	ctx = client.SetupGitHubContext(ctx)

	// Create and run the migration coordinator
	coordinator, err := app.NewCoordinator(logger, cfg)
	if err != nil {
		logger.Error("Failed to create coordinator", "error", err)
		os.Exit(1)
	}

	if err := coordinator.Run(ctx); err != nil {
		logger.Error("Application returned an error", "error", err)
		os.Exit(1)
	}

	logger.Info("Migration finished successfully.")
}
