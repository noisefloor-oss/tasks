//go:build !unix

package store

import (
	"os"
	"os/exec"
	"time"
)

// Non-Unix builds retain CommandContext's direct-child cancellation and
// still bound pipe cleanup. The confined broker is packaged only on Unix.
func configureBrokerProcess(cmd *exec.Cmd) { cmd.WaitDelay = time.Second }

func reapBrokerProcessGroup(*exec.Cmd) {}

func openBrokerCapabilityFile() (*os.File, error) { return nil, errWorktreeConfined }

func brokerCapabilityStdin(*os.File) *os.File { return nil }
