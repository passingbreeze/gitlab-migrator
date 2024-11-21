package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"

	"github.com/google/go-github/v66/github"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/xanzy/go-gitlab"
)

const (
	defaultGithubDomain = "github.com"
	defaultGitlabDomain = "gitlab.com"
)

var githubDomain, gitlabDomain string

var client *http.Client
var logger hclog.Logger
var gh *github.Client
var gl *gitlab.Client

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

	flag.StringVar(&githubDomain, "github-domain", defaultGithubDomain, "specifies the GitHub domain to use (defaults to: github.com)")
	flag.StringVar(&gitlabDomain, "gitlab-domain", defaultGitlabDomain, "specifies the GitLab domain to use (defaults to: gitlab.com)")
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

	logger = hclog.New(&hclog.LoggerOptions{
		Name:  "gitlab-migrator",
		Level: hclog.LevelFromString(os.Getenv("LOG_LEVEL")),
	})

	if err := performMigration(ctx); err != nil {
		log.Fatal(err)
	}
}

func performMigration(ctx context.Context) error {
	pullRequests, _, err := gh.PullRequests.List(ctx, "manicminer", "gh-migration-testing", nil)
	if err != nil {
		return err
	}

	for _, pr := range pullRequests {
		if pr == nil {
			continue
		}
		fmt.Printf("%#v", *pr)
	}

	return nil
}
