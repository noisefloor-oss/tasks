package backlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const projectEnv = "NOISE_PROJECT_DIR"

// ProjectContext returns the canonical starting directory and its discovery
// boundary. Outside Git, both values are the starting directory.
func ProjectContext() (start, boundary string, err error) {
	if configured := os.Getenv(projectEnv); configured != "" {
		if !filepath.IsAbs(configured) {
			return "", "", fmt.Errorf("%s must be an absolute directory: %q", projectEnv, configured)
		}
		start, err = canonicalDirectory(configured)
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", projectEnv, err)
		}
	} else {
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			return "", "", cwdErr
		}
		start, err = canonicalDirectory(cwd)
		if err != nil {
			return "", "", err
		}
	}
	boundary, err = gitBoundary(start)
	if errors.Is(err, errNoGitBoundary) {
		return start, start, nil
	}
	if err != nil {
		return "", "", err
	}
	return start, boundary, nil
}

func canonicalDirectory(path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve directory %q: %w", path, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("inspect directory %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	return canonical, nil
}

var errNoGitBoundary = errors.New("no git boundary")

func gitBoundary(start string) (string, error) {
	for dir := start; ; dir = filepath.Dir(dir) {
		marker := filepath.Join(dir, ".git")
		info, err := os.Lstat(marker)
		switch {
		case err == nil && (info.IsDir() || info.Mode().IsRegular()):
			return dir, nil
		case err == nil:
			// A symlink or special file is not a repository marker.
		case !errors.Is(err, os.ErrNotExist):
			return "", fmt.Errorf("inspect git marker %q: %w", marker, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errNoGitBoundary
		}
	}
}
