package main

import (
	"context"
	"errors"
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
)

// PullRequestMigrator handles the migration of pull requests and comments
type PullRequestMigrator struct {
	service *MigrationService
}

func NewPullRequestMigrator(service *MigrationService) *PullRequestMigrator {
	return &PullRequestMigrator{service: service}
}

// MigratePullRequests migrates all merge requests from GitLab to GitHub pull requests
func (prm *PullRequestMigrator) MigratePullRequests(ctx context.Context, githubPath, gitlabPath []string, project *gitlab.Project, repo *git.Repository) {
	mergeRequests, err := prm.fetchMergeRequests(project.ID, gitlabPath)
	if err != nil {
		prm.service.SendError(err)
		return
	}

	var successCount, failureCount int
	totalCount := len(mergeRequests)
	prm.service.logger.Info("migrating merge requests from GitLab to GitHub", "name", gitlabPath[1], "group", gitlabPath[0], "count", totalCount)

	for _, mergeRequest := range mergeRequests {
		if mergeRequest == nil {
			continue
		}

		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			prm.service.SendError(fmt.Errorf("preparing to list pull requests: %v", err))
			break
		}

		if err := prm.migrateSingleMergeRequest(ctx, githubPath, gitlabPath, project, repo, mergeRequest); err != nil {
			prm.service.SendError(err)
			failureCount++
		} else {
			successCount++
		}
	}

	skippedCount := totalCount - successCount - failureCount
	prm.service.logger.Info("migrated merge requests from GitLab to GitHub", "name", gitlabPath[1], "group", gitlabPath[0], "successful", successCount, "failed", failureCount, "skipped", skippedCount)
}

func (prm *PullRequestMigrator) fetchMergeRequests(projectID int, gitlabPath []string) ([]*gitlab.MergeRequest, error) {
	var mergeRequests []*gitlab.MergeRequest

	opts := &gitlab.ListProjectMergeRequestsOptions{
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}

	prm.service.logger.Debug("retrieving GitLab merge requests", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", projectID)
	for {
		result, resp, err := prm.service.gitlabClient.MergeRequests.ListProjectMergeRequests(projectID, opts)
		if err != nil {
			return nil, fmt.Errorf("retrieving gitlab merge requests: %v", err)
		}

		mergeRequests = append(mergeRequests, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return mergeRequests, nil
}

func (prm *PullRequestMigrator) migrateSingleMergeRequest(ctx context.Context, githubPath, gitlabPath []string, project *gitlab.Project, repo *git.Repository, mergeRequest *gitlab.MergeRequest) error {
	var cleanUpBranch bool
	var pullRequest *github.PullRequest

	prm.service.logger.Debug("searching for any existing pull request", "owner", githubPath[0], "repo", githubPath[1], "merge_request_id", mergeRequest.IID)

	// Look for existing pull request
	existingPR, err := prm.findExistingPullRequest(ctx, githubPath, mergeRequest)
	if err != nil {
		return fmt.Errorf("searching for existing pull request: %v", err)
	}
	pullRequest = existingPR

	// Handle closed/merged merge requests that need temporary branches
	if pullRequest == nil && !strings.EqualFold(mergeRequest.State, "opened") {
		tempBranchCreated, err := prm.createTemporaryBranches(ctx, repo, gitlabPath, project, mergeRequest)
		if err != nil {
			return err
		}
		cleanUpBranch = tempBranchCreated
	}

	// Rename master to main if needed
	if prm.service.config.RenameMasterToMain && mergeRequest.TargetBranch == MasterBranchName {
		prm.service.logger.Trace("changing target branch from master to main", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID)
		mergeRequest.TargetBranch = MainBranchName
	}

	// Create or update pull request
	pullRequest, err = prm.createOrUpdatePullRequest(ctx, githubPath, gitlabPath, mergeRequest, pullRequest)
	if err != nil {
		return err
	}

	// Clean up temporary branches
	if cleanUpBranch {
		if err := prm.cleanupTemporaryBranches(ctx, repo, githubPath, pullRequest, mergeRequest); err != nil {
			return err
		}
	}

	// Migrate comments
	return prm.migrateComments(ctx, githubPath, gitlabPath, project, pullRequest, mergeRequest)
}

func (prm *PullRequestMigrator) findExistingPullRequest(ctx context.Context, githubPath []string, mergeRequest *gitlab.MergeRequest) (*github.PullRequest, error) {
	query := fmt.Sprintf("repo:%s/%s is:pr", githubPath[0], githubPath[1])
	searchResult, err := prm.service.GetGithubSearchResults(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("listing pull requests: %v", err)
	}

	// Look for an existing GitHub pull request
	for _, issue := range searchResult.Issues {
		if issue == nil || !issue.IsPullRequest() {
			continue
		}

		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("preparing to retrieve pull request: %v", err)
		}

		// Extract the PR number from the URL
		prUrl, err := url.Parse(*issue.PullRequestLinks.URL)
		if err != nil {
			return nil, fmt.Errorf("parsing pull request url: %v", err)
		}

		if m := regexp.MustCompile(".+/([0-9]+)$").FindStringSubmatch(prUrl.Path); len(m) == 2 {
			prNumber, _ := strconv.Atoi(m[1])
			pr, err := prm.service.GetGithubPullRequest(ctx, githubPath[0], githubPath[1], prNumber)
			if err != nil {
				return nil, fmt.Errorf("retrieving pull request: %v", err)
			}

			if strings.Contains(pr.GetBody(), fmt.Sprintf("**GitLab MR Number** | %d", mergeRequest.IID)) ||
				strings.Contains(pr.GetBody(), fmt.Sprintf("**GitLab MR Number** | [%d]", mergeRequest.IID)) {
				prm.service.logger.Debug("found existing pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pr.GetNumber())
				return pr, nil
			}
		}
	}

	return nil, nil
}

func (prm *PullRequestMigrator) createTemporaryBranches(ctx context.Context, repo *git.Repository, gitlabPath []string, project *gitlab.Project, mergeRequest *gitlab.MergeRequest) (bool, error) {
	prm.service.logger.Trace("searching for existing branch for closed/merged merge request", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch)

	// Only create temporary branches if the source branch has been deleted
	if _, err := repo.Reference(plumbing.ReferenceName(mergeRequest.SourceBranch), false); err != nil {
		// Create a worktree
		worktree, err := repo.Worktree()
		if err != nil {
			return false, fmt.Errorf("creating worktree: %v", err)
		}

		// Generate temporary branch names
		mergeRequest.SourceBranch = fmt.Sprintf(MigrationSourceBranchPrefix, mergeRequest.IID, mergeRequest.SourceBranch)
		mergeRequest.TargetBranch = fmt.Sprintf(MigrationTargetBranchPrefix, mergeRequest.IID, mergeRequest.TargetBranch)

		prm.service.logger.Trace("retrieving commits for merge request", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID)
		mergeRequestCommits, _, err := prm.service.gitlabClient.MergeRequests.GetMergeRequestCommits(project.ID, mergeRequest.IID, &gitlab.GetMergeRequestCommitsOptions{OrderBy: "created_at", Sort: "asc"})
		if err != nil {
			return false, fmt.Errorf("retrieving merge request commits: %v", err)
		}

		// Some merge requests have no commits, disregard these
		if len(mergeRequestCommits) == 0 {
			return false, nil
		}

		// API is buggy, ordering is not respected, so we'll reorder by commit datestamp
		sort.Slice(mergeRequestCommits, func(i, j int) bool {
			return mergeRequestCommits[i].CommittedDate.Before(*mergeRequestCommits[j].CommittedDate)
		})

		if mergeRequestCommits[0] == nil {
			return false, fmt.Errorf("start commit for merge request %d is nil", mergeRequest.IID)
		}
		if mergeRequestCommits[len(mergeRequestCommits)-1] == nil {
			return false, fmt.Errorf("end commit for merge request %d is nil", mergeRequest.IID)
		}

		prm.service.logger.Trace("inspecting start commit", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "sha", mergeRequestCommits[0].ShortID)
		startCommit, err := object.GetCommit(repo.Storer, plumbing.NewHash(mergeRequestCommits[0].ID))
		if err != nil {
			return false, fmt.Errorf("loading start commit: %v", err)
		}

		// Starting with a merge commit is _probably_ wrong
		if startCommit.NumParents() > 1 {
			return false, fmt.Errorf("start commit %s for merge request %d has %d parents", mergeRequestCommits[0].ShortID, mergeRequest.IID, startCommit.NumParents())
		}

		if startCommit.NumParents() == 0 {
			// Orphaned commit, skip these for now
			return false, nil
		}

		// Branch out from parent commit
		prm.service.logger.Trace("inspecting start commit parent", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "sha", mergeRequestCommits[0].ShortID)
		startCommitParent, err := startCommit.Parent(0)
		if err != nil {
			return false, fmt.Errorf("loading parent commit: %s", err)
		}

		prm.service.logger.Trace("creating target branch for merged/closed merge request", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.TargetBranch, "sha", startCommitParent.Hash)
		if err = worktree.Checkout(&git.CheckoutOptions{
			Create: true,
			Force:  true,
			Branch: plumbing.NewBranchReferenceName(mergeRequest.TargetBranch),
			Hash:   startCommitParent.Hash,
		}); err != nil {
			return false, fmt.Errorf("checking out temporary target branch: %v", err)
		}

		endHash := plumbing.NewHash(mergeRequestCommits[len(mergeRequestCommits)-1].ID)
		prm.service.logger.Trace("creating source branch for merged/closed merge request", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.SourceBranch, "sha", endHash)
		if err = worktree.Checkout(&git.CheckoutOptions{
			Create: true,
			Force:  true,
			Branch: plumbing.NewBranchReferenceName(mergeRequest.SourceBranch),
			Hash:   endHash,
		}); err != nil {
			return false, fmt.Errorf("checking out temporary source branch: %v", err)
		}

		prm.service.logger.Debug("pushing branches for merged/closed merge request", "owner", githubPath[0], "repo", githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
		if err = repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			RefSpecs: []config.RefSpec{
				config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.SourceBranch)),
				config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.TargetBranch)),
			},
			Force: true,
		}); err != nil {
			upToDateError := errors.New("already up-to-date")
			if errors.As(err, &upToDateError) {
				prm.service.logger.Trace("branch already exists and is up-to-date on GitHub", "owner", githubPath[0], "repo", githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
			} else {
				return false, fmt.Errorf("pushing temporary branches to github: %v", err)
			}
		}

		return true, nil
	}

	return false, nil
}

func (prm *PullRequestMigrator) createOrUpdatePullRequest(ctx context.Context, githubPath, gitlabPath []string, mergeRequest *gitlab.MergeRequest, existingPR *github.PullRequest) (*github.PullRequest, error) {
	body, err := prm.buildPullRequestBody(mergeRequest, gitlabPath)
	if err != nil {
		return nil, err
	}

	if existingPR == nil {
		return prm.createPullRequest(ctx, githubPath, mergeRequest, body)
	}

	return prm.updatePullRequest(ctx, githubPath, mergeRequest, existingPR, body)
}

func (prm *PullRequestMigrator) buildPullRequestBody(mergeRequest *gitlab.MergeRequest, gitlabPath []string) (string, error) {
	githubAuthorName := mergeRequest.Author.Name

	author, err := prm.service.GetGitlabUser(mergeRequest.Author.Username)
	if err != nil {
		return "", fmt.Errorf("retrieving gitlab user: %v", err)
	}
	if author.WebsiteURL != "" {
		githubAuthorName = "@" + strings.TrimPrefix(strings.ToLower(author.WebsiteURL), "https://github.com/")
	}

	originalState := ""
	if !strings.EqualFold(mergeRequest.State, "opened") {
		originalState = fmt.Sprintf("> This merge request was originally **%s** on GitLab", mergeRequest.State)
	}

	// Get approvers
	approvers, err := prm.getApprovers(mergeRequest.ProjectID, mergeRequest.IID, gitlabPath)
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

	protocol := getProtocolFromURL(fmt.Sprintf("%s://%s", getProtocolFromURL(prm.service.config.GitlabDomain), prm.service.config.GitlabDomain))
	body := fmt.Sprintf(`> [!NOTE]
> This pull request was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **GitLab Project** | [%[4]s/%[5]s](%[10]s/%[4]s/%[5]s) |
> | **GitLab Merge Request** | [%[11]s](%[10]s/%[4]s/%[5]s/merge_requests/%[2]d) |
> | **Date Originally Created** | %[6]s |%[7]s
> | **Original Approvers** | %[8]s |
>
%[9]s

## Original Description

%[3]s`, githubAuthorName, mergeRequest.IID, description, gitlabPath[0], gitlabPath[1], mergeRequest.CreatedAt.Format(DateFormat), closeDate, approval, originalState, buildURL(prm.service.config.GitlabDomain, "", protocol), mergeRequestTitle)

	return body, nil
}

func (prm *PullRequestMigrator) getApprovers(projectID, mergeRequestIID int, gitlabPath []string) ([]string, error) {
	prm.service.logger.Debug("determining merge request approvers", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", projectID, "merge_request_id", mergeRequestIID)
	
	approvers := make([]string, 0)
	awards, _, err := prm.service.gitlabClient.AwardEmoji.ListMergeRequestAwardEmoji(projectID, mergeRequestIID, &gitlab.ListAwardEmojiOptions{PerPage: DefaultPerPage})
	if err != nil {
		return approvers, fmt.Errorf("listing merge request awards: %v", err)
	}

	for _, award := range awards {
		if award.Name == "thumbsup" {
			approver := award.User.Name

			approverUser, err := prm.service.GetGitlabUser(award.User.Username)
			if err != nil {
				prm.service.SendError(fmt.Errorf("retrieving gitlab user: %v", err))
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
	prm.service.logger.Info("creating pull request", "owner", githubPath[0], "repo", githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
	
	newPullRequest := github.NewPullRequest{
		Title:               &mergeRequest.Title,
		Head:                &mergeRequest.SourceBranch,
		Base:                &mergeRequest.TargetBranch,
		Body:                &body,
		MaintainerCanModify: pointer(true),
		Draft:               &mergeRequest.Draft,
	}
	
	pullRequest, _, err := prm.service.githubClient.PullRequests.Create(ctx, githubPath[0], githubPath[1], &newPullRequest)
	if err != nil {
		return nil, fmt.Errorf("creating pull request: %v", err)
	}

	if mergeRequest.State == "closed" || mergeRequest.State == "merged" {
		prm.service.logger.Debug("closing pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())

		pullRequest.State = pointer("closed")
		if pullRequest, _, err = prm.service.githubClient.PullRequests.Edit(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
			return nil, fmt.Errorf("updating pull request: %v", err)
		}
	}

	return pullRequest, nil
}

func (prm *PullRequestMigrator) updatePullRequest(ctx context.Context, githubPath []string, mergeRequest *gitlab.MergeRequest, pullRequest *github.PullRequest, body string) (*github.PullRequest, error) {
	var newState *string
	switch mergeRequest.State {
	case "opened":
		newState = pointer("open")
	case "closed", "merged":
		newState = pointer("closed")
	}

	if (newState != nil && (pullRequest.State == nil || *pullRequest.State != *newState)) ||
		(pullRequest.Title == nil || *pullRequest.Title != mergeRequest.Title) ||
		(pullRequest.Body == nil || *pullRequest.Body != body) ||
		(pullRequest.Draft == nil || *pullRequest.Draft != mergeRequest.Draft) {
		prm.service.logger.Info("updating pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())

		pullRequest.Title = &mergeRequest.Title
		pullRequest.Body = &body
		pullRequest.Draft = &mergeRequest.Draft
		pullRequest.State = newState
		
		var err error
		if pullRequest, _, err = prm.service.githubClient.PullRequests.Edit(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
			return nil, fmt.Errorf("updating pull request: %v", err)
		}
	} else {
		prm.service.logger.Trace("existing pull request is up-to-date", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())
	}

	return pullRequest, nil
}

func (prm *PullRequestMigrator) cleanupTemporaryBranches(ctx context.Context, repo *git.Repository, githubPath []string, pullRequest *github.PullRequest, mergeRequest *gitlab.MergeRequest) error {
	prm.service.logger.Debug("deleting temporary branches for closed pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
	
	if err := repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.SourceBranch)),
			config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.TargetBranch)),
		},
		Force: true,
	}); err != nil {
		upToDateError := errors.New("already up-to-date")
		if errors.As(err, &upToDateError) {
			prm.service.logger.Trace("branches already deleted on GitHub", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
		} else {
			return fmt.Errorf("pushing branch deletions to github: %v", err)
		}
	}

	return nil
}

func (prm *PullRequestMigrator) migrateComments(ctx context.Context, githubPath, gitlabPath []string, project *gitlab.Project, pullRequest *github.PullRequest, mergeRequest *gitlab.MergeRequest) error {
	comments, err := prm.fetchComments(project.ID, mergeRequest.IID, gitlabPath)
	if err != nil {
		return err
	}

	prm.service.logger.Debug("retrieving GitHub pull request comments", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())
	prComments, _, err := prm.service.githubClient.Issues.ListComments(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), &github.IssueListCommentsOptions{Sort: pointer("created"), Direction: pointer("asc")})
	if err != nil {
		return fmt.Errorf("listing pull request comments: %v", err)
	}

	prm.service.logger.Info("migrating merge request comments from GitLab to GitHub", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "count", len(comments))

	for _, comment := range comments {
		if comment == nil || comment.System {
			continue
		}

		if err := prm.migrateComment(ctx, githubPath, pullRequest, comment, prComments); err != nil {
			return err
		}
	}

	return nil
}

func (prm *PullRequestMigrator) fetchComments(projectID, mergeRequestIID int, gitlabPath []string) ([]*gitlab.Note, error) {
	var comments []*gitlab.Note
	opts := &gitlab.ListMergeRequestNotesOptions{
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}

	prm.service.logger.Debug("retrieving GitLab merge request comments", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", projectID, "merge_request_id", mergeRequestIID)
	for {
		result, resp, err := prm.service.gitlabClient.Notes.ListMergeRequestNotes(projectID, mergeRequestIID, opts)
		if err != nil {
			return nil, fmt.Errorf("listing merge request notes: %v", err)
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
		return fmt.Errorf("retrieving gitlab user: %v", err)
	}
	if commentAuthor.WebsiteURL != "" {
		githubCommentAuthorName = "@" + strings.TrimPrefix(strings.ToLower(commentAuthor.WebsiteURL), "https://github.com/")
	}

	commentBody := fmt.Sprintf(`> [!NOTE]
> This comment was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **Note ID** | %[2]d |
> | **Date Originally Created** | %[3]s |
> |      |      |
>

## Original Comment

%[4]s`, githubCommentAuthorName, comment.ID, comment.CreatedAt.Format(DateFormat), comment.Body)

	foundExistingComment := false
	for _, prComment := range prComments {
		if prComment == nil {
			continue
		}

		if strings.Contains(prComment.GetBody(), fmt.Sprintf("**Note ID** | %d", comment.ID)) {
			foundExistingComment = true

			if prComment.Body == nil || *prComment.Body != commentBody {
				prm.service.logger.Debug("updating pull request comment", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
				prComment.Body = &commentBody
				if _, _, err = prm.service.githubClient.Issues.EditComment(ctx, githubPath[0], githubPath[1], prComment.GetID(), prComment); err != nil {
					return fmt.Errorf("updating pull request comments: %v", err)
				}
			}
		} else {
			prm.service.logger.Trace("existing pull request comment is up-to-date", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
		}
	}

	if !foundExistingComment {
		prm.service.logger.Debug("creating pull request comment", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())
		newComment := github.IssueComment{
			Body: &commentBody,
		}
		if _, _, err = prm.service.githubClient.Issues.CreateComment(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), &newComment); err != nil {
			return fmt.Errorf("creating pull request comment: %v", err)
		}
	}

	return nil
}