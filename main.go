package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/go-github/v66/github"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/xanzy/go-gitlab"
)

const (
	defaultGithubDomain = "github.com"
	defaultGitlabDomain = "gitlab.com"
)

var deleteExistingRepos, renameMasterToMain bool
var githubDomain, githubRepo, githubToken, githubUser, gitlabDomain, gitlabProject, gitlabToken, projectsCsvPath string

var client *http.Client
var logger hclog.Logger
var gh *github.Client
var gl *gitlab.Client

var gitlabUsersCache map[string]*gitlab.User
var githubUsersCache map[string]*github.User

type Projects = [][]string

func main() {
	var err error

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
			cancel()
		case <-ctx.Done():
		}
	}()

	logger = hclog.New(&hclog.LoggerOptions{
		Name:  "gitlab-migrator",
		Level: hclog.LevelFromString(os.Getenv("LOG_LEVEL")),
	})

	gitlabUsersCache = make(map[string]*gitlab.User)
	githubUsersCache = make(map[string]*github.User)

	githubToken = os.Getenv("GITHUB_TOKEN")
	if githubToken == "" {
		logger.Error("missing environment variable", "name", "GITHUB_TOKEN")
		os.Exit(1)
	}

	gitlabToken = os.Getenv("GITLAB_TOKEN")
	if gitlabToken == "" {
		logger.Error("missing environment variable", "name", "GITLAB_TOKEN")
		os.Exit(1)
	}

	flag.BoolVar(&deleteExistingRepos, "delete-existing-repos", false, "whether existing repositories should be deleted before migrating (defaults to: false)")
	flag.BoolVar(&renameMasterToMain, "rename-master-to-main", false, "rename master branch to main and update pull requests (defaults to: false)")

	flag.StringVar(&githubDomain, "github-domain", defaultGithubDomain, "specifies the GitHub domain to use (defaults to: github.com)")
	flag.StringVar(&githubRepo, "github-repo", "", "the GitHub repository to migrate to")
	flag.StringVar(&githubUser, "github-user", "", "specifies the GitHub user to use, who will author any migrated PRs (required)")
	flag.StringVar(&gitlabDomain, "gitlab-domain", defaultGitlabDomain, "specifies the GitLab domain to use (defaults to: gitlab.com)")
	flag.StringVar(&gitlabProject, "gitlab-project", "", "the GitLab project to migrate")
	flag.StringVar(&projectsCsvPath, "projects-csv", "", "specifies the path to a CSV file describing projects to migrate (incompatible with -gitlab-project and -github-project)")
	flag.Parse()

	if githubUser == "" {
		logger.Error("must specify GitHub user")
		os.Exit(1)
	}

	repoSpecifiedInline := githubRepo != "" && gitlabProject != ""
	if repoSpecifiedInline && projectsCsvPath != "" {
		logger.Error("cannot specify -projects-csv and either -github-repo or -gitlab-project at the same time")
		os.Exit(1)
	}
	if !repoSpecifiedInline && projectsCsvPath == "" {
		logger.Error("must specify either -projects-csv or both of -github-repo and -gitlab-project")
		os.Exit(1)
	}

	retryClient := retryablehttp.NewClient()
	retryClient.Logger = nil
	retryClient.RetryMax = 5

	client = retryClient.StandardClient()

	if githubDomain == defaultGithubDomain {
		gh = github.NewClient(client).WithAuthToken(githubToken)
	} else {
		githubUrl := fmt.Sprintf("https://%s", githubDomain)
		if gh, err = github.NewClient(client).WithAuthToken(githubToken).WithEnterpriseURLs(githubUrl, githubUrl); err != nil {
			logger.Error(err.Error())
			os.Exit(1)
		}
	}

	gitlabOpts := make([]gitlab.ClientOptionFunc, 0)
	if gitlabDomain != defaultGitlabDomain {
		gitlabUrl := fmt.Sprintf("https://%s", gitlabDomain)
		gitlabOpts = append(gitlabOpts, gitlab.WithBaseURL(gitlabUrl))
	}
	if gl, err = gitlab.NewClient(gitlabToken, gitlabOpts...); err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}

	projects := make([][]string, 0)
	if projectsCsvPath != "" {
		csvFile, err := os.Open(projectsCsvPath)
		if err != nil {
			logger.Error(err.Error())
			os.Exit(1)
		}

		if projects, err = csv.NewReader(csvFile).ReadAll(); err != nil {
			logger.Error(err.Error())
			os.Exit(1)
		}
	} else {
		projects = [][]string{{gitlabProject, githubRepo}}
	}

	if err = performMigration(ctx, projects); err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}
}

func pointer[T any](v T) *T {
	return &v
}

func performMigration(ctx context.Context, projects Projects) error {
	logger.Info(fmt.Sprintf("processing %d project(s)", len(projects)))

	for _, proj := range projects {
		gitlabPath := strings.Split(proj[0], "/")
		githubPath := strings.Split(proj[1], "/")

		logger.Info("searching for GitLab project", "name", gitlabPath[1], "group", gitlabPath[0])
		searchTerm := gitlabPath[1]
		projectResult, _, err := gl.Projects.ListProjects(&gitlab.ListProjectsOptions{Search: &searchTerm})
		if err != nil {
			return err
		}

		var project *gitlab.Project
		for _, item := range projectResult {
			if item == nil {
				continue
			}

			if item.PathWithNamespace == proj[0] {
				logger.Debug("found GitLab project", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", item.ID)
				project = item
			}
		}

		if project == nil {
			return fmt.Errorf("no matching GitLab project found: %s", proj[0])
		}

		cloneUrl, err := url.Parse(project.HTTPURLToRepo)
		if err != nil {
			return err
		}

		logger.Info("mirroring repository from GitLab to GitHub", "name", gitlabPath[1], "group", gitlabPath[0])

		user, err := getGithubUser(ctx, githubPath[0])
		if err != nil {
			return err
		}

		var org string
		if strings.EqualFold(*user.Type, "organization") {
			org = githubPath[0]
		} else if !strings.EqualFold(*user.Type, "user") || !strings.EqualFold(*user.Login, githubPath[0]) {
			return fmt.Errorf("configured owner is neither an organization nor the current user: %s", githubPath[0])
		}

		logger.Debug("checking for existing repository on GitHub", "owner", githubPath[0], "repo", githubPath[1])
		_, _, err = gh.Repositories.Get(ctx, githubPath[0], githubPath[1])

		deleted := false
		if err == nil && deleteExistingRepos {
			logger.Warn("existing repository was found on GitHub, proceeding to delete", "owner", githubPath[0], "repo", githubPath[1])
			if _, err = gh.Repositories.Delete(ctx, githubPath[0], githubPath[1]); err != nil {
				return err
			}

			deleted = true
		}

		if deleted || err != nil {
			var githubError *github.ErrorResponse
			if err != nil && (!errors.As(err, &githubError) || githubError == nil || githubError.Response == nil || githubError.Response.StatusCode != http.StatusNotFound) {
				return err
			}

			if deleted {
				logger.Warn("recreating GitHub repository", "owner", githubPath[0], "repo", githubPath[1])
			} else {
				logger.Debug("repository not found on GitHub, proceeding to create", "owner", githubPath[0], "repo", githubPath[1])
			}
			newRepo := github.Repository{
				Name:        pointer(githubPath[1]),
				Private:     pointer(true),
				HasIssues:   pointer(true),
				HasProjects: pointer(true),
				HasWiki:     pointer(true),
			}
			if _, _, err = gh.Repositories.Create(ctx, org, &newRepo); err != nil {
				return err
			}
		}

		logger.Debug("updating repository settings", "owner", githubPath[0], "repo", githubPath[1])
		updateRepo := github.Repository{
			Name:              pointer(githubPath[1]),
			Description:       &project.Description,
			AllowAutoMerge:    pointer(true),
			AllowMergeCommit:  pointer(true),
			AllowRebaseMerge:  pointer(true),
			AllowSquashMerge:  pointer(true),
			AllowUpdateBranch: pointer(true),
		}
		if org != "" {
			updateRepo.AllowForking = pointer(true)
		}
		if _, _, err = gh.Repositories.Edit(ctx, githubPath[0], githubPath[1], &updateRepo); err != nil {
			return err
		}

		cloneUrl.User = url.UserPassword("oauth2", gitlabToken)
		cloneUrlWithCredentials := cloneUrl.String()

		// In-memory filesystem for worktree operations
		fs := memfs.New()

		logger.Debug("cloning repository", "name", gitlabPath[1], "group", gitlabPath[0], "url", project.HTTPURLToRepo)
		repo, err := git.CloneContext(ctx, memory.NewStorage(), fs, &git.CloneOptions{
			URL:        cloneUrlWithCredentials,
			Auth:       nil,
			RemoteName: "gitlab",
			Mirror:     true,
		})
		if err != nil {
			return err
		}

		if renameMasterToMain {
			if masterBranch, err := repo.Reference(plumbing.NewBranchReferenceName("master"), false); err == nil {
				logger.Info("renaming master branch to main prior to push", "repo", proj[1], "sha", masterBranch.Hash())

				logger.Debug("creating main branch", "repo", proj[1], "sha", masterBranch.Hash())
				mainBranch := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), masterBranch.Hash())
				if err = repo.Storer.SetReference(mainBranch); err != nil {
					return err
				}

				logger.Debug("deleting master branch", "repo", proj[1], "sha", masterBranch.Hash())
				if err = repo.Storer.RemoveReference(masterBranch.Name()); err != nil {
					return err
				}
			}
		}

		githubUrl := fmt.Sprintf("https://%s/%s/%s", githubDomain, githubPath[0], githubPath[1])
		githubUrlWithCredentials := fmt.Sprintf("https://%s:%s@%s/%s/%s", githubUser, githubToken, githubDomain, githubPath[0], githubPath[1])

		logger.Debug("adding remote for GitHub repository", "name", gitlabPath[1], "group", gitlabPath[0], "url", githubUrl)
		if _, err = repo.CreateRemote(&config.RemoteConfig{
			Name:   "github",
			URLs:   []string{githubUrlWithCredentials},
			Mirror: true,
		}); err != nil {
			return err
		}

		logger.Debug("force-pushing to GitHub repository", "name", gitlabPath[1], "group", gitlabPath[0], "url", githubUrl)
		if err = repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			Force:      true,
			//Prune:      true, // causes error, attempts to delete main branch
		}); err != nil {
			uptodateError := errors.New("already up-to-date")
			if errors.As(err, &uptodateError) {
				logger.Debug("repository already up-to-date on GitHub", "name", gitlabPath[1], "group", gitlabPath[0], "url", githubUrl)
			} else {
				return err
			}
		}

		logger.Debug("retrieving GitLab merge requests", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID)
		mergeRequests, _, err := gl.MergeRequests.ListProjectMergeRequests(project.ID, nil)
		if err != nil {
			return err
		}

		logger.Info("migrating merge requests from GitLab to GitHub", "name", gitlabPath[1], "group", gitlabPath[0])
		for _, mergeRequest := range mergeRequests {
			if mergeRequest == nil {
				continue
			}

			var cleanUpBranch bool

			if renameMasterToMain && mergeRequest.TargetBranch == "master" {
				logger.Trace("changing target branch from master to main", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID)
				mergeRequest.TargetBranch = "main"
			}

			if !strings.EqualFold(mergeRequest.State, "opened") {
				logger.Trace("searching for existing branch for closed/merged merge request", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch)

				if _, err = repo.Reference(plumbing.ReferenceName(mergeRequest.SourceBranch), false); err != nil {

					logger.Trace("inspecting merge commit parents", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "sha", mergeRequest.MergeCommitSHA)
					mergeCommit, err := object.GetCommit(repo.Storer, plumbing.NewHash(mergeRequest.MergeCommitSHA))
					if err != nil {
						return err
					}

					parents := make([]*object.Commit, 0)
					if err = mergeCommit.Parents().ForEach(func(commit *object.Commit) error {
						parents = append(parents, commit)
						return nil
					}); err != nil {
						return err
					}

					if len(parents) != 2 {
						return fmt.Errorf("encountered merge commit without 2 parents")
					}

					var startHash, endHash plumbing.Hash
					if parents[0].Committer.When.Before(parents[1].Committer.When) {
						startHash = parents[0].Hash
						endHash = parents[1].Hash
					} else {
						startHash = parents[1].Hash
						endHash = parents[0].Hash
					}

					worktree, err := repo.Worktree()
					if err != nil {
						return err
					}

					mergeRequest.SourceBranch = fmt.Sprintf("migration-source/%s", mergeRequest.SourceBranch)
					mergeRequest.TargetBranch = fmt.Sprintf("migration-target/%s", mergeRequest.TargetBranch)

					logger.Trace("creating target branch for closed/merged merge request", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.TargetBranch, "sha", startHash)
					if err = worktree.Checkout(&git.CheckoutOptions{
						Create: true,
						Branch: plumbing.NewBranchReferenceName(mergeRequest.TargetBranch),
						Hash:   startHash,
					}); err != nil {
						return err
					}

					logger.Trace("creating source branch for closed/merged merge request", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.SourceBranch, "sha", endHash)
					branchName := plumbing.NewBranchReferenceName(mergeRequest.SourceBranch)
					if err = worktree.Checkout(&git.CheckoutOptions{
						Create: true,
						Branch: plumbing.NewBranchReferenceName(mergeRequest.SourceBranch),
						Hash:   endHash,
					}); err != nil {
						return err
					}

					//noticeFile, err := fs.Create("__notice.txt")
					//if err != nil {
					//	return err
					//}
					//
					//if _, err = noticeFile.Write([]byte("NOTICE: This file was committed to facilitate migrating the pull request from GitLab to GitHub, and is not present in the original branch and was not merged")); err != nil {
					//	return err
					//}
					//
					//if err = noticeFile.Close(); err != nil {
					//	return err
					//}
					//
					//if _, err = worktree.Add("__notice.txt"); err != nil {
					//	return err
					//}
					//
					//if _, err = worktree.Commit("MIGRATION-ONLY: Do not merge", &git.CommitOptions{
					//	Author: &object.Signature{
					//		Name:  "GitLab Migrator",
					//		Email: "notreal@gitlab-migrator.internal",
					//		When:  time.Now(),
					//	},
					//}); err != nil {
					//	return err
					//}

					logger.Debug("pushing branches for closed/merged merge request", "repo", proj[1], "source_branch", branchName)
					if err = repo.PushContext(ctx, &git.PushOptions{
						RemoteName: "github",
						RefSpecs: []config.RefSpec{
							config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.SourceBranch)),
							config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.TargetBranch)),
						},
						Force: true,
					}); err != nil {
						uptodateError := errors.New("already up-to-date")
						if errors.As(err, &uptodateError) {
							logger.Debug("branch already exists and is up-to-date on GitHub", "repo", proj[1], "source_branch", branchName)
						} else {
							return err
						}
					}

					cleanUpBranch = true
				}
			}

			var pullRequest *github.PullRequest

			logger.Debug("searching for any existing pull request", "repo", proj[1], "source", mergeRequest.SourceBranch, "target", mergeRequest.TargetBranch)
			query := fmt.Sprintf("repo:%s/%s is:pr head:%s", githubPath[0], githubPath[1], mergeRequest.SourceBranch)
			searchResult, _, err := gh.Search.Issues(ctx, query, nil)
			if err != nil {
				return err
			}

			// Look for an existing GitHub pull request having the same source and head (target) branch
			for _, issue := range searchResult.Issues {
				if issue == nil {
					continue
				}

				if issue.IsPullRequest() {
					// Extract the PR number from the URL
					prUrl, err := url.Parse(*issue.PullRequestLinks.URL)
					if err != nil {
						return err
					}

					if m := regexp.MustCompile(".+/([0-9]+)$").FindStringSubmatch(prUrl.Path); len(m) == 2 {
						prNumber, _ := strconv.Atoi(m[1])
						pr, _, err := gh.PullRequests.Get(ctx, githubPath[0], githubPath[1], prNumber)
						if err != nil {
							return err
						}

						if pr.Head != nil && pr.Head.Ref != nil && *pr.Head.Ref == mergeRequest.SourceBranch && pr.Base != nil && pr.Base.Ref != nil && *pr.Base.Ref == mergeRequest.TargetBranch {
							logger.Debug("found existing pull request", "repo", proj[1], "source", mergeRequest.SourceBranch, "target", mergeRequest.TargetBranch)
							pullRequest = pr
						}
					}
				}
			}

			githubAuthorName := mergeRequest.Author.Name

			author, err := getGitlabUser(mergeRequest.Author.Username)
			if err != nil {
				return err
			}
			if author.WebsiteURL != "" {
				githubAuthorName = "@" + strings.TrimPrefix(strings.ToLower(author.WebsiteURL), "https://github.com/")
			}

			originalState := ""
			if !strings.EqualFold(mergeRequest.State, "opened") {
				originalState = fmt.Sprintf("> This merge request was originally **%s** on GitLab", mergeRequest.State)
			}

			logger.Debug("determining merge request approvers", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID)
			approvers := make([]string, 0)
			awards, _, err := gl.AwardEmoji.ListMergeRequestAwardEmoji(project.ID, mergeRequest.IID, nil)
			if err != nil {
				return err
			}
			for _, award := range awards {
				if award.Name == "thumbsup" {
					approver := award.User.Name

					approverUser, err := getGitlabUser(award.User.Username)
					if err != nil {
						return err
					}
					if approverUser.WebsiteURL != "" {
						approver = "@" + strings.TrimPrefix(strings.ToLower(approverUser.WebsiteURL), "https://github.com/")
					}

					approvers = append(approvers, approver)
				}
			}

			description := mergeRequest.Description
			if strings.TrimSpace(description) == "" {
				description = "_No description_"
			}

			approval := strings.Join(approvers, ", ")
			if approval == "" {
				approval = "_No approvers_"
			}

			body := fmt.Sprintf(`> [!NOTE]
> This pull request was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **GitLab Project** | %[4]s/%[5]s |
> | **GitLab MR Number** | %[2]d |
> | **Date Originally Opened** | %[6]s |
> | **Approved on GitLab by** | %[8]s |
> |      |      |
>
%[7]s

## Original Description

%[3]s`, githubAuthorName, mergeRequest.IID, description, gitlabPath[0], gitlabPath[1], mergeRequest.CreatedAt.Format("Mon, 2 Jan 2006"), originalState, approval)

			if pullRequest == nil {
				logger.Debug("creating pull request", "repo", proj[1], "source", mergeRequest.SourceBranch, "target", mergeRequest.TargetBranch)
				newPullRequest := github.NewPullRequest{
					Title:               &mergeRequest.Title,
					Head:                &mergeRequest.SourceBranch,
					Base:                &mergeRequest.TargetBranch,
					Body:                &body,
					MaintainerCanModify: pointer(true),
					Draft:               &mergeRequest.Draft,
				}
				if pullRequest, _, err = gh.PullRequests.Create(ctx, githubPath[0], githubPath[1], &newPullRequest); err != nil {
					return err
				}

				if mergeRequest.State == "closed" || mergeRequest.State == "merged" {
					pullRequest.State = pointer("closed")

					if pullRequest, _, err = gh.PullRequests.Edit(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
						return err
					}
				}

			} else {
				logger.Debug("updating pull request", "repo", proj[1], "source", mergeRequest.SourceBranch, "target", mergeRequest.TargetBranch)

				var newState *string
				switch mergeRequest.State {
				case "opened":
					newState = pointer("open")
				case "closed", "merged":
					newState = pointer("closed")
				}

				pullRequest.Title = &mergeRequest.Title
				pullRequest.Body = &body
				pullRequest.Draft = &mergeRequest.Draft
				pullRequest.State = newState
				if pullRequest, _, err = gh.PullRequests.Edit(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
					return err
				}
			}

			if cleanUpBranch {
				// TODO clean up temporary branch
			}

			logger.Debug("retrieving GitLab merge request comments", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID)
			comments, _, err := gl.Notes.ListMergeRequestNotes(project.ID, mergeRequest.IID, &gitlab.ListMergeRequestNotesOptions{OrderBy: pointer("created_at"), Sort: pointer("asc")})
			if err != nil {
				return err
			}

			logger.Debug("retrieving GitHub pull request comments", "repo", proj[1], "pull_request_id", pullRequest.GetNumber())
			prComments, _, err := gh.Issues.ListComments(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), &github.IssueListCommentsOptions{Sort: pointer("created"), Direction: pointer("asc")})
			if err != nil {
				return err
			}

			for _, comment := range comments {
				if comment == nil || comment.System {
					continue
				}

				githubCommentAuthorName := comment.Author.Name

				commentAuthor, err := getGitlabUser(comment.Author.Username)
				if err != nil {
					return err
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

%[4]s`, githubCommentAuthorName, comment.ID, comment.CreatedAt.Format("Mon, 2 Jan 2006"), comment.Body)

				updated := false
				for _, prComment := range prComments {
					if prComment == nil {
						continue
					}

					if strings.Contains(prComment.GetBody(), fmt.Sprintf("**Note ID** | %d", comment.ID)) {
						logger.Debug("updating pull request comment", "repo", proj[1], "source", mergeRequest.SourceBranch, "target", mergeRequest.TargetBranch, "pr_number", pullRequest.GetNumber())
						prComment.Body = &commentBody
						if _, _, err = gh.Issues.EditComment(ctx, githubPath[0], githubPath[1], prComment.GetID(), prComment); err != nil {
							return err
						}
						updated = true
					}
				}

				if !updated {
					logger.Debug("creating pull request comment", "repo", proj[1], "source", mergeRequest.SourceBranch, "target", mergeRequest.TargetBranch, "pr_number", pullRequest.GetNumber())
					newComment := github.IssueComment{
						Body: &commentBody,
					}
					if _, _, err = gh.Issues.CreateComment(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), &newComment); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

func getGithubUser(ctx context.Context, username string) (*github.User, error) {
	var user *github.User
	var err error
	var ok bool
	if user, ok = githubUsersCache[username]; !ok {
		logger.Debug("retrieving user details", "username", username)
		if user, _, err = gh.Users.Get(ctx, username); err != nil {
			return nil, err
		}

		logger.Trace("caching GitHub user", "username", username)
		githubUsersCache[username] = user
	}

	if user == nil {
		return nil, fmt.Errorf("nil user was returned: %s", username)
	}
	if user.Type == nil {
		return nil, fmt.Errorf("unable to determine whether owner is a user or organisatition: %s", username)
	}

	return user, nil
}

func getGitlabUser(username string) (*gitlab.User, error) {
	user, ok := gitlabUsersCache[username]
	if !ok {
		logger.Debug("retrieving user details", "username", username)
		users, _, err := gl.Users.ListUsers(&gitlab.ListUsersOptions{Username: &username})
		if err != nil {
			return nil, err
		}

		for _, user = range users {
			if user != nil && user.Username == username {
				logger.Trace("caching GitLab user", "username", username)
				gitlabUsersCache[username] = user

				return user, nil
			}
		}

		return nil, fmt.Errorf("GitLab user not found: %s", username)
	}

	return user, nil
}
