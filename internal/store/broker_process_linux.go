//go:build linux

package store

import (
	"fmt"
	"os"
	"strings"
)

// brokerCapabilityStdin forwards stdin only for the documented Linux fallback
// where fd 3 is a mount namespace descriptor and the capability document is
// supplied on stdin. Ordinary fd-3 capability documents keep stdin closed.
func brokerCapabilityStdin(capability *os.File) *os.File {
	target, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", capability.Fd()))
	if err != nil {
		return nil
	}
	if strings.HasPrefix(target, "mnt:[") || strings.Contains(target, "/ns/mnt") {
		return os.Stdin
	}
	return nil
}
