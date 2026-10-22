//go:build unix

package broker

import (
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openNoFollow walks each component of the absolute, already-validated
// path p with openat(O_NOFOLLOW), proving that no component — including
// the final one — is a symlink, and that the endpoint is a directory.
// It reports a missing endpoint as exists=false and any other deviation
// as a refusal.
func openNoFollow(p string) (bool, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, refuse("cannot walk path root")
	}
	defer unix.Close(fd)
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, part := range parts {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) && i == len(parts)-1 {
				return false, nil
			}
			if errors.Is(err, unix.ENOTDIR) {
				return false, refuse("path component is not a directory: %s", part)
			}
			return false, refuse("path component is unavailable: %s", part)
		}
		unix.Close(fd)
		fd = next
	}
	return true, nil
}

// statChildNoFollow stats one child of an already-validated directory
// without following a symlink, reporting whether it exists and whether it
// is a real directory.
func statChildNoFollow(dir, name string) (present, isDir bool, err error) {
	dirfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, false, refuse("cannot open parent directory")
	}
	defer unix.Close(dirfd)
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, false, nil
		}
		return false, false, refuse("cannot inspect target")
	}
	return true, st.Mode&unix.S_IFMT == unix.S_IFDIR, nil
}

// LoadKeyFile opens the installed capability key with
// O_NOFOLLOW|O_CLOEXEC and reads it strictly through that one descriptor.
// fstat on the open descriptor must prove a regular root-owned file whose
// permissions are exactly owner-only — mode 0400 per the packaging docs,
// with no group, world, or special bit — the read is bounded to one byte
// past the key size so a grown or swapped file is refused, and the path is
// never reopened, so there is no stat/read TOCTOU window. It must run
// before the privilege drop: the key file is root-only.
func LoadKeyFile(path string) (Key, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, refuse("capability key is unavailable")
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, refuse("capability key is unavailable")
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, refuse("capability key is not a regular file")
	}
	if perm := st.Mode & 0o777; perm&0o077 != 0 || perm&0o400 == 0 || st.Mode&0o7000 != 0 {
		return nil, refuse("capability key permissions are too broad")
	}
	if st.Uid != 0 {
		return nil, refuse("capability key is not root-owned")
	}
	buf := make([]byte, 33)
	total := 0
	for total < len(buf) {
		n, err := unix.Read(fd, buf[total:])
		if n > 0 {
			total += n
			continue
		}
		if err == nil {
			break
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return nil, refuse("capability key is unreadable")
	}
	if total > 32 {
		return nil, refuse("capability key is oversized")
	}
	return LoadKey(buf[:total])
}

// ValidatePaths performs the read-only, no-follow filesystem validation of
// the bound roots: the owner boundary and the registration root must be
// real, non-symlink directories and .worktrees beneath the boundary must
// be a real directory. For create, a missing target is the fresh case; an
// existing target is admitted only as a real directory — never a symlink
// or other non-directory — so Execute can decide the identical retry.
func (c *Capability) ValidatePaths() error {
	for _, dir := range []string{c.OwnerBoundary, c.RegistrationRoot} {
		exists, err := openNoFollow(dir)
		if err != nil {
			return err
		}
		if !exists {
			return refuse("bound root does not exist")
		}
	}
	exists, err := openNoFollow(filepath.Join(c.OwnerBoundary, ".worktrees"))
	if err != nil {
		return err
	}
	if !exists {
		return refuse("worktrees root does not exist")
	}
	if c.Verb == VerbCreate {
		present, isDir, err := statChildNoFollow(filepath.Dir(c.Target), filepath.Base(c.Target))
		if err != nil {
			return err
		}
		if present && !isDir {
			return refuse("worktree target exists and is not a real directory")
		}
		// A present real directory is admitted onward: Execute returns
		// changed:false only when the exact target is already registered
		// (the identical create retry) and refuses otherwise. A symlinked
		// target never reaches Execute.
	}
	return nil
}

// RegistrationRootReachable reports whether the bound registration root is
// visible in the current mount namespace.
func RegistrationRootReachable(root string) bool {
	var st unix.Stat_t
	return unix.Stat(root, &st) == nil
}
