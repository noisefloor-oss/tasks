//go:build linux || darwin

package main

import (
	"os"
	"testing"
)

func TestFileIsTerminalRejectsNullDevice(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if fileIsTerminal(file) {
		t.Fatal("null device was treated as an interactive terminal")
	}
}
