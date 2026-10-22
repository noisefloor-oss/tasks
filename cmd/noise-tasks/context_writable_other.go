//go:build !linux && !darwin

package main

func directoryWritable(string) bool {
	return false
}
