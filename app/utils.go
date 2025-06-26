package app

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
)

// Project represents a GitLab project path and GitHub repository mapping.
type Project = []string

// LoadProjects loads the list of projects to migrate from CSV file or inline arguments.
func LoadProjects(csvPath, gitlabProject, githubRepo string) ([]Project, error) {
	if csvPath != "" {
		return loadProjectsFromCSV(csvPath)
	}
	return []Project{{gitlabProject, githubRepo}}, nil
}

// loadProjectsFromCSV loads projects from a CSV file.
func loadProjectsFromCSV(csvPath string) ([]Project, error) {
	data, err := os.ReadFile(csvPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read projects csv file: %w", err)
	}

	// Trim UTF-8 BOM if present
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	r := csv.NewReader(bytes.NewBuffer(data))
	projects, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to parse CSV file: %w", err)
	}

	// Validate project format
	for i, project := range projects {
		if len(project) != 2 {
			return nil, fmt.Errorf("invalid project format at line %d: expected 2 columns, got %d", i+1, len(project))
		}
		if project[0] == "" || project[1] == "" {
			return nil, fmt.Errorf("empty project values at line %d", i+1)
		}
	}

	return projects, nil
}
