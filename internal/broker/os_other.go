//go:build !unix

package broker

import "errors"

// errUnsupported keeps non-unix builds compiling; the broker is a unix
// confinement tool and refuses to act elsewhere.
var errUnsupported = errors.New("broker requires a unix platform")

func (c *Capability) ValidatePaths() error { return refuse("platform is unsupported") }

func RegistrationRootReachable(string) bool { return false }

func EnterPID1MountNamespace() error { return errUnsupported }

func AlreadyInPID1MountNamespace() bool { return false }

func IsMountNamespaceFD(int) bool { return false }

func StageCapabilityForReexec(int, []byte) error { return errUnsupported }

func TryEnterNamespaceFD(int, []byte) error { return errUnsupported }

func LoadKeyFile(string) (Key, error) { return nil, refuse("platform is unsupported") }

func DropPrivilege(uid, gid int) error { return refuse("platform is unsupported") }
