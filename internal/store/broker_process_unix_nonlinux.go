//go:build unix && !linux

package store

import "os"

func brokerCapabilityStdin(*os.File) *os.File { return nil }
