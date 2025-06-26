package common

import "time"

const (
	// Date format for displaying dates
	DateFormat = "Mon, 2 Jan 2006"

	// Default domains
	DefaultGithubDomain = "github.com"
	DefaultGitlabDomain = "gitlab.com"

	// HTTP retry configuration
	DefaultRetryMax        = 16
	DefaultRetryWaitMin    = 30 * time.Second
	DefaultRetryWaitMax    = 300 * time.Second
	DefaultRateLimitWait   = 60 * time.Second
	DefaultClockSkewBuffer = 30 * time.Second

	// Pagination and limits
	DefaultPerPage        = 100
	QueueBufferMultiplier = 2

	// Branch naming
	MigrationSourceBranchPrefix = "migration-source-%d/%s"
	MigrationTargetBranchPrefix = "migration-target-%d/%s"

	// Default values
	DefaultDescription = "_No description_"
	DefaultApprovers   = "_No approvers_"

	// Git references
	MasterBranchName = "master"
	MainBranchName   = "main"

	MaxTitleLength = 300
)
