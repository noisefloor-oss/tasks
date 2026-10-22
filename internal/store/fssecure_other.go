//go:build !unix

package store

import (
	"errors"
	"os"
)

// fileIdentity approximates file identity off unix with size and modtime;
// publication revalidation still fails closed on any observed change.
func fileIdentity(info os.FileInfo) [2]uint64 {
	return [2]uint64{uint64(info.Size()), uint64(info.ModTime().UnixNano())}
}

// Off unix platforms the race-safe O_NOFOLLOW/openat opens are unavailable;
// the §8.5 bundle and legacy readers fail closed instead of falling back to
// Lstat-then-open, which a pathname swap could subvert.

var errNotRegular = errors.New("not a regular file")

func openDirNoFollow(dirPath string) (*os.File, error) {
	return nil, errors.New("race-safe directory open is not supported on this platform: " + dirPath)
}

func openRegularNoFollow(path string) (*os.File, os.FileInfo, error) {
	return nil, nil, errors.New("race-safe file open is not supported on this platform: " + path)
}

func openMemberNoFollow(dir *os.File, name string) (*os.File, os.FileInfo, error) {
	return nil, nil, errors.New("race-safe member open is not supported on this platform: " + name)
}
