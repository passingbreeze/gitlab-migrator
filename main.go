package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/google/go-github/v66/github"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/xanzy/go-gitlab"
)

const (
	defaultGithubDomain = "github.com"
	defaultGitlabDomain = "gitlab.com"
)

var githubDomain, gitlabDomain, projectsCsvPath string

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

	flag.StringVar(&githubDomain, "github-domain", defaultGithubDomain, "specifies the GitHub domain to use (defaults to: github.com)")
	flag.StringVar(&gitlabDomain, "gitlab-domain", defaultGitlabDomain, "specifies the GitLab domain to use (defaults to: gitlab.com)")
	flag.StringVar(&projectsCsvPath, "projects-csv", "projects.csv", "specifies the path to a CSV file describing projects to migrate (defaults to: projects.csv)")
	flag.Parse()

	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 5

	client = retryClient.StandardClient()

	if githubDomain == defaultGithubDomain {
		gh = github.NewClient(client).WithAuthToken(os.Getenv("GITHUB_TOKEN"))
	} else {
		githubUrl := fmt.Sprintf("https://%s", githubDomain)
		if gh, err = github.NewClient(client).WithAuthToken(os.Getenv("GITHUB_TOKEN")).WithEnterpriseURLs(githubUrl, githubUrl); err != nil {
			log.Fatal(err)
		}
	}

	gitlabOpts := make([]gitlab.ClientOptionFunc, 0)
	if gitlabDomain != defaultGitlabDomain {
		gitlabUrl := fmt.Sprintf("https://%s", gitlabDomain)
		gitlabOpts = append(gitlabOpts, gitlab.WithBaseURL(gitlabUrl))
	}
	if gl, err = gitlab.NewClient(os.Getenv("GITLAB_TOKEN"), gitlabOpts...); err != nil {
		log.Fatal(err)
	}

	csvFile, err := os.Open(projectsCsvPath)
	if err != nil {
		log.Fatal(err)
	}

	projects, err := csv.NewReader(csvFile).ReadAll()
	if err != nil {
		log.Fatal(err)
	}

	if err := performMigration(ctx, projects); err != nil {
		log.Fatal(err)
	}
}

func performMigration(ctx context.Context, projects Projects) error {
	for _, project := range projects {
		gitlabPath := strings.Split(project[0], "/")
		githubPath := strings.Split(project[1], "/")

		pullRequests, _, err := gh.PullRequests.List(ctx, githubPath[0], githubPath[1], nil)
		if err != nil {
			return err
		}

		for _, pr := range pullRequests {
			if pr == nil {
				continue
			}
			fmt.Printf("%#v", *pr)
		}

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
				match = proj
			}
		}

		if match == nil {
			return fmt.Errorf("no matching GitLab project found for: %s", project[0])
		}

		mergeRequests, _, err := gl.MergeRequests.ListProjectMergeRequests(match.ID, nil)
		if err != nil {
			return err
		}

		for _, mr := range mergeRequests {
			if mr == nil {
				continue
			}
			fmt.Printf("%#v", *mr)
		}
	}

	return nil
}
