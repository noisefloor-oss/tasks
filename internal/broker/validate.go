package broker

import (
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// nameCharset is the shared printable subset for bound identifiers.
const nameCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"

func validName(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return !strings.ContainsRune(nameCharset, r)
	}) < 0
}

// validBoundPath accepts only absolute, lexically clean paths with no
// relative components and bounded length. Symlink resolution is handled
// separately by the no-follow walker.
func validBoundPath(p string) bool {
	if p == "" || len(p) > maxPathBytes || !utf8.ValidString(p) {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// splitBranch validates the §9.4 branch shape tasks/<tag>/<id> and returns
// tag and id.
func splitBranch(branch string) (tag, id string, err error) {
	if len(branch) > maxBranchByte || !strings.HasPrefix(branch, "tasks/") {
		return "", "", refuse("branch is not a tasks branch")
	}
	rest := branch[len("tasks/"):]
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 || i == len(rest)-1 {
		return "", "", refuse("branch is not a tasks branch")
	}
	tag, id = rest[:i], rest[i+1:]
	if strings.Contains(tag, "/") || !validName(tag, maxTagBytes) {
		return "", "", refuse("branch tag is invalid")
	}
	if !validTaskID(id) {
		return "", "", refuse("branch task id is invalid")
	}
	return tag, id, nil
}

// validTaskID is the §1 id grammar: a positive integer with no leading zero.
func validTaskID(id string) bool {
	if id == "" || len(id) > 19 {
		return false
	}
	if id[0] == '0' {
		return false
	}
	for i := range len(id) {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

// validCommit accepts the full lowercase object ids emitted by supported Git
// repositories. Abbreviated or symbolic revisions are never capability
// inputs: the saga binds the exact commit it planned.
func validCommit(commit string) bool {
	if len(commit) != 40 && len(commit) != maxCommitBytes {
		return false
	}
	for i := range len(commit) {
		if commit[i] < '0' || commit[i] > '9' {
			if commit[i] < 'a' || commit[i] > 'f' {
				return false
			}
		}
	}
	return true
}

// ValidateBindings checks every capability binding that can be judged
// without touching the filesystem: verb, identifier shapes, path shape,
// branch/target agreement, and identity range.
func (c *Capability) ValidateBindings() error {
	switch c.Verb {
	case VerbCreate, VerbRemove, VerbPruneRegistration:
	default:
		return refuse("verb is not supported")
	}
	if !validName(c.FamilyID, maxFamilyID) {
		return refuse("family id is invalid")
	}
	if !validName(c.SagaFence, maxSagaFence) {
		return refuse("saga fence is invalid")
	}
	if !validBoundPath(c.RegistrationRoot) {
		return refuse("registration root is invalid")
	}
	if !validBoundPath(c.OwnerBoundary) {
		return refuse("owner boundary is invalid")
	}
	if !validBoundPath(c.Target) {
		return refuse("target is invalid")
	}
	tag, id, err := splitBranch(c.Branch)
	if err != nil {
		return err
	}
	if c.Target != filepath.Join(c.OwnerBoundary, ".worktrees", tag+"-"+id) {
		return refuse("target does not match the owner boundary and branch")
	}
	// §9.5 binds the exact saga-recorded base commit, null only for exact
	// legacy cleanup: empty is accepted solely for prune-registration,
	// while a non-empty base must be a full object id for every verb.
	if c.BaseCommit == "" {
		if c.Verb != VerbPruneRegistration {
			return refuse("base commit is invalid")
		}
	} else if !validCommit(c.BaseCommit) {
		return refuse("base commit is invalid")
	}
	if c.UID < 0 || c.GID < 0 {
		return refuse("invoking identity is invalid")
	}
	if c.Force && c.Verb != VerbRemove {
		return refuse("force is bound to remove only")
	}
	return nil
}
