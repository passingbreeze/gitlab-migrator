# GitLab to GitHub Repository Migration Tool

A comprehensive command-line tool for migrating projects from GitLab to GitHub repositories. This tool provides enterprise-grade migration capabilities with the following features:

## Features

* **Complete Repository Migration**: Migrate git repositories with full commit history, branches, and tags
* **Pull Request Migration**: Convert GitLab merge requests to GitHub pull requests, including:
  - Open, closed, and merged requests
  - Comments and discussions
  - Original author attribution
  - Approval tracking via thumbs-up reactions
* **Branch Management**: Automatic `master` to `main` branch renaming
* **Secure Authentication**: Token-based authentication with no credential exposure
* **Concurrent Processing**: Parallel migration of multiple projects with configurable concurrency
* **Idempotent Operations**: Safe to run multiple times - updates existing repositories and pull requests
* **Enterprise Support**: Works with GitLab self-hosted and GitHub Enterprise instances

**Not Currently Supported**: Issues, wikis, project settings, or other GitLab-specific features. Contributions welcome!

## Requirements

- Go 1.23 or later
- Valid GitLab and GitHub API tokens
- Network access to both GitLab and GitHub instances

## Installing

```bash
go install github.com/manicminer/gitlab-migrator
```

Or build from source:

```bash
git clone https://github.com/manicminer/gitlab-migrator.git
cd gitlab-migrator
go build -o gitlab-migrator
```

## Authentication Setup

Before using the tool, set up your authentication tokens:

```bash
export GITHUB_TOKEN="your_github_personal_access_token"
export GITLAB_TOKEN="your_gitlab_personal_access_token"
export GITHUB_USER="your_github_username"  # Optional, can use -github-user flag
```

**Required Token Permissions:**
- **GitHub**: `repo`, `delete_repo` (if using -delete-existing-repos)
- **GitLab**: `api`, `read_repository`

## Usage

### Basic Migration

```bash
# Migrate a single project
gitlab-migrator \
  -github-user=mytokenuser \
  -gitlab-project=mygitlabuser/myproject \
  -github-repo=mygithubuser/myrepo \
  -migrate-pull-requests

# Migrate with master->main branch rename
gitlab-migrator \
  -github-user=mytokenuser \
  -gitlab-project=mygitlabuser/myproject \
  -github-repo=mygithubuser/myrepo \
  -migrate-pull-requests \
  -rename-master-to-main
```

### Bulk Migration

```bash
# Create a CSV file with project mappings
echo "gitlab-group/project1,github-org/repo1" > projects.csv
echo "gitlab-group/project2,github-org/repo2" >> projects.csv

# Migrate multiple projects
gitlab-migrator \
  -github-user=mytokenuser \
  -projects-csv=projects.csv \
  -migrate-pull-requests \
  -max-concurrency=2
```

## Command Line Options

```
  -delete-existing-repos
        whether existing repositories should be deleted before migrating
  -github-domain string
        specifies the GitHub domain to use (default "github.com")
  -github-repo string
        the GitHub repository to migrate to
  -github-user string
        specifies the GitHub user to use, who will author any migrated PRs. can also be sourced from GITHUB_USER environment variable (required)
  -gitlab-domain string
        specifies the GitLab domain to use (default "gitlab.com")
  -gitlab-project string
        the GitLab project to migrate
  -loop
        continue migrating until canceled
  -max-concurrency int
        how many projects to migrate in parallel (default 4)
  -migrate-pull-requests
        whether pull requests should be migrated
  -projects-csv string
        specifies the path to a CSV file describing projects to migrate (incompatible with -gitlab-project and -github-repo)
  -rename-master-to-main
        rename master branch to main and update pull requests
```

Use the `-github-user` argument to specify the GitHub username for whom the authentication token was issued (mandatory). You can also specify this with the `GITHUB_USER` environment variable.

You can specify an individual GitLab project with the `-gitlab-project` argument, along with the target GitHub repository with the `-github-repo` argument.

Alternatively, you can supply the path to a CSV file with the `-projects-csv` argument, which should contain two columns:

```csv
gitlab-group/gitlab-project-name,github-org-or-user/github-repo-name
```

For authentication, the `GITLAB_TOKEN` and `GITHUB_TOKEN` environment variables must be populated. You cannot specify tokens as command-line arguments.

To enable migration of GitLab merge requests to GitHub pull requests (including closed/merged ones!), specify `-migrate-pull-requests`.

To delete existing GitHub repos prior to migrating, pass the `-delete-existing-repos` argument. _This is potentially dangerous, you won't be asked for confirmation._

Note: If the destination repository does not exist, this tool will attempt to create a private repository. If the destination repo already exists, it will be used unless you specify `-delete-existing-repos`

Specify the location of a self-hosted instance of GitLab with the `-gitlab-domain` argument, or a GitHub Enterprise instance with the `-github-domain` argument.

As a bonus, this tool can transparently rename the `master` branch on your GitLab repository, to `main` on the migrated GitHub repository - enable with the `-rename-master-to-main` argument.

By default, 4 workers will be spawned to migrate up to 4 projects in parallel. You can increase or decrease this with the `-max-concurrency` argument. Note that due to GitHub API rate-limiting, you may not experience any significant speed-up. See [GitHub API docs](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api) for details.

Specify `-loop` to continue migrating projects until canceled. This is useful for daemonizing the migration tool, or automatically restarting when migrating a large number of projects (or a small number of very large projects).

## Advanced Usage

### Enterprise Instances

```bash
# Self-hosted GitLab and GitHub Enterprise
gitlab-migrator \
  -github-user=mytokenuser \
  -gitlab-domain=gitlab.mycompany.com \
  -github-domain=github.mycompany.com \
  -gitlab-project=mygroup/myproject \
  -github-repo=myorg/myrepo \
  -migrate-pull-requests
```

### Continuous Migration

```bash
# Run continuously for ongoing synchronization
gitlab-migrator \
  -github-user=mytokenuser \
  -projects-csv=projects.csv \
  -migrate-pull-requests \
  -loop
```

### Reporting Mode

```bash
# Get a report without migrating
gitlab-migrator \
  -github-user=mytokenuser \
  -projects-csv=projects.csv \
  -report
```

## Logging

This tool is entirely non-interactive and provides structured logging output. Configure logging verbosity with the `LOG_LEVEL` environment variable:

```bash
export LOG_LEVEL=DEBUG  # Options: ERROR, WARN, INFO, DEBUG, TRACE (default: INFO)
```

**Log Levels:**
- `ERROR`: Critical errors only
- `WARN`: Warnings and errors  
- `INFO`: General progress information (default)
- `DEBUG`: Detailed operation information
- `TRACE`: Very verbose debugging output

## Caching

The tool maintains a thread-safe in-memory cache for certain primitives, in order to help reduce the number of API requests being made. At this time, the following are cached the first time they are encountered, and thereafter retrieved from the cache until the tool is restarted:

- GitHub pull requests
- GitHub issue search results
- GitHub user profiles
- GitLab user profiles

## Idempotence

This tool tries to be idempotent. You can run it over and over and it will patch the GitHub repository, along with its pull requests, to match what you have in GitLab. This should help you migrate a number of projects without enacting a large maintenance window.

_Note that this tool performs a forced mirror push, so it's not recommended to run this tool after commencing work in the target repository._

For pull requests and their comments, the corresponding IDs from GitLab are added to the Markdown header, this is parsed to enable idempotence (see next section).

## Pull Requests

Whilst the git repository will be migrated verbatim, the pull requests are managed using the GitHub API and typically will be authored by the person supplying the authentication token.

Each pull request, along with every comment, will be prepended with a Markdown table showing the original author and some other metadata that is useful to know.  This is also used to map pull requests and their comments to their counterparts in GitLab and enables the tool to be idempotent.

As a bonus, if your GitLab users add the URL to their GitHub profile in the `Website` field of their GitLab profile, this tool will add a link to their GitHub profile in the markdown header of any PR or comment they originally authored.

This tool also migrates merged/closed merge requests from your GitLab projects. It does this by reconstructing temporary branches in each repo, pushing them to GitHub, creating then closing the pull request, and lastly deleting the temporary branches. Once the tool has completed, you should not have any of these temporary branches in your repo - although GitHub will not garbage collect them immediately such that you can click the `Restore branch` button in any of these PRs.

_Example migrated pull request (open)_

![example migrated open pull request](pr-example-open.jpeg)

_Example migrated pull request (closed)_

![example migrated closed pull request](pr-example-closed.jpeg)

## Architecture

This tool is built with a modern, service-oriented architecture:

- **Service Layer**: Centralized migration services with dependency injection
- **Modular Design**: Separate modules for project migration, pull request handling, and authentication
- **Secure Authentication**: No credential exposure in URLs or logs
- **Thread-Safe Caching**: In-memory caching with concurrent access protection
- **Error Handling**: Comprehensive error tracking and reporting
- **Rate Limiting**: Built-in GitHub API rate limit handling with exponential backoff

## Troubleshooting

### Common Issues

**Authentication Errors**
```bash
# Verify tokens are set correctly
echo $GITHUB_TOKEN | wc -c  # Should be ~40+ characters
echo $GITLAB_TOKEN | wc -c  # Should be ~40+ characters

# Test token access
curl -H "Authorization: token $GITHUB_TOKEN" https://api.github.com/user
curl -H "Authorization: Bearer $GITLAB_TOKEN" https://gitlab.com/api/v4/user
```

**Rate Limiting**
- GitHub has API rate limits (5,000 requests/hour for authenticated users)
- The tool includes automatic retry with exponential backoff
- Use `-max-concurrency=1` for large migrations to reduce rate limit issues

**Memory Usage**
- Large repositories may consume significant memory
- The tool uses in-memory Git operations for safety
- Consider migrating large projects individually

**Network Issues**
- Ensure network access to both GitLab and GitHub instances
- For enterprise instances, verify SSL certificates and proxy settings

### Getting Help

1. Enable debug logging: `export LOG_LEVEL=DEBUG`
2. Check the logs for specific error messages
3. Verify your token permissions and expiration dates
4. For enterprise instances, confirm domain accessibility

## Contributing, reporting bugs etc...

Please use GitHub issues & pull requests. This project is licensed under the MIT license.

### Development

```bash
# Clone and build
git clone https://github.com/manicminer/gitlab-migrator.git
cd gitlab-migrator
go mod download
go build -o gitlab-migrator

# Run tests
go test ./...

# Format code
go fmt ./...
```
