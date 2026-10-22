package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/noisefloor-oss/tasks/internal/libexec"
	"github.com/noisefloor-oss/tasks/internal/setup"
)

type repeated []string

func (r *repeated) String() string     { return fmt.Sprint([]string(*r)) }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func main() {
	fs := flag.NewFlagSet("noise-setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var stage string
	var setupUID, setupGID, runtimeUID, runtimeGID repeated
	var homebrew bool
	fs.StringVar(&stage, "stage", "", "private staged plugin root")
	fs.Var(&setupUID, "setup-uid", "declared setup UID (repeatable)")
	fs.Var(&setupGID, "setup-gid", "declared writable setup GID (repeatable)")
	fs.Var(&runtimeUID, "runtime-uid", "runtime UID that must not modify the installation (repeatable)")
	fs.Var(&runtimeGID, "runtime-gid", "runtime GID that must not modify the installation (repeatable)")
	fs.BoolVar(&homebrew, "homebrew", false, "declare the selected root as Homebrew-managed")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "noise-setup: positional arguments are not accepted")
		os.Exit(2)
	}
	su, err := setup.ParseIDList(setupUID)
	if err != nil {
		declarationFailure("setup UID", err)
	}
	sg, err := setup.ParseIDList(setupGID)
	if err != nil {
		declarationFailure("setup GID", err)
	}
	ru, err := setup.ParseIDList(runtimeUID)
	if err != nil {
		declarationFailure("runtime UID", err)
	}
	rg, err := setup.ParseIDList(runtimeGID)
	if err != nil {
		declarationFailure("runtime GID", err)
	}
	if err := setup.Publish(setup.Config{Stage: stage, SetupUIDs: su, SetupGIDs: sg, RuntimeUIDs: ru, RuntimeGIDs: rg, Homebrew: homebrew}); err != nil {
		fail(err)
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, "noise-setup:", err); os.Exit(1) }

func declarationFailure(kind string, err error) {
	fail(fmt.Errorf("setup refused %s: invalid %s declaration: %v", libexec.Root, kind, err))
}
