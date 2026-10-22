// Package libexec resolves plugins from the installation-selected fixed root.
package libexec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	entryPrefix       = "noise-"
	projectDirEnvName = "NOISE_PROJECT_DIR"
)

var (
	// Root is selected by a release build with -ldflags -X. It is deliberately
	// not configurable at runtime.
	Root = "/usr/local/libexec/noise"

	// Platform defaults to the build target. Tests may select an unsupported
	// value at link time without introducing a runtime override.
	Platform = runtime.GOOS

	ErrInstallation = errors.New("plugin installation error")
	ErrNotInstalled = errors.New("plugin not installed")
	ErrUnsafe       = errors.New("unsafe plugin")
)

// Supported reports whether discovery and dispatch are implemented for the
// selected build platform. Help and version handling occurs before this gate.
func Supported() bool {
	return Platform == "linux" || Platform == "darwin"
}

// ValidNoun applies the complete host grammar before any filesystem access.
func ValidNoun(noun string) bool {
	if noun == "" || strings.HasPrefix(noun, entryPrefix) || reserved(noun) {
		return false
	}
	componentStart := true
	for _, c := range []byte(noun) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			componentStart = false
		case c == '-' && !componentStart:
			componentStart = true
		default:
			return false
		}
	}
	return !componentStart
}

func reserved(noun string) bool {
	switch noun {
	case "help", "version", "plugins":
		return true
	default:
		return false
	}
}

// CanonicalRoot consumes the trust invariant established by setup. Runtime
// resolution is intentionally bounded: it canonicalizes the recorded root
// once and requires a directory, but does not re-audit ancestor ownership.
func CanonicalRoot() (string, error) {
	if !filepath.IsAbs(Root) || filepath.Clean(Root) != Root {
		return "", fmt.Errorf("%w: configured root %q is not an absolute clean path", ErrInstallation, Root)
	}
	canonical, err := filepath.EvalSymlinks(Root)
	if err != nil {
		return "", fmt.Errorf("%w: resolve configured root %q: %v", ErrInstallation, Root, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: inspect configured root %q: %v", ErrInstallation, Root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: configured root %q is not a directory", ErrInstallation, Root)
	}
	return canonical, nil
}

// Resolve returns the canonical executable for noun beneath the canonical
// root. A missing direct entry is distinct from an existing unsafe entry.
func Resolve(noun string) (string, error) {
	root, err := CanonicalRoot()
	if err != nil {
		return "", err
	}
	return resolveInRoot(root, noun)
}

func resolveInRoot(root, noun string) (string, error) {
	if !ValidNoun(noun) {
		return "", fmt.Errorf("%w: invalid noun %q", ErrUnsafe, noun)
	}
	entry := filepath.Join(root, entryPrefix+noun)
	if _, err := os.Lstat(entry); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrNotInstalled, noun)
		}
		return "", fmt.Errorf("%w: inspect %q: %v", ErrUnsafe, entry, err)
	}
	canonical, err := filepath.EvalSymlinks(entry)
	if err != nil {
		return "", fmt.Errorf("%w: resolve %q: %v", ErrUnsafe, entry, err)
	}
	contained, err := beneath(root, canonical)
	if err != nil || !contained {
		return "", fmt.Errorf("%w: %q resolves outside configured root %q", ErrUnsafe, entry, root)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: inspect %q: %v", ErrUnsafe, canonical, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %q is not a regular file", ErrUnsafe, canonical)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: %q is not executable", ErrUnsafe, canonical)
	}
	return canonical, nil
}

func beneath(root, target string) (bool, error) {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false, err
	}
	return rel != "." && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// Discover returns safe direct plugin nouns in ascending bytewise order.
func Discover() ([]string, error) {
	root, err := CanonicalRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("%w: read configured root %q: %v", ErrInstallation, Root, err)
	}
	var nouns []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, entryPrefix) {
			continue
		}
		noun := strings.TrimPrefix(name, entryPrefix)
		if !ValidNoun(noun) {
			continue
		}
		if _, err := resolveInRoot(root, noun); err == nil {
			nouns = append(nouns, noun)
		}
	}
	sort.Strings(nouns)
	return nouns, nil
}

// ProjectEnvironment preserves the inherited environment except for reducing
// NOISE_PROJECT_DIR to exactly one entry. A nonempty inherited value wins;
// otherwise the canonical current directory supplies the context.
func ProjectEnvironment(inherited []string) ([]string, error) {
	out := make([]string, 0, len(inherited)+1)
	projectDir := ""
	for _, item := range inherited {
		name, value, found := strings.Cut(item, "=")
		if name != projectDirEnvName {
			out = append(out, item)
			continue
		}
		if found && value != "" && projectDir == "" {
			projectDir = value
		}
	}
	if projectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve current directory: %w", err)
		}
		projectDir, err = filepath.EvalSymlinks(cwd)
		if err != nil {
			return nil, fmt.Errorf("canonicalize current directory %q: %w", cwd, err)
		}
		info, err := os.Stat(projectDir)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("current directory %q is not an existing directory", projectDir)
		}
	}
	out = append(out, projectDirEnvName+"="+projectDir)
	return out, nil
}
