//go:build unix

package libexec

import "syscall"

// Exec replaces the current process with the validated plugin.
func Exec(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
