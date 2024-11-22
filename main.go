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

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
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

var deleteExistingRepos bool
var githubDomain, githubRepo, githubToken, githubUser, gitlabDomain, gitlabProject, gitlabToken, projectsCsvPath string

var client *http.Client
var logger hclog.Logger
var gh *github.Client
var gl *gitlab.Client

var usersCache map[string]*github.User

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

	usersCache = make(map[string]*github.User)

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

	for _, project := range projects {
		gitlabPath := strings.Split(project[0], "/")
		githubPath := strings.Split(project[1], "/")

		logger.Info("searching for GitLab project", "name", gitlabPath[1], "group", gitlabPath[0])
		searchTerm := gitlabPath[1]
		projectResult, _, err := gl.Projects.ListProjects(&gitlab.ListProjectsOptions{Search: &searchTerm})
		if err != nil {
			return err
		}

		var match *gitlab.Project
		for _, proj := range projectResult {
			if proj == nil {
				continue
			}

			if proj.PathWithNamespace == project[0] {
				logger.Debug("found GitLab project", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", proj.ID)
				match = proj
			}
		}

		if match == nil {
			return fmt.Errorf("no matching GitLab project found: %s", project[0])
		}

		cloneUrl, err := url.Parse(match.HTTPURLToRepo)
		if err != nil {
			return err
		}

		logger.Info("mirroring repository from GitLab to GitHub", "name", gitlabPath[1], "group", gitlabPath[0])

		var user *github.User
		var ok bool
		if user, ok = usersCache[githubPath[0]]; !ok {
			logger.Debug("retrieving user details", "name", githubPath[0])
			if user, _, err = gh.Users.Get(ctx, githubPath[0]); err != nil {
				return err
			}
		}

		if user == nil {
			return fmt.Errorf("nil user was returned: %s", githubPath[0])
		}
		if user.Type == nil {
			return fmt.Errorf("unable to determine whether owner is a user or organisatition: %s", githubPath[0])
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
			Description:       &match.Description,
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

		logger.Debug("cloning repository", "name", gitlabPath[1], "group", gitlabPath[0], "url", match.HTTPURLToRepo)
		repo, err := git.CloneContext(ctx, memory.NewStorage(), nil, &git.CloneOptions{
			URL:        cloneUrlWithCredentials,
			Auth:       nil,
			RemoteName: "gitlab",
			Mirror:     true,
		})
		if err != nil {
			return err
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

		logger.Debug("retrieving GitLab merge requests", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", match.ID)
		mergeRequests, _, err := gl.MergeRequests.ListProjectMergeRequests(match.ID, nil)
		if err != nil {
			return err
		}

		for _, mergeRequest := range mergeRequests {
			if mergeRequest == nil {
				continue
			}

			if !strings.EqualFold(mergeRequest.State, "opened") {
				logger.Trace("TODO: check for branch of closed/merged MR and recreate it temporarily as needed")
			}

			var pullRequest *github.PullRequest

			logger.Debug("searching for any existing pull request", "repo", project[1], "source", mergeRequest.SourceBranch, "target", mergeRequest.TargetBranch)
			query := fmt.Sprintf("repo:%s/%s is:pr head:%s", githubPath[0], githubPath[1], mergeRequest.SourceBranch)
			searchResult, _, err := gh.Search.Issues(ctx, query, nil)
			if err != nil {
				return err
			}

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
							logger.Debug("found existing pull request", "repo", project[1], "source", mergeRequest.SourceBranch, "target", mergeRequest.TargetBranch)
							pullRequest = pr
						}
					}
				}
			}

			githubAuthorName := mergeRequest.Author.Name

			author, _, err := gl.Users.GetUser(mergeRequest.Author.ID, gitlab.GetUsersOptions{})
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

			// TODO: work out the original approvers

			body := fmt.Sprintf(`> [!NOTE]
> This pull request was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **GitLab Project** | %[4]s/%[5]s |
> | **GitLab MR Number** | %[2]d |
> | **Date Originally Opened** | %[6]s |
> |      |      |
>
%[7]s

## Original Description

%[3]s`, githubAuthorName, mergeRequest.IID, mergeRequest.Description, gitlabPath[0], gitlabPath[1], mergeRequest.CreatedAt.Format("Mon, 2 Jan 2006"), originalState)

			if pullRequest == nil {
				logger.Debug("creating pull request", "repo", project[1], "source", mergeRequest.SourceBranch, "target", mergeRequest.TargetBranch)
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

			} else {
				logger.Debug("updating pull request", "repo", project[1], "source", mergeRequest.SourceBranch, "target", mergeRequest.TargetBranch)

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
		}

		//pullRequests, _, err := gh.PullRequests.List(ctx, githubPath[0], githubPath[1], nil)
		//if err != nil {
		//	return err
		//}
		//
		//for _, pr := range pullRequests {
		//	if pr == nil {
		//		continue
		//	}
		//	fmt.Printf("%#v", *pr)
		//}
	}

	return nil
}
