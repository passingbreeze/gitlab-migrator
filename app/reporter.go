package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/go-hclog"
	"github.com/xanzy/go-gitlab"
)

// Reporter handles generation and display of migration reports.
type Reporter struct {
	logger  hclog.Logger
	service *MigrationService
}

// Report contains migration report information for a project.
type Report struct {
	GroupName          string
	ProjectName        string
	MergeRequestsCount int
}

// NewReporter creates a new migration reporter.
func NewReporter(logger hclog.Logger, service *MigrationService) *Reporter {
	return &Reporter{
		logger:  logger,
		service: service,
	}
}

// PrintReport prints a summary report of projects to be migrated.
func (r *Reporter) PrintReport(ctx context.Context, projects []Project) {
	r.logger.Debug("building migration report")

	results := make([]Report, 0)

	for _, proj := range projects {
		if err := ctx.Err(); err != nil {
			return
		}

		result, err := r.reportProject(ctx, proj)
		if err != nil {
			r.service.SendError(err)
			continue
		}

		if result != nil {
			results = append(results, *result)
		}
	}

	r.displayReport(results)
}

// reportProject generates a migration report for a single project.
func (r *Reporter) reportProject(ctx context.Context, proj []string) (*Report, error) {
	// Use the full path for finding the project, as it's more robust
	gitlabPathFull := proj[0]

	// Project name is the last part of the path
	pathParts := strings.Split(gitlabPathFull, "/")
	projectName := pathParts[len(pathParts)-1]

	r.logger.Debug("searching for GitLab project", "projectName", projectName, "fullPath", gitlabPathFull)

	project, err := r.findGitlabProject(gitlabPathFull, projectName) // Pass fullPath and projectName
	if err != nil {
		return nil, err
	}

	mergeRequestsCount, err := r.countMergeRequests(project)
	if err != nil {
		return nil, err
	}

	// Extract group and project name from PathWithNamespace for the report struct
	// This handles nested groups correctly.
	reportPathParts := strings.Split(project.PathWithNamespace, "/")
	groupName := strings.Join(reportPathParts[:len(reportPathParts)-1], "/")
	if groupName == "" && len(reportPathParts) == 1 { // Case for project directly under root, no group
		groupName = "N/A" // Or some other indicator
	}

	return &Report{
		GroupName:          groupName,
		ProjectName:        reportPathParts[len(reportPathParts)-1],
		MergeRequestsCount: mergeRequestsCount,
	}, nil
}

// findGitlabProject searches for a GitLab project by name and path.
func (r *Reporter) findGitlabProject(fullPath, projectName string) (*gitlab.Project, error) {
	searchTerm := projectName
	projectResult, _, err := r.service.gitlabClient.Projects.ListProjects(&gitlab.ListProjectsOptions{Search: &searchTerm})
	if err != nil {
		return nil, fmt.Errorf("listing projects: %v", err)
	}

	for _, item := range projectResult {
		if item == nil {
			continue
		}

		if item.PathWithNamespace == fullPath {
			r.logger.Debug("found GitLab project",
				"project", item.PathWithNamespace, // Use PathWithNamespace directly
				"project_id", item.ID)
			return item, nil
		}
	}

	return nil, fmt.Errorf("no matching GitLab project found: %s", fullPath)
}

// countMergeRequests retrieves the count of merge requests for a project.
func (r *Reporter) countMergeRequests(project *gitlab.Project) (int, error) {
	var totalCount int
	opts := &gitlab.ListProjectMergeRequestsOptions{
		OrderBy: gitlab.String("created_at"),
		Sort:    gitlab.String("asc"),
	}

	r.logger.Debug("counting GitLab merge requests",
		"project", project.PathWithNamespace,
		"project_id", project.ID)

	for {
		result, resp, err := r.service.gitlabClient.MergeRequests.ListProjectMergeRequests(project.ID, opts)
		if err != nil {
			return 0, fmt.Errorf("retrieving gitlab merge requests: %v", err)
		}

		totalCount += len(result)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return totalCount, nil
}

// displayReport prints the formatted report to stdout.
func (r *Reporter) displayReport(results []Report) {
	fmt.Println()

	totalMergeRequests := 0
	for _, result := range results {
		totalMergeRequests += result.MergeRequestsCount
		fmt.Printf("%#v\n", result)
	}

	fmt.Println()
	fmt.Printf("Total merge requests: %d\n", totalMergeRequests)
	fmt.Println()
}
