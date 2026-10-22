// Package buildinfo holds release metadata shared by every executable.
package buildinfo

// Version is replaced by release builds with -ldflags -X. Direct builds use
// the specification-defined development version.
var Version = "dev"
