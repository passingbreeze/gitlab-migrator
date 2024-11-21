package main

import (
	"context"
	"encoding/csv"
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

var githubDomain, githubUser, githubToken, gitlabDomain, gitlabToken, projectsCsvPath string

var client *http.Client
var logger hclog.Logger
var gh *github.Client
var gl *gitlab.Client

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

	githubToken = os.Getenv("GITHUB_TOKEN")
	gitlabToken = os.Getenv("GITLAB_TOKEN")

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

func performMigration(ctx context.Context, projects Projects) error {
	for _, project := range projects {
		gitlabPath := strings.Split(project[0], "/")
		githubPath := strings.Split(project[1], "/")

		logger.Debug("searching for GitLab project", "name", gitlabPath[1], "group", gitlabPath[0])
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
				logger.Trace("found GitLab project", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", proj.ID)
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

		logger.Debug("mirroring repository from GitLab to GitHub", "name", gitlabPath[1], "group", gitlabPath[0])

		cloneUrl.User = url.UserPassword("oauth2", gitlabToken)
		cloneUrlWithCredentials := cloneUrl.String()

		logger.Trace("cloning repository", "name", gitlabPath[1], "group", gitlabPath[0], "url", match.HTTPURLToRepo)
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

		logger.Trace("adding remote for GitHub repository", "name", gitlabPath[1], "group", gitlabPath[0], "url", githubUrl)
		if _, err = repo.CreateRemote(&config.RemoteConfig{
			Name:   "github",
			URLs:   []string{githubUrlWithCredentials},
			Mirror: true,
		}); err != nil {
			return err
		}

		logger.Trace("force-pushing to GitHub repository", "name", gitlabPath[1], "group", gitlabPath[0], "url", githubUrl)
		if err = repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			Force:      true,
			//Prune:      true,
		}); err != nil {
			return err
		}

		//mergeRequests, _, err := gl.MergeRequests.ListProjectMergeRequests(match.ID, nil)
		//if err != nil {
		//	return err
		//}
		//
		//for _, mr := range mergeRequests {
		//	if mr == nil {
		//		continue
		//	}
		//	fmt.Printf("%#v", *mr)
		//}
		//
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
