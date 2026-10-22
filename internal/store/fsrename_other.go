//go:build !linux && !darwin

package store

import "fmt"

// renameNoReplace fails closed off Linux: no supported atomic no-replace
// directory rename exists here, and a racy Lstat-then-Rename fallback could
// replace an artifact, so bundle publication is refused instead.
func renameNoReplace(oldPath, newPath string) error {
	return fmt.Errorf("atomic no-replace rename of %s to %s is not supported on this platform", oldPath, newPath)
}
