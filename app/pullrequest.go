package app

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-github/v69/github"
	"github.com/xanzy/go-gitlab"

	"gitlab-migrator/common" // Import common
)

// Constants from common
const (
	DateFormat                  = common.DateFormat
	MigrationSourceBranchPrefix = common.MigrationSourceBranchPrefix
	MigrationTargetBranchPrefix = common.MigrationTargetBranchPrefix
	DefaultDescription          = common.DefaultDescription
	DefaultApprovers            = common.DefaultApprovers
	MaxTitleLength              = common.MaxTitleLength
	DefaultPerPage              = common.DefaultPerPage
)

// PullRequestMigrator handles the migration of pull requests and comments.
type PullRequestMigrator struct {
	service *MigrationService
}

// NewPullRequestMigrator creates a new pull request migrator with the provided service.
func NewPullRequestMigrator(service *MigrationService) *PullRequestMigrator {
	return &PullRequestMigrator{service: service}
}

// MigratePullRequests migrates all merge requests from GitLab to GitHub pull requests.
func (prm *PullRequestMigrator) MigratePullRequests(ctx context.Context, githubPath []string, project *gitlab.Project, repo *git.Repository) {
	mergeRequests, err := prm.fetchMergeRequests(project)
	if err != nil {
		prm.service.SendError(err)
		return
	}

	var successCount, failureCount int
	totalCount := len(mergeRequests)
	prm.service.logger.Info("migrating merge requests from GitLab to GitHub", "project", project.PathWithNamespace, "count", totalCount)

	for _, mergeRequest := range mergeRequests {
		if mergeRequest == nil {
			continue
		}

		if err := ctx.Err(); err != nil {
			prm.service.SendError(fmt.Errorf("context canceled during merge request migration: %w", err))
			break
		}

		if err := prm.migrateSingleMergeRequest(ctx, githubPath, project, repo, mergeRequest); err != nil {
			prm.service.SendError(err)
			failureCount++
		} else {
			successCount++
		}
	}

	skippedCount := totalCount - successCount - failureCount
	prm.service.logger.Info("finished migrating merge requests", "project", project.PathWithNamespace, "successful", successCount, "failed", failureCount, "skipped", skippedCount)
}

// fetchMergeRequests retrieves all merge requests for a project from GitLab.
func (prm *PullRequestMigrator) fetchMergeRequests(project *gitlab.Project) ([]*gitlab.MergeRequest, error) {
	var mergeRequests []*gitlab.MergeRequest

	opts := &gitlab.ListProjectMergeRequestsOptions{
		OrderBy: gitlab.String("created_at"),
		Sort:    gitlab.String("asc"),
	}

	prm.service.logger.Debug("retrieving GitLab merge requests", "project", project.PathWithNamespace, "project_id", project.ID)
	for {
		result, resp, err := prm.service.gitlabClient.MergeRequests.ListProjectMergeRequests(project.ID, opts)
		if err != nil {
			return nil, fmt.Errorf("retrieving gitlab merge requests for project %s: %w", project.PathWithNamespace, err)
		}

		mergeRequests = append(mergeRequests, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return mergeRequests, nil
}

// migrateSingleMergeRequest migrates a single merge request from GitLab to GitHub.
func (prm *PullRequestMigrator) migrateSingleMergeRequest(ctx context.Context, githubPath []string, project *gitlab.Project, repo *git.Repository, mergeRequest *gitlab.MergeRequest) error {
	var cleanUpBranch bool
	var pullRequest *github.PullRequest

	prm.service.logger.Debug("searching for any existing pull request", "owner", githubPath[0], "repo", githubPath[1], "merge_request_id", mergeRequest.IID)

	existingPR, err := prm.findExistingPullRequest(ctx, githubPath, mergeRequest)
	if err != nil {
		return fmt.Errorf("searching for existing pull request: %w", err)
	}
	pullRequest = existingPR

	if pullRequest == nil && !strings.EqualFold(mergeRequest.State, "opened") {
		tempBranchCreated, err := prm.createTemporaryBranches(ctx, githubPath, repo, project, mergeRequest)
		if err != nil {
			return err
		}
		cleanUpBranch = tempBranchCreated
	}

	if prm.service.config.RenameMasterToMain && mergeRequest.TargetBranch == MasterBranchName {
		prm.service.logger.Trace("changing target branch from master to main", "project", project.PathWithNamespace, "merge_request_id", mergeRequest.IID)
		mergeRequest.TargetBranch = MainBranchName
	}

	pullRequest, err = prm.createOrUpdatePullRequest(ctx, githubPath, project, mergeRequest, pullRequest)
	if err != nil {
		return err
	}

	if cleanUpBranch {
		if err := prm.cleanupTemporaryBranches(ctx, repo, githubPath, pullRequest, mergeRequest); err != nil {
			return err
		}
	}

	return prm.migrateComments(ctx, githubPath, project, pullRequest, mergeRequest)
}

// findExistingPullRequest searches for an existing GitHub pull request for the given GitLab merge request.
func (prm *PullRequestMigrator) findExistingPullRequest(ctx context.Context, githubPath []string, mergeRequest *gitlab.MergeRequest) (*github.PullRequest, error) {
	query := fmt.Sprintf("repo:%s/%s is:pr", githubPath[0], githubPath[1])
	searchResult, err := prm.service.GetGithubSearchResults(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("listing pull requests: %w", err)
	}

	for _, issue := range searchResult.Issues {
		if issue == nil || !issue.IsPullRequest() {
			continue
		}

		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("preparing to retrieve pull request: %w", err)
		}

		prUrl, err := url.Parse(issue.GetPullRequestLinks().GetURL())
		if err != nil {
			return nil, fmt.Errorf("parsing pull request url: %w", err)
		}

		if m := regexp.MustCompile(".+/([0-9]+)$").FindStringSubmatch(prUrl.Path); len(m) == 2 {
			prNumber, _ := strconv.Atoi(m[1])
			pr, err := prm.service.GetGithubPullRequest(ctx, githubPath[0], githubPath[1], prNumber)
			if err != nil {
				return nil, fmt.Errorf("retrieving pull request: %w", err)
			}

			if strings.Contains(pr.GetBody(), fmt.Sprintf("**GitLab MR ID** | %d", mergeRequest.IID)) {
				prm.service.logger.Debug("found existing pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pr.GetNumber())
				return pr, nil
			}
		}
	}

	return nil, nil
}

// createTemporaryBranches creates temporary source and target branches for closed/merged merge requests.
func (prm *PullRequestMigrator) createTemporaryBranches(ctx context.Context, githubPath []string, repo *git.Repository, project *gitlab.Project, mergeRequest *gitlab.MergeRequest) (bool, error) {
	prm.service.logger.Trace("searching for existing branch for closed/merged merge request", "project", project.PathWithNamespace, "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch)

	if _, err := repo.Reference(plumbing.NewBranchReferenceName(mergeRequest.SourceBranch), false); err != nil {
		worktree, err := repo.Worktree()
		if err != nil {
			return false, fmt.Errorf("creating worktree: %w", err)
		}

		mergeRequest.SourceBranch = fmt.Sprintf(MigrationSourceBranchPrefix, mergeRequest.IID, mergeRequest.SourceBranch)
		mergeRequest.TargetBranch = fmt.Sprintf(MigrationTargetBranchPrefix, mergeRequest.IID, mergeRequest.TargetBranch)

		prm.service.logger.Trace("retrieving commits for merge request", "project", project.PathWithNamespace, "merge_request_id", mergeRequest.IID)
		mergeRequestCommits, _, err := prm.service.gitlabClient.MergeRequests.GetMergeRequestCommits(project.ID, mergeRequest.IID, &gitlab.GetMergeRequestCommitsOptions{OrderBy: "created_at", Sort: "asc"})
		if err != nil {
			return false, fmt.Errorf("retrieving merge request commits: %w", err)
		}

		if len(mergeRequestCommits) == 0 {
			return false, nil
		}

		sort.Slice(mergeRequestCommits, func(i, j int) bool {
			return mergeRequestCommits[i].CommittedDate.Before(*mergeRequestCommits[j].CommittedDate)
		})

		if mergeRequestCommits[0] == nil {
			return false, fmt.Errorf("start commit for merge request %d is nil", mergeRequest.IID)
		}
		if mergeRequestCommits[len(mergeRequestCommits)-1] == nil {
			return false, fmt.Errorf("end commit for merge request %d is nil", mergeRequest.IID)
		}

		prm.service.logger.Trace("inspecting start commit", "project", project.PathWithNamespace, "merge_request_id", mergeRequest.IID, "sha", mergeRequestCommits[0].ShortID)
		startCommit, err := object.GetCommit(repo.Storer, plumbing.NewHash(mergeRequestCommits[0].ID))
		if err != nil {
			return false, fmt.Errorf("loading start commit: %w", err)
		}

		if startCommit.NumParents() > 1 {
			return false, fmt.Errorf("start commit %s for merge request %d has %d parents", mergeRequestCommits[0].ShortID, mergeRequest.IID, startCommit.NumParents())
		}

		if startCommit.NumParents() == 0 {
			return false, nil
		}

		startCommitParent, err := startCommit.Parent(0)
		if err != nil {
			return false, fmt.Errorf("loading parent commit: %w", err)
		}

		prm.service.logger.Trace("creating target branch for merged/closed merge request", "project", project.PathWithNamespace, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.TargetBranch, "sha", startCommitParent.Hash)
		if err = worktree.Checkout(&git.CheckoutOptions{
			Create: true,
			Force:  true,
			Branch: plumbing.NewBranchReferenceName(mergeRequest.TargetBranch),
			Hash:   startCommitParent.Hash,
		}); err != nil {
			return false, fmt.Errorf("checking out temporary target branch: %w", err)
		}

		endHash := plumbing.NewHash(mergeRequestCommits[len(mergeRequestCommits)-1].ID)
		prm.service.logger.Trace("creating source branch for merged/closed merge request", "project", project.PathWithNamespace, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.SourceBranch, "sha", endHash)
		if err = worktree.Checkout(&git.CheckoutOptions{
			Create: true,
			Force:  true,
			Branch: plumbing.NewBranchReferenceName(mergeRequest.SourceBranch),
			Hash:   endHash,
		}); err != nil {
			return false, fmt.Errorf("checking out temporary source branch: %w", err)
		}

		githubOwner, githubRepo := githubPath[0], githubPath[1]
		prm.service.logger.Debug("pushing branches for merged/closed merge request", "owner", githubOwner, "repo", githubRepo, "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
		if err = prm.service.authManager.PushToGithub(ctx, repo, "github", &git.PushOptions{
			RefSpecs: []config.RefSpec{
				config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.SourceBranch)),
				config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.TargetBranch)),
			},
			Force: true,
		}); err != nil {
			if err.Error() == "already up-to-date" {
				prm.service.logger.Trace("branch already exists and is up-to-date on GitHub", "owner", githubOwner, "repo", githubRepo, "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
			} else {
				return false, fmt.Errorf("pushing temporary branches to github: %w", err)
			}
		}

		return true, nil
	}

	return false, nil
}

// createOrUpdatePullRequest creates a new pull request or updates an existing one.
func (prm *PullRequestMigrator) createOrUpdatePullRequest(ctx context.Context, githubPath []string, project *gitlab.Project, mergeRequest *gitlab.MergeRequest, existingPR *github.PullRequest) (*github.PullRequest, error) {
	body, err := prm.buildPullRequestBody(project, mergeRequest)
	if err != nil {
		return nil, err
	}

	if existingPR == nil {
		return prm.createPullRequest(ctx, githubPath, mergeRequest, body)
	}

	return prm.updatePullRequest(ctx, githubPath, mergeRequest, existingPR, body)
}

// buildPullRequestBody constructs the pull request body with GitLab metadata.
func (prm *PullRequestMigrator) buildPullRequestBody(project *gitlab.Project, mergeRequest *gitlab.MergeRequest) (string, error) {
	githubAuthorName := mergeRequest.Author.Name

	author, err := prm.service.GetGitlabUser(mergeRequest.Author.Username)
	if err != nil {
		return "", fmt.Errorf("retrieving gitlab user: %w", err)
	}
	if author.WebsiteURL != "" {
		githubAuthorName = "@" + strings.TrimPrefix(strings.ToLower(author.WebsiteURL), "https://github.com/")
	}

	originalState := ""
	if !strings.EqualFold(mergeRequest.State, "opened") {
		originalState = fmt.Sprintf("> This merge request was originally **%s** on GitLab", mergeRequest.State)
	}

	approvers, err := prm.getApprovers(project, mergeRequest.IID)
	if err != nil {
		return "", err
	}

	description := mergeRequest.Description
	if strings.TrimSpace(description) == "" {
		description = DefaultDescription
	}

	slices.Sort(approvers)
	approval := strings.Join(approvers, ", ")
	if approval == "" {
		approval = DefaultApprovers
	}

	closeDate := ""
	if mergeRequest.State == "closed" && mergeRequest.ClosedAt != nil {
		closeDate = fmt.Sprintf("\n> | **Date Originally Closed** | %s |", mergeRequest.ClosedAt.Format(DateFormat))
	} else if mergeRequest.State == "merged" && mergeRequest.MergedAt != nil {
		closeDate = fmt.Sprintf("\n> | **Date Originally Merged** | %s |", mergeRequest.MergedAt.Format(DateFormat))
	}

	mergeRequestTitle := mergeRequest.Title
	if len(mergeRequestTitle) > MaxTitleLength {
		mergeRequestTitle = mergeRequestTitle[:MaxTitleLength] + "..."
	}

	body := fmt.Sprintf(`> [!NOTE]
> This pull request was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %s |
> | **GitLab Project** | %s |
> | **GitLab MR ID** | %d |
> | **Original GitLab MR** | %s |
> | **Date Originally Created** | %s |%s
> | **Original Approvers** | %s |
>
%s

## Original Description

%s`,
		githubAuthorName,
		project.PathWithNamespace,
		mergeRequest.IID,
		mergeRequestTitle, mergeRequest.WebURL,
		mergeRequest.CreatedAt.Format(DateFormat), closeDate,
		approval,
		originalState,
		description,
	)

	return body, nil
}

func (prm *PullRequestMigrator) getApprovers(project *gitlab.Project, mergeRequestIID int) ([]string, error) {
	prm.service.logger.Debug("determining merge request approvers", "project", project.PathWithNamespace, "merge_request_id", mergeRequestIID)

	approvers := make([]string, 0)
	awards, _, err := prm.service.gitlabClient.AwardEmoji.ListMergeRequestAwardEmoji(project.ID, mergeRequestIID, &gitlab.ListAwardEmojiOptions{PerPage: DefaultPerPage})
	if err != nil {
		return approvers, fmt.Errorf("listing merge request awards: %w", err)
	}

	for _, award := range awards {
		if award.Name == "thumbsup" {
			approver := award.User.Name

			approverUser, err := prm.service.GetGitlabUser(award.User.Username)
			if err != nil {
				prm.service.SendError(fmt.Errorf("retrieving gitlab user: %w", err))
				continue
			}
			if approverUser.WebsiteURL != "" {
				approver = "@" + strings.TrimPrefix(strings.ToLower(approverUser.WebsiteURL), "https://github.com/")
			}

			approvers = append(approvers, approver)
		}
	}

	return approvers, nil
}

func (prm *PullRequestMigrator) createPullRequest(ctx context.Context, githubPath []string, mergeRequest *gitlab.MergeRequest, body string) (*github.PullRequest, error) {
	owner, repo := githubPath[0], githubPath[1]
	prm.service.logger.Info("creating pull request", "owner", owner, "repo", repo, "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)

	newPullRequest := github.NewPullRequest{
		Title:               &mergeRequest.Title,
		Head:                &mergeRequest.SourceBranch,
		Base:                &mergeRequest.TargetBranch,
		Body:                &body,
		MaintainerCanModify: common.Pointer(true), // Use common.Pointer
		Draft:               &mergeRequest.Draft,
	}

	pullRequest, _, err := prm.service.githubClient.PullRequests.Create(ctx, owner, repo, &newPullRequest)
	if err != nil {
		return nil, fmt.Errorf("creating pull request: %w", err)
	}

	if mergeRequest.State == "closed" || mergeRequest.State == "merged" {
		prm.service.logger.Debug("closing pull request", "owner", owner, "repo", repo, "pr_number", pullRequest.GetNumber())

		pullRequest.State = common.Pointer("closed") // Use common.Pointer
		if pullRequest, _, err = prm.service.githubClient.PullRequests.Edit(ctx, owner, repo, pullRequest.GetNumber(), pullRequest); err != nil {
			return nil, fmt.Errorf("updating pull request state to closed: %w", err)
		}
	}

	return pullRequest, nil
}

func (prm *PullRequestMigrator) updatePullRequest(ctx context.Context, githubPath []string, mergeRequest *gitlab.MergeRequest, pullRequest *github.PullRequest, body string) (*github.PullRequest, error) {
	var newState *string
	switch mergeRequest.State {
	case "opened":
		newState = common.Pointer("open") // Use common.Pointer
	case "closed", "merged":
		newState = common.Pointer("closed") // Use common.Pointer
	}

	if (newState != nil && pullRequest.GetState() != *newState) ||
		pullRequest.GetTitle() != mergeRequest.Title ||
		pullRequest.GetBody() != body ||
		pullRequest.GetDraft() != mergeRequest.Draft {
		owner, repo := githubPath[0], githubPath[1]
		prm.service.logger.Info("updating pull request", "owner", owner, "repo", repo, "pr_number", pullRequest.GetNumber())

		pullRequest.Title = &mergeRequest.Title
		pullRequest.Body = &body
		pullRequest.Draft = &mergeRequest.Draft
		pullRequest.State = newState

		var err error
		if pullRequest, _, err = prm.service.githubClient.PullRequests.Edit(ctx, owner, repo, pullRequest.GetNumber(), pullRequest); err != nil {
			return nil, fmt.Errorf("updating pull request: %w", err)
		}
	} else {
		prm.service.logger.Trace("existing pull request is up-to-date", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())
	}

	return pullRequest, nil
}

func (prm *PullRequestMigrator) cleanupTemporaryBranches(ctx context.Context, repo *git.Repository, githubPath []string, pullRequest *github.PullRequest, mergeRequest *gitlab.MergeRequest) error {
	owner, ghRepo := githubPath[0], githubPath[1]
	prm.service.logger.Debug("deleting temporary branches for closed pull request", "owner", owner, "repo", ghRepo, "pr_number", pullRequest.GetNumber(), "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)

	if err := prm.service.authManager.PushToGithub(ctx, repo, "github", &git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.SourceBranch)),
			config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.TargetBranch)),
		},
		Force: true,
	}); err != nil {
		if err.Error() == "already up-to-date" {
			prm.service.logger.Trace("branches already deleted on GitHub", "owner", owner, "repo", ghRepo, "pr_number", pullRequest.GetNumber(), "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
		} else {
			return fmt.Errorf("pushing branch deletions to github: %w", err)
		}
	}

	return nil
}

func (prm *PullRequestMigrator) migrateComments(ctx context.Context, githubPath []string, project *gitlab.Project, pullRequest *github.PullRequest, mergeRequest *gitlab.MergeRequest) error {
	comments, err := prm.fetchComments(project, mergeRequest.IID)
	if err != nil {
		return err
	}

	owner, repo := githubPath[0], githubPath[1]
	prNumber := pullRequest.GetNumber()

	prm.service.logger.Debug("retrieving GitHub pull request comments", "owner", owner, "repo", repo, "pr_number", prNumber)
	prComments, _, err := prm.service.githubClient.Issues.ListComments(ctx, owner, repo, prNumber, &github.IssueListCommentsOptions{Sort: common.Pointer("created"), Direction: common.Pointer("asc")}) // Use common.Pointer
	if err != nil {
		return fmt.Errorf("listing pull request comments: %w", err)
	}

	prm.service.logger.Info("migrating merge request comments from GitLab to GitHub", "owner", owner, "repo", repo, "pr_number", prNumber, "count", len(comments))

	for _, comment := range comments {
		if comment == nil || comment.System {
			continue
		}

		if err := prm.migrateComment(ctx, githubPath, pullRequest, comment, prComments); err != nil {
			prm.service.SendError(err)
		}
	}

	return nil
}

func (prm *PullRequestMigrator) fetchComments(project *gitlab.Project, mergeRequestIID int) ([]*gitlab.Note, error) {
	var comments []*gitlab.Note
	opts := &gitlab.ListMergeRequestNotesOptions{
		OrderBy: gitlab.String("created_at"),
		Sort:    gitlab.String("asc"),
	}

	prm.service.logger.Debug("retrieving GitLab merge request comments", "project", project.PathWithNamespace, "merge_request_id", mergeRequestIID)
	for {
		result, resp, err := prm.service.gitlabClient.Notes.ListMergeRequestNotes(project.ID, mergeRequestIID, opts)
		if err != nil {
			return nil, fmt.Errorf("listing merge request notes: %w", err)
		}

		comments = append(comments, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return comments, nil
}

func (prm *PullRequestMigrator) migrateComment(ctx context.Context, githubPath []string, pullRequest *github.PullRequest, comment *gitlab.Note, prComments []*github.IssueComment) error {
	githubCommentAuthorName := comment.Author.Name

	commentAuthor, err := prm.service.GetGitlabUser(comment.Author.Username)
	if err != nil {
		return fmt.Errorf("retrieving gitlab user: %w", err)
	}
	if commentAuthor.WebsiteURL != "" {
		githubCommentAuthorName = "@" + strings.TrimPrefix(strings.ToLower(commentAuthor.WebsiteURL), "https://github.com/")
	}

	commentBody := fmt.Sprintf(`> [!NOTE]
> This comment was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %s |
> | **Note ID** | %d |
> | **Date Originally Created** | %s |
>
> ---

## Original Comment

%s`, githubCommentAuthorName, comment.ID, comment.CreatedAt.Format(DateFormat), comment.Body)

	owner, repo := githubPath[0], githubPath[1]
	prNumber := pullRequest.GetNumber()

	foundExistingComment := false
	for _, prComment := range prComments {
		if prComment == nil {
			continue
		}

		if strings.Contains(prComment.GetBody(), fmt.Sprintf("**Note ID** | %d", comment.ID)) {
			foundExistingComment = true

			if prComment.GetBody() != commentBody {
				prm.service.logger.Debug("updating pull request comment", "owner", owner, "repo", repo, "pr_number", prNumber, "comment_id", prComment.GetID())
				prComment.Body = &commentBody
				if _, _, err = prm.service.githubClient.Issues.EditComment(ctx, owner, repo, prComment.GetID(), prComment); err != nil {
					return fmt.Errorf("updating pull request comments: %w", err)
				}
			} else {
				prm.service.logger.Trace("existing pull request comment is up-to-date", "owner", owner, "repo", repo, "pr_number", prNumber, "comment_id", prComment.GetID())
			}
			break
		}
	}

	if !foundExistingComment {
		prm.service.logger.Debug("creating pull request comment", "owner", owner, "repo", repo, "pr_number", prNumber)
		newComment := github.IssueComment{
			Body: &commentBody,
		}
		if _, _, err = prm.service.githubClient.Issues.CreateComment(ctx, owner, repo, prNumber, &newComment); err != nil {
			return fmt.Errorf("creating pull request comment: %w", err)
		}
	}

	return nil
}
