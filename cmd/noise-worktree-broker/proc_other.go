//go:build !unix

package main

import "errors"

func readFd(fd int, buf []byte) (int, error) { return 0, errors.New("unsupported platform") }

func closeFd(fd int) {}

func umask022() {}
