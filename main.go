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
var githubDomain, githubUser, githubToken, gitlabDomain, gitlabToken, projectsCsvPath string

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
	flag.StringVar(&githubUser, "github-user", "", "specifies the GitHub user to use (required)")
	flag.StringVar(&gitlabDomain, "gitlab-domain", defaultGitlabDomain, "specifies the GitLab domain to use (defaults to: gitlab.com)")
	flag.StringVar(&projectsCsvPath, "projects-csv", "projects.csv", "specifies the path to a CSV file describing projects to migrate (defaults to: projects.csv)")
	flag.Parse()

	if githubUser == "" {
		logger.Error("must specify GitHub user")
		os.Exit(1)
	}

	retryClient := retryablehttp.NewClient()
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

	csvFile, err := os.Open(projectsCsvPath)
	if err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}

	projects, err := csv.NewReader(csvFile).ReadAll()
	if err != nil {
		logger.Error(err.Error())
		os.Exit(1)
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

		//logger.Debug("retrieving GitLab merge requests", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", match.ID)
		//mergeRequests, _, err := gl.MergeRequests.ListProjectMergeRequests(match.ID, nil)
		//if err != nil {
		//	return err
		//}
		//
		//for _, mr := range mergeRequests {
		//	if mr == nil {
		//		continue
		//	}
		//
		//	fmt.Printf("%#v", *mr)
		//}

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
