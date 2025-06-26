package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/go-github/v69/github"
	"github.com/xanzy/go-gitlab"

	"gitlab-migrator/common" // Import common
)

// Constants from the common package
const (
	MasterBranchName = common.MasterBranchName
	MainBranchName   = common.MainBranchName
)

// ProjectMigrator handles the migration of individual projects.
type ProjectMigrator struct {
	service *MigrationService
}

// NewProjectMigrator creates a new project migrator with the provided service.
func NewProjectMigrator(service *MigrationService) *ProjectMigrator {
	return &ProjectMigrator{service: service}
}

// MigrateProject migrates a single project from GitLab to GitHub.
func (pm *ProjectMigrator) MigrateProject(ctx context.Context, proj []string) error {
	gitlabPathFull := proj[0]
	githubPath := strings.Split(proj[1], "/")

	pm.service.logger.Info("starting project migration",
		"gitlabPath", gitlabPathFull,
		"githubPath", proj[1],
		"gitlabDomain", pm.service.config.GitlabDomain)

	// Project name is the last part of the path
	pathParts := strings.Split(gitlabPathFull, "/")
	projectName := pathParts[len(pathParts)-1]

	pm.service.logger.Info("searching for GitLab project",
		"projectName", projectName,
		"fullPath", gitlabPathFull,
		"gitlabDomain", pm.service.config.GitlabDomain)

	project, err := pm.findGitlabProject(gitlabPathFull, projectName)
	if err != nil {
		return err
	}

	pm.service.logger.Info("mirroring repository from GitLab to GitHub",
		"project", project.PathWithNamespace,
		"github_org", githubPath[0],
		"github_repo", githubPath[1],
		"gitlabDomain", pm.service.config.GitlabDomain)

	// Validate GitHub user/organization
	if err := pm.validateGithubOwner(ctx, githubPath[0]); err != nil {
		return err
	}

	if err := pm.handleGithubRepository(ctx, githubPath, project); err != nil {
		return err
	}

	repo, err := pm.cloneAndPushRepository(ctx, project, githubPath)
	if err != nil {
		return err
	}

	// Migrate pull requests if enabled
	if pm.service.config.EnablePullRequests {
		prMigrator := NewPullRequestMigrator(pm.service)
		// FIXED: Removed the extra `gitlabPath` argument to match the function signature.
		prMigrator.MigratePullRequests(ctx, githubPath, project, repo)
	}

	return nil
}

func (pm *ProjectMigrator) findGitlabProject(gitlabPathFull, projectName string) (*gitlab.Project, error) {
	searchTerm := projectName
	projectResult, _, err := pm.service.gitlabClient.Projects.ListProjects(&gitlab.ListProjectsOptions{Search: &searchTerm})
	if err != nil {
		return nil, fmt.Errorf("listing projects: %v", err)
	}

	var project *gitlab.Project
	for _, item := range projectResult {
		if item == nil {
			continue
		}

		if item.PathWithNamespace == gitlabPathFull {
			pm.service.logger.Debug("found GitLab project",
				"project", item.PathWithNamespace,
				"project_id", item.ID,
				"gitlabDomain", pm.service.config.GitlabDomain)
			project = item
			break
		}
	}

	if project == nil {
		return nil, fmt.Errorf("GitLab project not found: %s (gitlabDomain: %s)", gitlabPathFull, pm.service.config.GitlabDomain)
	}

	return project, nil
}

func (pm *ProjectMigrator) validateGithubOwner(ctx context.Context, owner string) error {
	user, err := pm.service.GetGithubUser(ctx, owner)
	if err != nil {
		return fmt.Errorf("retrieving github user: %v", err)
	}

	if !strings.EqualFold(*user.Type, "organization") &&
		(!strings.EqualFold(*user.Type, "user") || !strings.EqualFold(*user.Login, owner)) {
		return fmt.Errorf("configured owner is neither an organization nor the current user: %s", owner)
	}

	return nil
}

func (pm *ProjectMigrator) handleGithubRepository(ctx context.Context, githubPath []string, project *gitlab.Project) error {
	pm.service.logger.Debug("checking for existing repository on GitHub",
		"owner", githubPath[0],
		"repo", githubPath[1],
		"gitlabDomain", pm.service.config.GitlabDomain)

	_, _, err := pm.service.githubClient.Repositories.Get(ctx, githubPath[0], githubPath[1])

	var githubError *github.ErrorResponse
	if err != nil && (!errors.As(err, &githubError) || githubError == nil || githubError.Response == nil || githubError.Response.StatusCode != http.StatusNotFound) {
		return fmt.Errorf("retrieving github repo: %v", err)
	}

	var createRepo, repoDeleted bool
	if err != nil {
		createRepo = true
	} else if pm.service.config.DeleteExistingRepos {
		pm.service.logger.Warn("existing repository found on GitHub, proceeding with deletion",
			"owner", githubPath[0],
			"repo", githubPath[1],
			"gitlabDomain", pm.service.config.GitlabDomain)
		if _, err = pm.service.githubClient.Repositories.Delete(ctx, githubPath[0], githubPath[1]); err != nil {
			return fmt.Errorf("deleting existing github repo: %v", err)
		}

		createRepo = true
		repoDeleted = true
	}

	defaultBranch := MainBranchName
	if !pm.service.config.RenameMasterToMain && project.DefaultBranch != "" {
		defaultBranch = project.DefaultBranch
	}

	homepage := common.BuildURL(pm.service.config.GitlabDomain, project.PathWithNamespace, "") // Use common.BuildURL

	if createRepo {
		if err := pm.createGithubRepository(ctx, githubPath, project, defaultBranch, homepage, repoDeleted); err != nil {
			return err
		}
	}

	return pm.updateRepositorySettings(ctx, githubPath, project, homepage)
}

func (pm *ProjectMigrator) createGithubRepository(ctx context.Context, githubPath []string, project *gitlab.Project, defaultBranch, homepage string, repoDeleted bool) error {
	var org string
	user, _ := pm.service.GetGithubUser(ctx, githubPath[0])
	if strings.EqualFold(*user.Type, "organization") {
		org = githubPath[0]
	}

	if repoDeleted {
		pm.service.logger.Warn("recreating GitHub repository", "owner", githubPath[0], "repo", githubPath[1])
	} else {
		pm.service.logger.Debug("repository not found on GitHub, proceeding to create", "owner", githubPath[0], "repo", githubPath[1])
	}

	newRepo := github.Repository{
		Name:          common.Pointer(githubPath[1]), // Use common.Pointer
		Description:   &project.Description,
		Homepage:      &homepage,
		DefaultBranch: &defaultBranch,
		Private:       common.Pointer(true), // Use common.Pointer
		HasIssues:     common.Pointer(true), // Use common.Pointer
		HasProjects:   common.Pointer(true), // Use common.Pointer
		HasWiki:       common.Pointer(true), // Use common.Pointer
	}

	if _, _, err := pm.service.githubClient.Repositories.Create(ctx, org, &newRepo); err != nil {
		return fmt.Errorf("creating github repo: %v", err)
	}

	return nil
}

func (pm *ProjectMigrator) updateRepositorySettings(ctx context.Context, githubPath []string, project *gitlab.Project, homepage string) error {
	pm.service.logger.Debug("updating repository settings", "owner", githubPath[0], "repo", githubPath[1])

	updateRepo := github.Repository{
		Name:              common.Pointer(githubPath[1]), // Use common.Pointer
		Description:       &project.Description,
		Homepage:          &homepage,
		AllowAutoMerge:    common.Pointer(true), // Use common.Pointer
		AllowMergeCommit:  common.Pointer(true), // Use common.Pointer
		AllowRebaseMerge:  common.Pointer(true), // Use common.Pointer
		AllowSquashMerge:  common.Pointer(true), // Use common.Pointer
		AllowUpdateBranch: common.Pointer(true), // Use common.Pointer
	}

	if _, _, err := pm.service.githubClient.Repositories.Edit(ctx, githubPath[0], githubPath[1], &updateRepo); err != nil {
		return fmt.Errorf("updating github repo: %v", err)
	}

	return nil
}

// cloneAndPushRepository clones the GitLab repository and pushes it to GitHub.
func (pm *ProjectMigrator) cloneAndPushRepository(ctx context.Context, project *gitlab.Project, githubPath []string) (*git.Repository, error) {
	cloneUrlWithCredentials, err := pm.service.authManager.GetSafeCloneURL(project.HTTPURLToRepo)
	if err != nil {
		return nil, fmt.Errorf("preparing clone URL: %v", err)
	}

	fs := memfs.New()

	pm.service.logger.Debug("cloning repository", "path", project.PathWithNamespace, "url", project.HTTPURLToRepo)
	repo, err := git.CloneContext(ctx, memory.NewStorage(), fs, &git.CloneOptions{
		URL:        cloneUrlWithCredentials,
		Auth:       pm.service.authManager.GetGitlabAuth(),
		RemoteName: "gitlab",
		Mirror:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("cloning gitlab repo: %v", err)
	}

	if pm.service.config.RenameMasterToMain {
		if err := pm.renameMasterBranch(repo, project); err != nil {
			return nil, err
		}
	}

	githubUrl := common.BuildURL(pm.service.config.GithubDomain, fmt.Sprintf("%s/%s", githubPath[0], githubPath[1]), "") // Use common.BuildURL

	pm.service.logger.Debug("adding remote for GitHub repository", "project", project.PathWithNamespace, "url", githubUrl)
	if err = pm.service.authManager.AddGithubRemote(repo, "github", githubPath[0], githubPath[1], pm.service.config.GithubDomain); err != nil {
		return nil, fmt.Errorf("adding github remote: %v", err)
	}

	pm.service.logger.Debug("force-pushing to GitHub repository", "project", project.PathWithNamespace, "url", githubUrl)
	if err = pm.service.authManager.PushToGithub(ctx, repo, "github", &git.PushOptions{
		Force: true,
	}); err != nil {
		if err.Error() == "already up-to-date" {
			pm.service.logger.Debug("repository already up-to-date on GitHub", "project", project.PathWithNamespace, "url", githubUrl)
		} else {
			return nil, fmt.Errorf("pushing to github repo: %v", err)
		}
	}

	defaultBranch := MainBranchName
	if !pm.service.config.RenameMasterToMain && project.DefaultBranch != "" {
		defaultBranch = project.DefaultBranch
	}

	pm.service.logger.Debug("setting default repository branch", "owner", githubPath[0], "repo", githubPath[1], "branch_name", defaultBranch)
	updateRepo := github.Repository{
		DefaultBranch: &defaultBranch,
	}
	if _, _, err = pm.service.githubClient.Repositories.Edit(ctx, githubPath[0], githubPath[1], &updateRepo); err != nil {
		return nil, fmt.Errorf("setting default branch: %v", err)
	}

	return repo, nil
}

// renameMasterBranch renames the master branch to main if it exists.
func (pm *ProjectMigrator) renameMasterBranch(repo *git.Repository, project *gitlab.Project) error {
	if masterBranch, err := repo.Reference(plumbing.NewBranchReferenceName(MasterBranchName), false); err == nil {
		pm.service.logger.Info("renaming master branch to main prior to push", "project", project.PathWithNamespace, "sha", masterBranch.Hash())

		pm.service.logger.Debug("creating main branch", "project", project.PathWithNamespace, "sha", masterBranch.Hash())
		mainBranch := plumbing.NewHashReference(plumbing.NewBranchReferenceName(MainBranchName), masterBranch.Hash())
		if err = repo.Storer.SetReference(mainBranch); err != nil {
			return fmt.Errorf("creating main branch: %v", err)
		}

		pm.service.logger.Debug("deleting master branch", "project", project.PathWithNamespace, "sha", masterBranch.Hash())
		if err = repo.Storer.RemoveReference(masterBranch.Name()); err != nil {
			return fmt.Errorf("deleting master branch: %v", err)
		}
	}
	return nil
}
