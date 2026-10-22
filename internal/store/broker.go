package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/libexec"
)

// §9.5 confined registration boundary. A permission-class failure of a Git
// write into <common-dir>/worktrees means the registration area is confined;
// the one registration step is then delegated exactly once to the installed
// noise-worktree-broker, resolved by the bounded §5.2 runtime resolution.
// Neither the direct write nor the broker is ever retried a second time.

const (
	// brokerMaxOutput caps how much broker output is consumed at all.
	brokerMaxOutput = 4 << 10
	// brokerMaxReason caps the sanitized refusal reason.
	brokerMaxReason = 200
)

var (
	// errWorktreeConfined is the pinned §9.5 diagnostic for a registration
	// permission failure with no usable broker. The CLI prefixes it with
	// "noise-tasks: " and exits 1; the saga stays recoverable.
	errWorktreeConfined = errors.New("worktree registration is confined and no broker is available")

	// resolveWorktreeBroker resolves the broker executable with the strict
	// §5.2 bounded runtime resolution. Tests replace it with fakes.
	resolveWorktreeBroker = func() (string, error) {
		return libexec.Resolve("worktree-broker")
	}

	// brokerTimeout bounds one broker invocation.
	brokerTimeout = 30 * time.Second
)

// WorktreeBrokerRefusal is a §9.5 broker refusal (exit 1): the broker ran and
// answered no. The reason is bounded and sanitized.
type WorktreeBrokerRefusal struct{ Reason string }

func (e *WorktreeBrokerRefusal) Error() string {
	return "worktree broker refused: " + e.Reason
}

// permissionClass reports whether err is a permission-class failure
// (EACCES/EPERM/EROFS, or Git's C-locale rendering of them).
func permissionClass(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "operation not permitted") ||
		strings.Contains(msg, "read-only file system")
}

// gitRegistrationEnvironment preserves the caller's Git configuration while
// pinning Git's diagnostic locale. Registration confinement is necessarily
// classified from Git's child-process error text, so that text must not vary
// with the invoking agent's locale.
func gitRegistrationEnvironment(inherited []string) []string {
	out := make([]string, 0, len(inherited)+2)
	for _, item := range inherited {
		name, _, _ := strings.Cut(item, "=")
		if name != "LANG" && name != "LC_ALL" {
			out = append(out, item)
		}
	}
	return append(out, "LANG=C", "LC_ALL=C")
}

func gitRegistrationRun(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitRegistrationEnvironment(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// registrationRoots returns exact spellings Git may use for the canonical
// <common-dir>/worktrees root: canonical/absolute and boundary-relative. No
// generic path component is accepted as registration evidence.
func registrationRoots(boundary string) []string {
	out, err := gitRegistrationRun(boundary, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(boundary, dir)
	}
	raw := filepath.Clean(filepath.Join(dir, "worktrees"))
	canonical := filepath.Join(canonicalPath(dir), "worktrees")
	roots := []string{canonical}
	if raw != canonical {
		roots = append(roots, raw)
	}
	for _, root := range append([]string(nil), roots...) {
		if rel, err := filepath.Rel(boundary, root); err == nil && !filepath.IsAbs(rel) {
			rel = filepath.Clean(rel)
			if rel != "." && !slices.Contains(roots, rel) {
				roots = append(roots, rel)
			}
		}
	}
	return roots
}

// registrationPermissionFailure reports whether err is a permission-class
// failure of a worktree registration write: a permission-class error whose
// message names the registration area — the canonical
// <common-dir>/worktrees root when Git names it absolutely, else Git's
// relative "<common-dir>/worktrees/..." spelling. The sandbox root's
// ".worktrees" spelling (dot-prefixed) never matches "/worktrees/".
func registrationPermissionFailure(boundary string, err error) bool {
	if !permissionClass(err) {
		return false
	}
	msg := filepath.ToSlash(err.Error())
	for _, root := range registrationRoots(boundary) {
		// Every Git worktree registration mutation addresses an entry below the
		// root. Requiring the exact computed root plus a separator prevents a
		// sandbox path (or an ancestor merely named "worktrees") from matching.
		if messageNamesRegistrationRoot(msg, filepath.ToSlash(root)) {
			return true
		}
	}
	return false
}

func messageNamesRegistrationRoot(message, root string) bool {
	needle := root + "/"
	for offset := 0; ; {
		i := strings.Index(message[offset:], needle)
		if i < 0 {
			return false
		}
		i += offset
		if i == 0 || !registrationPathByte(message[i-1]) {
			return true
		}
		offset = i + 1
	}
}

// registrationPathByte identifies bytes that can continue a surrounding path.
// Requiring a non-path boundary keeps a relative spelling such as
// ".git/worktrees" from matching the middle of an absolute ancestor path.
func registrationPathByte(b byte) bool {
	return b == '/' || b == '.' || b == '_' || b == '-' || b == '~' ||
		b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

// brokerEnvironment is the stripped, fixed broker environment (§9.5): no
// inherited Git configuration, no claim/run tokens, no credential sources —
// only what a child Git needs to run.
func brokerEnvironment() []string {
	return []string{
		"LANG=C",
		"LC_ALL=C",
		"PATH=/usr/bin:/bin",
		"HOME=/nonexistent",
	}
}

// boundedBuffer keeps the first cap bytes written to it and silently drains
// the rest, so a chatty child can never wedge the pipe or the diagnostic.
type boundedBuffer struct {
	buf bytes.Buffer
	cap int
}

func newBoundedBuffer(cap int) *boundedBuffer { return &boundedBuffer{cap: cap} }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.cap - b.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf.Write(p[:room])
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

// sanitizeBrokerReason reduces broker output to a bounded printable-ASCII
// single-line reason: control characters and invalid UTF-8 become spaces,
// whitespace collapses, and anything past brokerMaxReason is dropped.
func sanitizeBrokerReason(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 0x20 && r <= 0x7E:
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	reason := strings.Join(strings.Fields(b.String()), " ")
	if len(reason) > brokerMaxReason {
		reason = reason[:brokerMaxReason]
	}
	return reason
}

// delegateWorktreeBroker performs the one delegated broker invocation with a
// fixed argv and the stripped environment, consuming only bounded output. The
// real broker accepts exactly one argument — create|remove|prune-registration —
// and obtains the already-minted host capability from inherited fd 3.
func delegateWorktreeBroker(dir, verb string) error {
	capability, err := openBrokerCapabilityFile()
	if err != nil {
		return err
	}
	defer capability.Close()
	exe, err := resolveWorktreeBroker()
	if err != nil {
		// No strictly valid broker is installed: the pinned confinement
		// diagnostic, never a raw resolution error.
		return errWorktreeConfined
	}
	ctx, cancel := context.WithTimeout(context.Background(), brokerTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, verb)
	cmd.Dir = dir
	cmd.Env = brokerEnvironment()
	cmd.ExtraFiles = []*os.File{capability}
	if stdin := brokerCapabilityStdin(capability); stdin != nil {
		cmd.Stdin = stdin
	}
	configureBrokerProcess(cmd)
	stdout := newBoundedBuffer(brokerMaxOutput)
	stderr := newBoundedBuffer(brokerMaxOutput)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	reapBrokerProcessGroup(cmd)
	if runErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return &WorktreeBrokerRefusal{Reason: "broker timed out"}
	case errors.As(runErr, &exitErr):
		reason := sanitizeBrokerReason(stderr.String())
		if reason == "" {
			reason = fmt.Sprintf("broker exited %d", exitErr.ExitCode())
		}
		return &WorktreeBrokerRefusal{Reason: reason}
	default:
		// The broker could not be executed at all: not an available broker.
		return errWorktreeConfined
	}
}

// runWorktreeRegistration performs one registration-affecting Git worktree
// verb directly and, when that write fails for a permission-class reason on
// the <common-dir>/worktrees registration area, delegates the registration
// step exactly once to the strictly resolved broker (SPEC §9.5). gitArgs are
// the direct fixed Git argv. The broker receives all operation bindings from
// the fd-3 host capability, never from argv.
func runWorktreeRegistration(boundary, verb string, gitArgs []string) error {
	_, err := gitRegistrationRun(boundary, gitArgs...)
	if err == nil || !registrationPermissionFailure(boundary, err) {
		return err
	}
	return delegateWorktreeBroker(boundary, verb)
}
