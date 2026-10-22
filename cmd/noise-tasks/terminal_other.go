//go:build !linux && !darwin

package main

import "os"

// Unsupported platforms conservatively disable prompting. A false negative
// is a clear usage error; a false positive could hang unattended automation.
func fileIsTerminal(_ *os.File) bool { return false }
