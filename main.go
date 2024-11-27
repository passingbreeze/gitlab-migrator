package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
	dateFormat          = "Mon, 2 Jan 2006"
	defaultGithubDomain = "github.com"
	defaultGitlabDomain = "gitlab.com"
)

var deleteExistingRepos, renameMasterToMain bool
var githubDomain, githubRepo, githubToken, githubUser, gitlabDomain, gitlabProject, gitlabToken, projectsCsvPath string

var (
	cache          *objectCache
	client         *http.Client
	logger         hclog.Logger
	gh             *github.Client
	gl             *gitlab.Client
	maxConcurrency int
)

type Project = []string

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

	cache = newObjectCache()

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

	flag.StringVar(&githubDomain, "github-domain", defaultGithubDomain, "specifies the GitHub domain to use")
	flag.StringVar(&githubRepo, "github-repo", "", "the GitHub repository to migrate to")
	flag.StringVar(&githubUser, "github-user", "", "specifies the GitHub user to use, who will author any migrated PRs (required)")
	flag.StringVar(&gitlabDomain, "gitlab-domain", defaultGitlabDomain, "specifies the GitLab domain to use")
	flag.StringVar(&gitlabProject, "gitlab-project", "", "the GitLab project to migrate")
	flag.StringVar(&projectsCsvPath, "projects-csv", "", "specifies the path to a CSV file describing projects to migrate (incompatible with -gitlab-project and -github-project)")

	flag.IntVar(&maxConcurrency, "max-concurrency", 4, "how many projects to migrate in parallel")

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
	retryClient.RetryMax = 8
	retryClient.RetryWaitMin = 30 * time.Second
	retryClient.RetryWaitMax = 120 * time.Second

	retryClient.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
		if resp != nil {
			// Check the Retry-After header
			if s, ok := resp.Header["Retry-After"]; ok {
				if sleep, err := strconv.ParseInt(s[0], 10, 64); err == nil {
					return time.Second * time.Duration(sleep)
				}
			}

			// Reference:
			// - https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api?apiVersion=2022-11-28
			// - https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api?apiVersion=2022-11-28
			if v, ok := resp.Header["X-Ratelimit-Remaining"]; ok {
				if remaining, err := strconv.ParseInt(v[0], 10, 64); err == nil && remaining == 0 {

					// If x-ratelimit-reset is present, this indicates the UTC timestamp when we can retry
					if w, ok := resp.Header["X-Ratelimit-Reset"]; ok {
						if recoveryEpoch, err := strconv.ParseInt(w[0], 10, 64); err == nil {
							return time.Until(time.Unix(recoveryEpoch, 0))
						}
					}

					// Otherwise, wait for 60 seconds
					return 60 * time.Second
				}
			}
		}

		// Exponential backoff
		mult := math.Pow(2, float64(attemptNum)) * float64(min)
		sleep := time.Duration(mult)
		if float64(sleep) != mult || sleep > max {
			sleep = max
		}

		return sleep
	}

	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if err != nil {
			return false, err
		}

		// Potential connection reset
		if resp == nil {
			return true, nil
		}

		retryableStatuses := []int{
			http.StatusTooManyRequests, // rate-limiting
			http.StatusForbidden,       // rate-limiting

			http.StatusRequestTimeout,
			http.StatusFailedDependency,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		}

		for _, status := range retryableStatuses {
			if resp.StatusCode == status {
				return true, nil
			}
		}

		return false, nil
	}

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

	projects := make([]Project, 0)
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
		projects = []Project{{gitlabProject, githubRepo}}
	}

	if errCount, err := performMigration(ctx, projects); err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	} else if errCount > 0 {
		logger.Warn(fmt.Sprintf("encountered %d errors during migration, review log output for details", errCount))
	}
}

func performMigration(ctx context.Context, projects []Project) (int, error) {
	concurrency := maxConcurrency
	if len(projects) < maxConcurrency {
		concurrency = len(projects)
	}

	logger.Info(fmt.Sprintf("processing %d project(s) with %d workers", len(projects), concurrency))

	var wg sync.WaitGroup
	var errCount int
	queue := make(chan Project, concurrency*2)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for proj := range queue {
				if err := ctx.Err(); err != nil {
					break
				}

				if err := migrateProject(ctx, proj); err != nil {
					errCount++
					logger.Error(err.Error())
				}
			}
		}()
	}

	for _, proj := range projects {
		if err := ctx.Err(); err != nil {
			break
		}

		queue <- proj
	}

	close(queue)
	wg.Wait()

	return errCount, nil
}

func migrateProject(ctx context.Context, proj []string) error {
	gitlabPath := strings.Split(proj[0], "/")
	githubPath := strings.Split(proj[1], "/")

	logger.Info("searching for GitLab project", "name", gitlabPath[1], "group", gitlabPath[0])
	searchTerm := gitlabPath[1]
	projectResult, _, err := gl.Projects.ListProjects(&gitlab.ListProjectsOptions{Search: &searchTerm})
	if err != nil {
		return fmt.Errorf("listing projects: %v", err)
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
		return fmt.Errorf("parsing clone URL: %v", err)
	}

	logger.Info("mirroring repository from GitLab to GitHub", "name", gitlabPath[1], "group", gitlabPath[0])

	user, err := getGithubUser(ctx, githubPath[0])
	if err != nil {
		return fmt.Errorf("retrieving github user: %v", err)
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
		if _, err := gh.Repositories.Delete(ctx, githubPath[0], githubPath[1]); err != nil {
			return fmt.Errorf("deleting existing github repo: %v", err)
		}

		deleted = true
	}

	defaultBranch := "main"
	if !renameMasterToMain && project.DefaultBranch != "" {
		defaultBranch = project.DefaultBranch
	}
	logger.Trace("setting default branch", "owner", githubPath[0], "repo", githubPath[1], "default_branch", defaultBranch)

	if deleted || err != nil {
		var githubError *github.ErrorResponse
		if err != nil && (!errors.As(err, &githubError) || githubError == nil || githubError.Response == nil || githubError.Response.StatusCode != http.StatusNotFound) {
			return fmt.Errorf("retrieving github repo: %v", err)
		}

		if deleted {
			logger.Warn("recreating GitHub repository", "owner", githubPath[0], "repo", githubPath[1])
		} else {
			logger.Debug("repository not found on GitHub, proceeding to create", "owner", githubPath[0], "repo", githubPath[1])
		}
		newRepo := github.Repository{
			Name:          pointer(githubPath[1]),
			Description:   &project.Description,
			DefaultBranch: &defaultBranch,
			Private:       pointer(true),
			HasIssues:     pointer(true),
			HasProjects:   pointer(true),
			HasWiki:       pointer(true),
		}
		if _, _, err = gh.Repositories.Create(ctx, org, &newRepo); err != nil {
			return fmt.Errorf("creating github repo: %v", err)
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
	if _, _, err = gh.Repositories.Edit(ctx, githubPath[0], githubPath[1], &updateRepo); err != nil {
		return fmt.Errorf("updating github repo: %v", err)
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
		return fmt.Errorf("cloning gitlab repo: %v", err)
	}

	if renameMasterToMain {
		if masterBranch, err := repo.Reference(plumbing.NewBranchReferenceName("master"), false); err == nil {
			logger.Info("renaming master branch to main prior to push", "repo", proj[1], "sha", masterBranch.Hash())

			logger.Debug("creating main branch", "repo", proj[1], "sha", masterBranch.Hash())
			mainBranch := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), masterBranch.Hash())
			if err = repo.Storer.SetReference(mainBranch); err != nil {
				return fmt.Errorf("creating main branch: %v", err)
			}

			logger.Debug("deleting master branch", "repo", proj[1], "sha", masterBranch.Hash())
			if err = repo.Storer.RemoveReference(masterBranch.Name()); err != nil {
				return fmt.Errorf("deleting master branch: %v", err)
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
		return fmt.Errorf("adding github remote: %v", err)
	}

	logger.Debug("force-pushing to GitHub repository", "name", gitlabPath[1], "group", gitlabPath[0], "url", githubUrl)
	if err = repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      true,
		//Prune:      true, // causes error, attempts to delete main branch
	}); err != nil {
		upToDateError := errors.New("already up-to-date")
		if errors.As(err, &upToDateError) {
			logger.Debug("repository already up-to-date on GitHub", "name", gitlabPath[1], "group", gitlabPath[0], "url", githubUrl)
		} else {
			return fmt.Errorf("pushing to github repo: %v", err)
		}
	}

	logger.Debug("setting default repository branch", "owner", githubPath[0], "repo", githubPath[1], "branch_name", defaultBranch)
	updateRepo = github.Repository{
		DefaultBranch: &defaultBranch,
	}
	if _, _, err = gh.Repositories.Edit(ctx, githubPath[0], githubPath[1], &updateRepo); err != nil {
		return fmt.Errorf("setting default branch: %v", err)
	}

	logger.Debug("retrieving GitLab merge requests", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID)
	mergeRequests, _, err := gl.MergeRequests.ListProjectMergeRequests(project.ID, &gitlab.ListProjectMergeRequestsOptions{OrderBy: pointer("created_at"), Sort: pointer("asc")})
	if err != nil {
		return fmt.Errorf("retrieving gitlab merge requests: %v", err)
	}

	logger.Info("migrating merge requests from GitLab to GitHub", "name", gitlabPath[1], "group", gitlabPath[0])
	for _, mergeRequest := range mergeRequests {
		if mergeRequest == nil {
			continue
		}

		var cleanUpBranch bool
		var pullRequest *github.PullRequest

		logger.Debug("searching for any existing pull request", "repo", proj[1], "merge_request_id", mergeRequest.IID)
		//query := fmt.Sprintf("repo:%s/%s is:pr head:%s", githubPath[0], githubPath[1], mergeRequest.SourceBranch)
		query := fmt.Sprintf("repo:%s/%s is:pr", githubPath[0], githubPath[1])
		searchResult, err := getGithubSearchResults(ctx, query)
		if err != nil {
			return fmt.Errorf("listing pull requests: %v", err)
		}

		// Look for an existing GitHub pull request
		for _, issue := range searchResult.Issues {
			if issue == nil {
				continue
			}

			if issue.IsPullRequest() {
				// Extract the PR number from the URL
				prUrl, err := url.Parse(*issue.PullRequestLinks.URL)
				if err != nil {
					return fmt.Errorf("parsing pull request url: %v", err)
				}

				if m := regexp.MustCompile(".+/([0-9]+)$").FindStringSubmatch(prUrl.Path); len(m) == 2 {
					prNumber, _ := strconv.Atoi(m[1])
					pr, err := getGithubPullRequest(ctx, githubPath[0], githubPath[1], prNumber)
					if err != nil {
						return fmt.Errorf("retrieving pull request: %v", err)
					}

					if strings.Contains(pr.GetBody(), fmt.Sprintf("**GitLab MR Number** | %d", mergeRequest.IID)) {
						logger.Debug("found existing pull request", "repo", proj[1], "pull_request_id", pr.GetID())
						pullRequest = pr
					}
				}
			}
		}

		// Proceed to create temporary branches when migrating a merged/closed merge request that doesn't yet have a counterpart PR in GitHub (can't create one without a branch)
		if pullRequest == nil && !strings.EqualFold(mergeRequest.State, "opened") {
			logger.Trace("searching for existing branch for closed/merged merge request", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch)

			// Only create temporary branches if the source branch has been deleted
			if _, err = repo.Reference(plumbing.ReferenceName(mergeRequest.SourceBranch), false); err != nil {
				// Create a worktree
				worktree, err := repo.Worktree()
				if err != nil {
					return fmt.Errorf("creating worktree: %v", err)
				}

				// Generate temporary branch names
				mergeRequest.SourceBranch = fmt.Sprintf("migration-source-%d/%s", mergeRequest.IID, mergeRequest.SourceBranch)
				mergeRequest.TargetBranch = fmt.Sprintf("migration-target-%d/%s", mergeRequest.IID, mergeRequest.TargetBranch)

				logger.Trace("retrieving commits for merge request", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID)
				mergeRequestCommits, _, err := gl.MergeRequests.GetMergeRequestCommits(project.ID, mergeRequest.IID, &gitlab.GetMergeRequestCommitsOptions{OrderBy: "created_at", Sort: "asc"})
				if err != nil {
					return fmt.Errorf("retrieving merge request commits: %v", err)
				}

				// Some merge requests have no commits, disregard these
				if len(mergeRequestCommits) == 0 {
					continue
				}

				// API is buggy, ordering is not respected, so we'll reorder by commit datestamp
				sort.Slice(mergeRequestCommits, func(i, j int) bool {
					return mergeRequestCommits[i].CommittedDate.Before(*mergeRequestCommits[j].CommittedDate)
				})

				if mergeRequestCommits[0] == nil {
					return fmt.Errorf("start commit for merge request %d is nil", mergeRequest.IID)
				}
				if mergeRequestCommits[len(mergeRequestCommits)-1] == nil {
					return fmt.Errorf("end commit for merge request %d is nil", mergeRequest.IID)
				}

				logger.Trace("inspecting start commit", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "sha", mergeRequestCommits[0].ShortID)
				startCommit, err := object.GetCommit(repo.Storer, plumbing.NewHash(mergeRequestCommits[0].ID))
				if err != nil {
					return fmt.Errorf("loading start commit: %v", err)
				}

				// Starting with a merge commit is _probably_ wrong
				if startCommit.NumParents() > 1 {
					return fmt.Errorf("start commit %s for merge request %d has %d parents", mergeRequestCommits[0].ShortID, mergeRequest.IID, startCommit.NumParents())
				}

				if startCommit.NumParents() == 0 {
					// Orphaned commit, start with an empty branch
					// TODO: this isn't working as hoped, try to figure this out. in the meantime, we'll skip MRs from orphaned branches
					//if err = repo.Storer.SetReference(plumbing.NewSymbolicReference("HEAD", plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", mergeRequest.TargetBranch)))); err != nil {
					//	return fmt.Errorf("creating empty branch: %s", err)
					//}
					continue
				} else {
					// Branch out from parent commit
					logger.Trace("inspecting start commit parent", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "sha", mergeRequestCommits[0].ShortID)
					startCommitParent, err := startCommit.Parent(0)
					if err != nil {
						return fmt.Errorf("loading parent commit: %s", err)
					}

					logger.Trace("creating target branch for merged/closed merge request", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.TargetBranch, "sha", startCommitParent.Hash)
					if err = worktree.Checkout(&git.CheckoutOptions{
						Create: true,
						Force:  true,
						Branch: plumbing.NewBranchReferenceName(mergeRequest.TargetBranch),
						Hash:   startCommitParent.Hash,
					}); err != nil {
						return fmt.Errorf("checking out temporary target branch: %v", err)
					}
				}

				endHash := plumbing.NewHash(mergeRequestCommits[len(mergeRequestCommits)-1].ID)
				logger.Trace("creating source branch for merged/closed merge request", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.SourceBranch, "sha", endHash)
				if err = worktree.Checkout(&git.CheckoutOptions{
					Create: true,
					Force:  true,
					Branch: plumbing.NewBranchReferenceName(mergeRequest.SourceBranch),
					Hash:   endHash,
				}); err != nil {
					return fmt.Errorf("checking out temporary source branch: %v", err)
				}

				logger.Debug("pushing branches for merged/closed merge request", "repo", proj[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
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
						logger.Debug("branch already exists and is up-to-date on GitHub", "repo", proj[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
					} else {
						return fmt.Errorf("pushing temporary branches to github: %v", err)
					}
				}

				// We will clean up these temporary branches after configuring and closing the pull request
				cleanUpBranch = true
			}
		}

		if renameMasterToMain && mergeRequest.TargetBranch == "master" {
			logger.Trace("changing target branch from master to main", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID)
			mergeRequest.TargetBranch = "main"
		}

		githubAuthorName := mergeRequest.Author.Name

		author, err := getGitlabUser(mergeRequest.Author.Username)
		if err != nil {
			return fmt.Errorf("retrieving gitlab user: %v", err)
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
			return fmt.Errorf("listing merge request awards: %v", err)
		}
		for _, award := range awards {
			if award.Name == "thumbsup" {
				approver := award.User.Name

				approverUser, err := getGitlabUser(award.User.Username)
				if err != nil {
					return fmt.Errorf("retrieving gitlab user: %v", err)
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

		closeDate := ""
		if mergeRequest.State == "closed" && mergeRequest.ClosedAt != nil {
			closeDate = fmt.Sprintf("\n> | **Date Originally Closed** | %s |", mergeRequest.ClosedAt.Format(dateFormat))
		} else if mergeRequest.State == "merged" && mergeRequest.MergedAt != nil {
			closeDate = fmt.Sprintf("\n> | **Date Originally Merged** | %s |", mergeRequest.MergedAt.Format(dateFormat))
		}

		body := fmt.Sprintf(`> [!NOTE]
> This pull request was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **GitLab Project** | %[4]s/%[5]s |
> | **GitLab MR Number** | %[2]d |
> | **Date Originally Opened** | %[6]s |%[7]s
> | **Approved on GitLab by** | %[8]s |
> |      |      |
>
%[9]s

## Original Description

%[3]s`, githubAuthorName, mergeRequest.IID, description, gitlabPath[0], gitlabPath[1], mergeRequest.CreatedAt.Format(dateFormat), closeDate, approval, originalState)

		if pullRequest == nil {
			logger.Debug("creating pull request", "owner", githubPath[0], "repo", githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
			newPullRequest := github.NewPullRequest{
				Title:               &mergeRequest.Title,
				Head:                &mergeRequest.SourceBranch,
				Base:                &mergeRequest.TargetBranch,
				Body:                &body,
				MaintainerCanModify: pointer(true),
				Draft:               &mergeRequest.Draft,
			}
			if pullRequest, _, err = gh.PullRequests.Create(ctx, githubPath[0], githubPath[1], &newPullRequest); err != nil {
				return fmt.Errorf("creating pull request: %v", err)
			}

			if mergeRequest.State == "closed" || mergeRequest.State == "merged" {
				logger.Debug("closing pull request", "owner", githubPath[0], "repo", githubPath[1], "pull_request_id", pullRequest.GetNumber())

				pullRequest.State = pointer("closed")
				if pullRequest, _, err = gh.PullRequests.Edit(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
					return fmt.Errorf("updating pull request: %v", err)
				}
			}

		} else if (pullRequest.Title == nil || *pullRequest.Title != mergeRequest.Title) ||
			(pullRequest.Body == nil || *pullRequest.Body != body) ||
			(pullRequest.Draft == nil || *pullRequest.Draft != mergeRequest.Draft) {
			var newState *string
			switch mergeRequest.State {
			case "opened":
				newState = pointer("open")
			case "closed", "merged":
				newState = pointer("closed")
			}

			if newState != nil && (pullRequest.State == nil || *pullRequest.State != *newState) {
				logger.Debug("updating pull request", "owner", githubPath[0], "repo", githubPath[1], "pull_request_id", pullRequest.GetNumber())

				pullRequest.Title = &mergeRequest.Title
				pullRequest.Body = &body
				pullRequest.Draft = &mergeRequest.Draft
				pullRequest.State = newState
				if pullRequest, _, err = gh.PullRequests.Edit(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
					return fmt.Errorf("updating pull request: %v", err)
				}
			} else {
				logger.Trace("existing pull request is up-to-date", "owner", githubPath[0], "repo", githubPath[1], "pull_request_id", pullRequest.GetNumber())
			}
		}

		if cleanUpBranch {
			logger.Debug("deleting temporary branches for closed pull request", "repo", proj[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch, "pull_request_id", pullRequest.GetNumber())
			if err = repo.PushContext(ctx, &git.PushOptions{
				RemoteName: "github",
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.SourceBranch)),
					config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.TargetBranch)),
				},
				Force: true,
			}); err != nil {
				upToDateError := errors.New("already up-to-date")
				if errors.As(err, &upToDateError) {
					logger.Debug("branches already deleted on GitHub", "repo", proj[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
				} else {
					return fmt.Errorf("pushing branch deletions to github: %v", err)
				}
			}
		}

		logger.Debug("retrieving GitLab merge request comments", "name", gitlabPath[1], "group", gitlabPath[0], "project_id", project.ID, "merge_request_id", mergeRequest.IID)
		comments, _, err := gl.Notes.ListMergeRequestNotes(project.ID, mergeRequest.IID, &gitlab.ListMergeRequestNotesOptions{OrderBy: pointer("created_at"), Sort: pointer("asc")})
		if err != nil {
			return fmt.Errorf("listing merge request notes: %v", err)
		}

		logger.Debug("retrieving GitHub pull request comments", "owner", githubPath[0], "repo", githubPath[1], "pull_request_id", pullRequest.GetNumber())
		prComments, _, err := gh.Issues.ListComments(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), &github.IssueListCommentsOptions{Sort: pointer("created"), Direction: pointer("asc")})
		if err != nil {
			return fmt.Errorf("listing pull request comments: %v", err)
		}

		for _, comment := range comments {
			if comment == nil || comment.System {
				continue
			}

			githubCommentAuthorName := comment.Author.Name

			commentAuthor, err := getGitlabUser(comment.Author.Username)
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

%[4]s`, githubCommentAuthorName, comment.ID, comment.CreatedAt.Format("Mon, 2 Jan 2006"), comment.Body)

			foundExistingComment := false
			for _, prComment := range prComments {
				if prComment == nil {
					continue
				}

				if strings.Contains(prComment.GetBody(), fmt.Sprintf("**Note ID** | %d", comment.ID)) {
					foundExistingComment = true

					if prComment.Body == nil || *prComment.Body != commentBody {
						logger.Debug("updating pull request comment", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
						prComment.Body = &commentBody
						if _, _, err = gh.Issues.EditComment(ctx, githubPath[0], githubPath[1], prComment.GetID(), prComment); err != nil {
							return fmt.Errorf("updating pull request comments: %v", err)
						}
					}
				} else {
					logger.Trace("existing pull request comment is up-to-date", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
				}
			}

			if !foundExistingComment {
				logger.Debug("creating pull request comment", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())
				newComment := github.IssueComment{
					Body: &commentBody,
				}
				if _, _, err = gh.Issues.CreateComment(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), &newComment); err != nil {
					return fmt.Errorf("creating pull request comments: %v", err)
				}
			}
		}
	}

	return nil
}

func getGithubPullRequest(ctx context.Context, org, repo string, prNumber int) (*github.PullRequest, error) {
	var err error
	cacheToken := fmt.Sprintf("%s/%s/%d", org, repo, prNumber)
	pullRequest := cache.getGithubPullRequest(cacheToken)
	if pullRequest == nil {
		logger.Debug("retrieving user details", "owner", org, "repo", repo, "pr_number", prNumber)
		pullRequest, _, err = gh.PullRequests.Get(ctx, org, repo, prNumber)
		if err != nil {
			return nil, fmt.Errorf("retrieving pull request: %v", err)
		}

		if pullRequest == nil {
			return nil, fmt.Errorf("nil pull request was returned: %d", prNumber)
		}

		logger.Trace("caching GitHub user", "owner", org, "repo", repo, "pr_number", prNumber)
		cache.setGithubPullRequest(cacheToken, pullRequest)
	}

	return pullRequest, nil
}

func getGithubSearchResults(ctx context.Context, query string) (*github.IssuesSearchResult, error) {
	var err error
	result := cache.getGithubSearchResults(query)
	if result == nil {
		logger.Debug("performing search", "query", query)
		result, _, err = gh.Search.Issues(ctx, query, nil)
		if err != nil {
			return nil, fmt.Errorf("performing issue search: %v", err)
		}

		if result == nil {
			return nil, fmt.Errorf("nil search result was returned for query: %s", query)
		}

		logger.Trace("caching GitHub search result", "query", query)
		cache.setGithubSearchResults(query, result)
	}

	return result, nil
}

func getGithubUser(ctx context.Context, username string) (*github.User, error) {
	var err error
	user := cache.getGithubUser(username)
	if user == nil {
		logger.Debug("retrieving user details", "username", username)
		if user, _, err = gh.Users.Get(ctx, username); err != nil {
			return nil, err
		}

		if user == nil {
			return nil, fmt.Errorf("nil user was returned: %s", username)
		}

		logger.Trace("caching GitHub user", "username", username)
		cache.setGithubUser(username, user)
	}

	if user.Type == nil {
		return nil, fmt.Errorf("unable to determine whether owner is a user or organisatition: %s", username)
	}

	return user, nil
}

func getGitlabUser(username string) (*gitlab.User, error) {
	user := cache.getGitlabUser(username)
	if user == nil {
		logger.Debug("retrieving user details", "username", username)
		users, _, err := gl.Users.ListUsers(&gitlab.ListUsersOptions{Username: &username})
		if err != nil {
			return nil, err
		}

		for _, user = range users {
			if user != nil && user.Username == username {
				logger.Trace("caching GitLab user", "username", username)
				cache.setGitlabUser(username, user)

				return user, nil
			}
		}

		return nil, fmt.Errorf("GitLab user not found: %s", username)
	}

	return user, nil
}

func pointer[T any](v T) *T {
	return &v
}
