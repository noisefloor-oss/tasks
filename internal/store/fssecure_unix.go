//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// fileIdentity returns the (device, inode) identity of a regular file, used
// to revalidate that an overwrite publication target is still the exact file
// that was validated under the exclusive maintenance lock.
func fileIdentity(info os.FileInfo) [2]uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return [2]uint64{uint64(st.Dev), uint64(st.Ino)}
	}
	return [2]uint64{}
}

// Race-safe opens for the §8.5 bundle and legacy surfaces: every path is
// opened with O_NOFOLLOW and every identity, type, and mode check reads the
// open handle, never a re-stat of a pathname that could have been swapped
// between the check and the use.

// errNotRegular marks a member that is a symlink or otherwise not a regular
// file; the §8.5 contract refuses both identically.
var errNotRegular = errors.New("not a regular file")

func openNoFollowFlags() int { return unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW }

// openDirNoFollow opens dirPath as a directory handle, refusing a symlinked
// final component.
func openDirNoFollow(dirPath string) (*os.File, error) {
	fd, err := unix.Open(dirPath, openNoFollowFlags()|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), dirPath), nil
}

// openRegularNoFollow opens path refusing symlinks; regularity is validated
// from the returned open handle.
func openRegularNoFollow(path string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, openNoFollowFlags(), 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, nil, errNotRegular
		}
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, errNotRegular
	}
	return f, info, nil
}

// openMemberNoFollow opens name inside the already-open directory dir and
// revalidates from the open handle that it is a regular, non-symlink file.
func openMemberNoFollow(dir *os.File, name string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, openNoFollowFlags(), 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, nil, errNotRegular
		}
		return nil, nil, fmt.Errorf("open %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name))
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, errNotRegular
	}
	return f, info, nil
}
