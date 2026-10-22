package backlog

import (
	"os"
	"path/filepath"
	"strings"
)

// GitCommonDir resolves the Git common directory for a repository boundary.
// For a normal `.git` directory it is that directory; for a `.git` file it
// follows the `gitdir:` target and then the target's `commondir` contents when
// present. ok is false when the common directory cannot be resolved (outside
// Git, or an unsupported/unreadable marker).
func GitCommonDir(boundary string) (string, bool) {
	marker := filepath.Join(boundary, ".git")
	info, err := os.Lstat(marker)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return marker, true
	}
	if !info.Mode().IsRegular() {
		return "", false
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return "", false
	}
	const prefix = "gitdir:"
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if gitdir == "" {
		return "", false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(boundary, gitdir)
	}
	if commondir, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		if c := strings.TrimSpace(string(commondir)); c != "" {
			if !filepath.IsAbs(c) {
				c = filepath.Join(gitdir, c)
			}
			return filepath.Clean(c), true
		}
	}
	return filepath.Clean(gitdir), true
}
