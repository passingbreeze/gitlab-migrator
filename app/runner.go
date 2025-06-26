package app

import (
	"context"
	"fmt"
	"sync"

	"github.com/hashicorp/go-hclog"

	"gitlab-migrator/common" // Import common
)

const (
	QueueBufferMultiplier = common.QueueBufferMultiplier // Use common.QueueBufferMultiplier
)

// Runner handles the execution of migration operations.
type Runner struct {
	logger  hclog.Logger
	service *MigrationService
}

// NewRunner creates a new migration runner.
func NewRunner(logger hclog.Logger, service *MigrationService) *Runner {
	return &Runner{
		logger:  logger,
		service: service,
	}
}

// PerformMigration executes the actual migration process for all projects.
func (r *Runner) PerformMigration(ctx context.Context, projects []Project) error {
	concurrency := r.calculateConcurrency(projects)

	r.logger.Info(fmt.Sprintf("processing %d project(s) with %d workers", len(projects), concurrency))

	var wg sync.WaitGroup
	queue := make(chan Project, concurrency*QueueBufferMultiplier)

	// Start worker goroutines
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go r.worker(ctx, &wg, queue)
	}

	// Queue projects for processing
	go r.queueProjects(ctx, projects, queue)

	// Wait for all workers to complete
	wg.Wait()

	return nil
}

// worker processes projects from the queue.
func (r *Runner) worker(ctx context.Context, wg *sync.WaitGroup, queue <-chan Project) {
	defer wg.Done()

	for proj := range queue {
		if err := ctx.Err(); err != nil {
			r.logger.Debug("worker stopping due to context cancellation")
			break
		}

		projectMigrator := NewProjectMigrator(r.service)
		if err := projectMigrator.MigrateProject(ctx, proj); err != nil {
			r.service.SendError(err)
		}
	}
}

// queueProjects adds projects to the processing queue.
func (r *Runner) queueProjects(ctx context.Context, projects []Project, queue chan<- Project) {
	defer close(queue)

	queueOnce := func() {
		for _, proj := range projects {
			select {
			case <-ctx.Done():
				r.logger.Debug("stopping project queueing due to context cancellation")
				return
			case queue <- proj:
				// Project successfully queued
			}
		}
	}

	if r.service.config.Loop {
		r.logger.Info("looping migration until canceled")
		for {
			select {
			case <-ctx.Done():
				r.logger.Debug("stopping migration loop due to context cancellation")
				return
			default:
				queueOnce()
			}
		}
	} else {
		queueOnce()
	}
}

// calculateConcurrency determines the appropriate concurrency level.
func (r *Runner) calculateConcurrency(projects []Project) int {
	maxConcurrency := r.service.config.MaxConcurrency // This comes from config.Config
	if len(projects) < maxConcurrency {
		return len(projects)
	}
	return maxConcurrency
}
