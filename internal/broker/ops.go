package broker

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// gitExecutable is an installation trust input, never caller PATH data.
	// Both Linux and macOS provide the system Git entry point here.
	gitExecutable = "/usr/bin/git"
	// Successful porcelain must be complete to decide exact idempotency. The
	// writers retain at most these limits while draining all surplus bytes.
	maxGitStdout = 8 * 1024 * 1024
	maxGitStderr = 64 * 1024
)

// Result is the bounded success report for one verb.
type Result struct {
	Verb    string
	Changed bool
}

// gitEnvironment is the complete, fixed environment for every Git child:
// no inherited value, credential, or configuration surface survives.
func gitEnvironment() []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=/nonexistent",
		"LC_ALL=C",
		"LANG=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	}
}

type boundedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	want := b.limit - b.buf.Len()
	if want > len(p) {
		want = len(p)
	}
	if want > 0 {
		_, _ = b.buf.Write(p[:want])
	}
	if want < len(p) {
		b.overflow = true
	}
	// Report the full write so os/exec keeps draining rather than turning an
	// output limit into a child-side broken pipe or deadlock.
	return len(p), nil
}

// gitRaw runs one fixed-executable, fixed-argv Git command. Stdout and stderr
// are bounded while the process runs; successful structured output is either
// complete or refused, never silently truncated.
func gitRaw(boundary string, args ...string) ([]byte, error) {
	argv := append([]string{"-C", boundary}, args...)
	cmd := exec.Command(gitExecutable, argv...)
	cmd.Env = gitEnvironment()
	stdout := &boundedBuffer{limit: maxGitStdout}
	stderr := &boundedBuffer{limit: maxGitStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.overflow || stderr.overflow {
		return nil, refuse("git output exceeded its safety bound")
	}
	if err != nil {
		reason := Sanitize(stderr.buf.String())
		if reason == "" {
			reason = Sanitize(stdout.buf.String())
		}
		if reason == "" {
			reason = fmt.Sprintf("git %s failed", args[0])
		}
		return nil, refuse("git refused: %s", reason)
	}
	return append([]byte(nil), stdout.buf.Bytes()...), nil
}

// git runs one fixed-argv Git command in the owner boundary with the
// stripped environment and returns bounded, sanitized output.
func git(boundary string, args ...string) (string, error) {
	out, err := gitRaw(boundary, args...)
	if err != nil {
		return "", err
	}
	return Sanitize(string(out)), nil
}

func gitOneLine(boundary string, args ...string) (string, error) {
	out, err := gitRaw(boundary, args...)
	if err != nil {
		return "", err
	}
	out = bytes.TrimSuffix(out, []byte{'\n'})
	if len(out) == 0 || bytes.ContainsAny(out, "\x00\r\n") {
		return "", refuse("git returned malformed single-line output")
	}
	return string(out), nil
}

// verifyCommonDir proves the bound registration root is exactly the
// worktrees directory of the owner boundary's Git common directory, so the
// broker can never act on another family's registration.
func verifyCommonDir(cap *Capability) error {
	out, err := gitOneLine(cap.OwnerBoundary, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	common := out
	if common == "" {
		return refuse("owner boundary is not a git repository")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(cap.OwnerBoundary, common)
	}
	if filepath.Clean(filepath.Join(common, "worktrees")) != filepath.Clean(cap.RegistrationRoot) {
		return refuse("registration root is not this repository's worktrees directory")
	}
	return nil
}

// registeredWorktrees returns each registered worktree path and its exact
// branch ref from NUL-delimited `git worktree list --porcelain -z` output.
func registeredWorktrees(boundary string) (map[string]string, error) {
	out, err := gitRaw(boundary, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	paths := make(map[string]string)
	current := ""
	for _, raw := range bytes.Split(out, []byte{0}) {
		line := string(raw)
		if line == "" {
			current = ""
			continue
		}
		if rest, ok := strings.CutPrefix(line, "worktree "); ok {
			if rest == "" || current != "" {
				return nil, refuse("git returned malformed worktree porcelain")
			}
			current = rest
			paths[current] = ""
			continue
		}
		if rest, ok := strings.CutPrefix(line, "branch "); ok && current != "" {
			paths[current] = rest
		}
	}
	return paths, nil
}

// Execute performs one idempotent verb after bindings and paths have been
// validated and privilege dropped. It opens no database, mints no path,
// and holds no state.
func Execute(cap *Capability) (*Result, error) {
	if err := verifyCommonDir(cap); err != nil {
		return nil, err
	}
	registered, err := registeredWorktrees(cap.OwnerBoundary)
	if err != nil {
		return nil, err
	}
	switch cap.Verb {
	case VerbCreate:
		return executeCreate(cap, registered)
	case VerbRemove:
		return executeRemove(cap, registered)
	case VerbPruneRegistration:
		return executePruneRegistration(cap, registered)
	default:
		return nil, refuse("verb is not supported")
	}
}

func executeCreate(cap *Capability, registered map[string]string) (*Result, error) {
	if branch, ok := registered[cap.Target]; ok {
		if branch != "refs/heads/"+cap.Branch {
			return nil, refuse("worktree target is registered to another branch")
		}
		// Identical retry: the registration exists; the saga's shared
		// (worktree, requestId) receipt rule decides finality.
		return &Result{Verb: cap.Verb, Changed: false}, nil
	}
	if _, err := os.Lstat(cap.Target); err == nil {
		// The target exists but is not the registered worktree: this is
		// not an identical retry and never a silent takeover.
		return nil, refuse("worktree target exists but is not a registered worktree")
	}
	base, err := gitOneLine(cap.OwnerBoundary, "rev-parse", "--verify", "--quiet", cap.BaseCommit+"^{commit}")
	if err != nil {
		return nil, err
	}
	if base != cap.BaseCommit {
		return nil, refuse("base commit does not resolve exactly")
	}
	if _, err := git(cap.OwnerBoundary, "worktree", "add", "-b", cap.Branch, "--", cap.Target, cap.BaseCommit); err != nil {
		return nil, err
	}
	return &Result{Verb: cap.Verb, Changed: true}, nil
}

func executePruneRegistration(cap *Capability, registered map[string]string) (*Result, error) {
	branch, ok := registered[cap.Target]
	if !ok {
		if _, err := os.Lstat(cap.Target); err == nil {
			return nil, refuse("target exists without its bound registration")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, refuse("cannot inspect target")
		}
		return &Result{Verb: cap.Verb, Changed: false}, nil
	}
	if branch != "refs/heads/"+cap.Branch {
		return nil, refuse("worktree target is registered to another branch")
	}
	if _, err := os.Lstat(cap.Target); err == nil {
		return nil, refuse("cannot prune registration while target exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, refuse("cannot inspect target")
	}
	// Git's scoped remove verb removes exactly this stale registration when
	// the worktree path is absent; unlike `worktree prune`, it cannot affect
	// another saga's registration.
	if _, err := git(cap.OwnerBoundary, "worktree", "remove", "--", cap.Target); err != nil {
		return nil, err
	}
	return &Result{Verb: cap.Verb, Changed: true}, nil
}

func executeRemove(cap *Capability, registered map[string]string) (*Result, error) {
	branch, registeredTarget := registered[cap.Target]
	if !registeredTarget {
		if _, err := os.Lstat(cap.Target); err == nil {
			return nil, refuse("target exists but is not a registered worktree")
		}
		// Already gone: identical retry succeeds without effect.
		return &Result{Verb: cap.Verb, Changed: false}, nil
	}
	if branch != "refs/heads/"+cap.Branch {
		return nil, refuse("worktree target is registered to another branch")
	}
	args := []string{"worktree", "remove"}
	if cap.Force {
		args = append(args, "--force")
	}
	args = append(args, "--", cap.Target)
	if _, err := git(cap.OwnerBoundary, args...); err != nil {
		return nil, err
	}
	return &Result{Verb: cap.Verb, Changed: true}, nil
}
