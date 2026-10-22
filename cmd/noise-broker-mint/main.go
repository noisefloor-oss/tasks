// Command noise-broker-mint mints §9.5 broker capabilities for the trusted
// host (Deck). It is deliberately not part of the setuid broker: the broker
// accepts exactly create|remove|prune-registration. The minting tool must
// run as a principal allowed to read the installed capability key and never
// inside the sandbox.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/noisefloor-oss/tasks/internal/broker"
	"github.com/noisefloor-oss/tasks/internal/libexec"
)

func main() {
	fs := flag.NewFlagSet("noise-broker-mint", flag.ExitOnError)
	verb := fs.String("verb", "", "bound verb: create|remove|prune-registration")
	family := fs.String("family", "", "repository family id")
	saga := fs.String("saga", "", "saga fence this capability authorizes")
	registration := fs.String("registration-root", "", "absolute common-dir worktrees directory")
	boundary := fs.String("owner-boundary", "", "absolute owner boundary directory")
	target := fs.String("target", "", "absolute sandbox working-tree target")
	branch := fs.String("branch", "", "branch tasks/<tag>/<id>")
	baseCommit := fs.String("base-commit", "", "immutable saga-recorded base commit")
	force := fs.Bool("force", false, "bind forced removal (remove only)")
	uid := fs.Int("uid", os.Getuid(), "invoking uid to bind")
	gid := fs.Int("gid", os.Getgid(), "invoking gid to bind")
	ttl := fs.Duration("ttl", 10*time.Minute, "capability lifetime")
	keyPath := fs.String("key", "", "capability key path (default: beside the libexec root)")
	fs.Parse(os.Args[1:])

	cap := broker.Capability{
		Verb:             *verb,
		FamilyID:         *family,
		SagaFence:        *saga,
		RegistrationRoot: *registration,
		OwnerBoundary:    *boundary,
		Target:           *target,
		Branch:           *branch,
		BaseCommit:       *baseCommit,
		Force:            *force,
		UID:              *uid,
		GID:              *gid,
		Expires:          time.Now().Add(*ttl).Unix(),
	}
	if err := cap.ValidateBindings(); err != nil {
		fmt.Fprintf(os.Stderr, "noise-broker-mint: %s\n", err)
		os.Exit(2)
	}
	path := *keyPath
	if path == "" {
		path = filepath.Join(filepath.Dir(libexec.Root), "noise-worktree-broker.key")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "noise-broker-mint: cannot read capability key\n")
		os.Exit(1)
	}
	key, err := broker.LoadKey(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "noise-broker-mint: %s\n", err)
		os.Exit(1)
	}
	doc, err := broker.Mint(key, cap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "noise-broker-mint: %s\n", err)
		os.Exit(1)
	}
	os.Stdout.Write(doc)
}
