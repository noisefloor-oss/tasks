//go:build linux || darwin

package main

import "syscall"

func directoryWritable(path string) bool {
	const writeAndSearch = 3
	return syscall.Access(path, writeAndSearch) == nil
}
