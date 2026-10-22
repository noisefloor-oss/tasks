//go:build !unix

package main

import (
	"errors"
	"os"
)

// openTokenFile fails closed off unix: the §4 non-symlink and
// regular-file guarantees need O_NOFOLLOW and O_NONBLOCK opens, which
// are unavailable here, and an open that follows symlinks would accept
// a symlink to a regular file. Like the store's own non-unix secure
// opens, the capability is refused rather than degraded.
func openTokenFile(path string) (*os.File, error) {
	return nil, errors.New("fencing token files require a platform with race-safe opens")
}
