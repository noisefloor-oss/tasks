// Command noise-worktree-broker is the §9.5 confined worktree-registration
// broker. It accepts exactly create|remove|prune-registration, requires an
// authenticated host capability on inherited fd 3, drops permanently to the
// invoking uid/gid before any Git or target effect, and reports only
// bounded success or a sanitized refusal.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/broker"
	"github.com/noisefloor-oss/tasks/internal/libexec"
)

// capabilityFd is the single authenticated inherited channel (§9.5): the
// leaf never inherits it, and the broker consumes and closes it before any
// Git or target effect, whichever form it takes.
const capabilityFd = 3

// maxDoc bounds the capability document read from fd 3 or stdin.
const maxDoc = 65 * 1024

// testKey is the sole test injection point for the capability key. It
// exists only so package-internal, in-process tests can exercise the
// broker without installing a root-owned key at the fixed installed path;
// the production binary never assigns it, no argv, environment, or file
// can set it, and any spawned process leaves it nil so the real key
// loading path always runs.
var testKey broker.Key

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	verb, err := parseArgs(args)
	if err != nil {
		return usage(stderr, broker.Sanitize(err.Error()))
	}

	capBytes, err := readCapability(stdin)
	if err != nil {
		return refuse(stderr, err)
	}
	var key broker.Key
	if testKey != nil {
		key = testKey
	} else {
		key, err = broker.LoadKeyFile(installedKeyPath())
		if err != nil {
			return refuse(stderr, err)
		}
	}
	cap, err := broker.Parse(key, capBytes, time.Now().Unix())
	if err != nil {
		return refuse(stderr, err)
	}
	if cap.Verb != verb {
		return refuse(stderr, errors.New("capability is bound to another verb"))
	}
	if cap.UID != os.Getuid() || cap.GID != os.Getgid() {
		return refuse(stderr, errors.New("capability is bound to another identity"))
	}
	if err := cap.ValidateBindings(); err != nil {
		return refuse(stderr, err)
	}

	// Namespace re-entry immediately re-executes this binary. The fresh Go
	// runtime and every later Git child therefore inherit one mount namespace;
	// no runtime thread is left with the old namespace's filesystem context.
	if !broker.RegistrationRootReachable(cap.RegistrationRoot) {
		if os.Geteuid() == 0 {
			if broker.AlreadyInPID1MountNamespace() {
				return refuse(stderr, errors.New("registration root is unreachable after namespace re-entry"))
			}
			if err := broker.StageCapabilityForReexec(capabilityFd, capBytes); err != nil {
				return refuse(stderr, err)
			}
			if err := broker.EnterPID1MountNamespace(); err != nil {
				closeFd(capabilityFd)
				return refuse(stderr, err)
			}
		} else {
			return refuse(stderr, errors.New("registration root is unreachable"))
		}
	}

	if err := cap.ValidatePaths(); err != nil {
		return refuse(stderr, err)
	}
	if err := broker.DropPrivilege(cap.UID, cap.GID); err != nil {
		return refuse(stderr, err)
	}
	umask022()
	res, err := broker.Execute(cap)
	if err != nil {
		return refuse(stderr, err)
	}
	fmt.Fprintf(stdout, "{\"ok\":true,\"verb\":%q,\"changed\":%t}\n", res.Verb, res.Changed)
	return 0
}

// parseArgs accepts exactly one verb. There is no option surface at all:
// a setuid binary must not let a caller select anything — most importantly
// not the capability key path, which is fixed at install time.
func parseArgs(args []string) (verb string, err error) {
	rest := []string{}
	for i := range len(args) {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			return "", fmt.Errorf("unknown option: %s", a)
		}
		rest = append(rest, a)
	}
	if len(rest) != 1 {
		return "", errors.New("exactly one verb is required")
	}
	switch rest[0] {
	case broker.VerbCreate, broker.VerbRemove, broker.VerbPruneRegistration:
		return rest[0], nil
	default:
		return "", errors.New("verb is not supported")
	}
}

// readCapability reads and closes a complete, bounded document channel. For
// the namespace-fd channel, the document comes from stdin and is staged onto
// fd 3 for the immediate internal re-exec.
func readCapability(stdin io.Reader) ([]byte, error) {
	doc, ok, err := readFdBounded(capabilityFd, maxDoc)
	if err != nil {
		return nil, err
	}
	if ok {
		closeFd(capabilityFd)
		return doc, nil
	}
	if !broker.IsMountNamespaceFD(capabilityFd) {
		return nil, errors.New("no host capability on fd 3")
	}
	doc, err = readStdinBounded(stdin, maxDoc)
	if err != nil {
		return nil, err
	}
	if err := broker.TryEnterNamespaceFD(capabilityFd, doc); err != nil {
		return nil, err
	}
	return nil, errors.New("namespace re-entry did not re-execute")
}

// readFdBounded reads fd to EOF. It returns ok=true only for a complete,
// bounded document. ok=false with a nil error means the descriptor carried
// no document at all — it was not a readable byte source — and may instead
// be the host's mount-namespace descriptor; any other outcome is an
// explicit refusal.
func readFdBounded(fd, max int) (doc []byte, ok bool, err error) {
	buf := make([]byte, 4096)
	var acc []byte
	for {
		n, rerr := readFd(fd, buf)
		if n > 0 {
			acc = append(acc, buf[:n]...)
			if len(acc) > max {
				return nil, false, errors.New("capability document is oversized")
			}
			continue
		}
		if rerr == nil {
			// EOF: complete only when something preceded it.
			return acc, len(acc) > 0, nil
		}
		if len(acc) == 0 {
			// Unreadable from the first byte: not a document channel.
			return nil, false, nil
		}
		return nil, false, errors.New("capability document is unreadable")
	}
}

// readStdinBounded performs the complete, bounded read for the
// namespace-fd channel, whose document arrives on stdin.
func readStdinBounded(stdin io.Reader, max int) ([]byte, error) {
	doc, err := io.ReadAll(io.LimitReader(stdin, int64(max)+1))
	if err != nil || len(doc) == 0 {
		return nil, errors.New("capability document is unreadable")
	}
	if len(doc) > max {
		return nil, errors.New("capability document is oversized")
	}
	return doc, nil
}

func usage(stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "noise-worktree-broker: %s\n", msg)
	fmt.Fprintln(stderr, "usage: noise-worktree-broker <create|remove|prune-registration>")
	return 2
}

func refuse(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "noise-worktree-broker: refused: %s\n", broker.Sanitize(err.Error()))
	return 1
}

// installedKeyPath is the fixed capability key location: beside the
// build-time-recorded libexec root, outside the version-swapped plugin
// tree it points into. No argv, environment, or caller input selects it;
// only the release build's recorded root does.
func installedKeyPath() string {
	return filepath.Join(filepath.Dir(libexec.Root), "noise-worktree-broker.key")
}
