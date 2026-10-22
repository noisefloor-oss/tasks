//go:build !unix

package libexec

import "errors"

// Exec is unreachable after the unsupported-platform gate.
func Exec(string, []string, []string) error {
	return errors.New("exec is unsupported on this platform")
}
