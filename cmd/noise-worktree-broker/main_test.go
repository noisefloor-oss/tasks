//go:build unix

package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noisefloor-oss/tasks/internal/broker"

	"golang.org/x/sys/unix"
)

// injectKey installs the in-process test key for one test and always
// restores the production nil state afterwards.
func injectKey(t *testing.T, key broker.Key) {
	t.Helper()
	previous := testKey
	testKey = key
	t.Cleanup(func() { testKey = previous })
}

// placeFd3 installs src on fd 3 for the duration of fn and restores
// whatever descriptor the test runtime kept there.
func placeFd3(t *testing.T, src int, fn func()) {
	t.Helper()
	saved, serr := unix.FcntlInt(capabilityFd, unix.F_DUPFD, 100)
	if serr == nil {
		defer unix.Close(saved)
	}
	if src != capabilityFd {
		if err := unix.Dup2(src, capabilityFd); err != nil {
			t.Skipf("cannot place capability on fd 3: %v", err)
		}
	}
	defer func() {
		unix.Close(capabilityFd)
		if serr == nil {
			unix.Dup2(saved, capabilityFd)
		}
	}()
	fn()
}

// withCapabilityFd places the capability document on fd 3 for the duration
// of fn, mimicking the host's inherited-fd spawn. The document is held on
// a high descriptor first, so dup2 to fd 3 can never self-collide with
// whatever descriptor the runtime handed out.
func withCapabilityFd(t *testing.T, doc []byte, fn func()) {
	t.Helper()
	f, err := os.CreateTemp("", "broker-cap")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Write(doc)
	f.Close()
	docf, err := os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer docf.Close()
	src, err := unix.FcntlInt(uintptr(docf.Fd()), unix.F_DUPFD_CLOEXEC, 100)
	if err != nil {
		t.Skipf("cannot stage capability descriptor: %v", err)
	}
	defer unix.Close(src)
	placeFd3(t, src, fn)
}

// withCapabilityPipe delivers the document through a blocking pipe in
// small chunks — the complete-read regression shape: a single-read
// implementation would see only a truncated document and fail
// authentication.
func withCapabilityPipe(t *testing.T, doc []byte, fn func()) {
	t.Helper()
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		t.Skipf("cannot create capability pipe: %v", err)
	}
	go func() {
		for i := 0; i < len(doc); i += 7 {
			j := min(i+7, len(doc))
			unix.Write(fds[1], doc[i:j])
			time.Sleep(time.Millisecond)
		}
		unix.Close(fds[1])
	}()
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	placeFd3(t, fds[0], fn)
}

// withoutCapabilityFd closes fd 3, the leaf's view of the world.
func withoutCapabilityFd(t *testing.T, fn func()) {
	t.Helper()
	saved, err := unix.FcntlInt(3, unix.F_DUPFD, 0)
	if err == nil {
		defer func() {
			unix.Dup2(saved, 3)
			unix.Close(saved)
		}()
	}
	unix.Close(3)
	fn()
}

type fixture struct {
	key      broker.Key
	boundary string
	cap      broker.Capability
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	raw := make([]byte, 32)
	rand.Read(raw)
	key, err := broker.LoadKey(raw)
	if err != nil {
		t.Fatal(err)
	}

	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main", "."},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = real
		cmd.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(real, "f.txt"), []byte("x"), 0o644)
	gitRunT(real, "add", "f.txt")
	gitRunT(real, "commit", "-m", "init")
	os.Mkdir(filepath.Join(real, ".worktrees"), 0o755)
	os.Mkdir(filepath.Join(real, ".git", "worktrees"), 0o755)

	return &fixture{
		key:      key,
		boundary: real,
		cap: broker.Capability{
			Verb:             broker.VerbCreate,
			FamilyID:         "family-1",
			SagaFence:        "saga-7",
			RegistrationRoot: filepath.Join(real, ".git", "worktrees"),
			OwnerBoundary:    real,
			Target:           filepath.Join(real, ".worktrees", "main-42"),
			Branch:           "tasks/main/42",
			BaseCommit:       strings.TrimSpace(gitRunT(real, "rev-parse", "HEAD")),
			UID:              os.Getuid(),
			GID:              os.Getgid(),
			Expires:          time.Now().Add(10 * time.Minute).Unix(),
		},
	}
}

func gitRunT(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C"}
	if out, err := cmd.CombinedOutput(); err != nil {
		panic(fmt.Sprintf("git %v: %v: %s", args, err, out))
	} else {
		return string(out)
	}
}

func (fx *fixture) mint(t *testing.T, mut ...func(*broker.Capability)) []byte {
	t.Helper()
	cap := fx.cap
	for _, m := range mut {
		m(&cap)
	}
	doc, err := broker.Mint(fx.key, cap)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestRunRefusesWithoutFd3(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	var out, errb bytes.Buffer
	var rc int
	withoutCapabilityFd(t, func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 1 {
		t.Fatalf("direct invocation exit %d, want 1", rc)
	}
	if !strings.Contains(errb.String(), "no host capability on fd 3") {
		t.Fatalf("unexpected refusal: %q", errb.String())
	}
}

func TestRunCreateAndRetry(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	var out, errb bytes.Buffer
	var rc int
	withCapabilityFd(t, fx.mint(t), func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 0 {
		t.Fatalf("create failed: %s", errb.String())
	}
	if !strings.Contains(out.String(), `"changed":true`) {
		t.Fatalf("unexpected output: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(fx.cap.Target, "f.txt")); err != nil {
		t.Fatalf("worktree content missing: %v", err)
	}
	// Exact identical retry — same capability, existing registered target —
	// is a bounded success without effect.
	out.Reset()
	withCapabilityFd(t, fx.mint(t), func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 0 || !strings.Contains(out.String(), `"changed":false`) {
		t.Fatalf("retry: rc=%d out=%q err=%q", rc, out.String(), errb.String())
	}
}

func TestRunRefusesExpiredCapability(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	doc := fx.mint(t, func(c *broker.Capability) { c.Expires = time.Now().Add(-time.Minute).Unix() })
	var out, errb bytes.Buffer
	var rc int
	withCapabilityFd(t, doc, func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 1 || !strings.Contains(errb.String(), "expired") {
		t.Fatalf("expired capability: rc=%d err=%q", rc, errb.String())
	}
}

func TestRunRefusesBindingMismatch(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	// Capability minted for create, invoked as remove.
	doc := fx.mint(t)
	var out, errb bytes.Buffer
	var rc int
	withCapabilityFd(t, doc, func() {
		rc = run_([]string{broker.VerbRemove}, strings.NewReader(""), &out, &errb)
	})
	if rc != 1 || !strings.Contains(errb.String(), "another verb") {
		t.Fatalf("verb mismatch: rc=%d err=%q", rc, errb.String())
	}
	// Capability bound to another identity.
	doc = fx.mint(t, func(c *broker.Capability) { c.UID++; c.GID++ })
	withCapabilityFd(t, doc, func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 1 || !strings.Contains(errb.String(), "another identity") {
		t.Fatalf("identity mismatch: rc=%d err=%q", rc, errb.String())
	}
}
func TestRunRefusesTamperedAuthenticator(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	doc := string(fx.mint(t))
	i := strings.LastIndexByte(doc[:len(doc)-1], '\n')
	doc = doc[:i+1] + "0000000000000000000000000000000000000000000000000000000000000000\n"
	var out, errb bytes.Buffer
	var rc int
	withCapabilityFd(t, []byte(doc), func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 1 || !strings.Contains(errb.String(), "authentication failed") {
		t.Fatalf("tampered capability: rc=%d err=%q", rc, errb.String())
	}
}

// TestRunRefusesAttackerMintedCapability proves the broker verifies
// against the installed key only: a document the attacker minted with
// their own 32-byte key — valid in shape, correctly signed for a key the
// attacker controls — still fails authentication. There is no way to make
// the broker load that key instead.
func TestRunRefusesAttackerMintedCapability(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	attackerRaw := make([]byte, 32)
	rand.Read(attackerRaw)
	attackerKey, err := broker.LoadKey(attackerRaw)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := broker.Mint(attackerKey, fx.cap)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	var rc int
	withCapabilityFd(t, doc, func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 1 || !strings.Contains(errb.String(), "authentication failed") {
		t.Fatalf("attacker-key capability: rc=%d err=%q", rc, errb.String())
	}
}

func TestRunUsageErrors(t *testing.T) {
	// The removed production key override is now an unknown option in
	// every spelling, as is any other option; non-option garbage stays a
	// plain usage error.
	cases := []struct {
		args        []string
		wantUnknown bool
	}{
		{args: []string{"destroy"}},
		{args: []string{broker.VerbCreate, "extra"}},
		{args: []string{broker.VerbCreate, "--key", "/nonexistent/attacker.key"}, wantUnknown: true},
		{args: []string{broker.VerbCreate, "--key=/nonexistent/attacker.key"}, wantUnknown: true},
		{args: []string{"--key", broker.VerbCreate}, wantUnknown: true},
		{args: []string{broker.VerbCreate, "-k"}, wantUnknown: true},
	}
	for _, tc := range cases {
		var out, errb bytes.Buffer
		rc := run_(tc.args, strings.NewReader(""), &out, &errb)
		if rc != 2 {
			t.Fatalf("argv %v exit %d, want usage error 2", tc.args, rc)
		}
		if tc.wantUnknown && !strings.Contains(errb.String(), "unknown option") {
			t.Fatalf("argv %v: unexpected diagnostics %q", tc.args, errb.String())
		}
	}
}

// TestRunClosesCapabilityFd proves the inherited capability descriptor is
// consumed: after a successful run it is closed, so no leaf Git exec can
// ever inherit it.
func TestRunClosesCapabilityFd(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	var out, errb bytes.Buffer
	withCapabilityFd(t, fx.mint(t), func() {
		rc := run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
		if rc != 0 {
			t.Fatalf("create failed: %s", errb.String())
		}
		if _, err := unix.FcntlInt(capabilityFd, unix.F_GETFD, 0); err == nil {
			t.Fatal("capability fd 3 survived the broker run")
		}
	})
}

// TestRunRefusesOversizedCapability proves the fd-3 read is bounded: a
// stream longer than the document bound is refused as oversized, not read
// once and parsed.
func TestRunRefusesOversizedCapability(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	doc := bytes.Repeat([]byte("A"), maxDoc+1)
	var out, errb bytes.Buffer
	var rc int
	withCapabilityFd(t, doc, func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 1 || !strings.Contains(errb.String(), "oversized") {
		t.Fatalf("oversized capability: rc=%d err=%q", rc, errb.String())
	}
}

// TestRunRefusesTruncatedCapability proves a partial document never
// parses: half a minted document is a refusal.
func TestRunRefusesTruncatedCapability(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	full := fx.mint(t)
	var out, errb bytes.Buffer
	var rc int
	withCapabilityFd(t, full[:len(full)/2], func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 1 || errb.Len() == 0 {
		t.Fatalf("truncated capability: rc=%d err=%q", rc, errb.String())
	}
}

// TestRunAcceptsChunkedCapability proves the fd-3 read is complete: a
// document delivered in many small pipe writes still arrives whole.
func TestRunAcceptsChunkedCapability(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	var out, errb bytes.Buffer
	var rc int
	withCapabilityPipe(t, fx.mint(t), func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 0 || !strings.Contains(out.String(), `"changed":true`) {
		t.Fatalf("chunked capability: rc=%d out=%q err=%q", rc, out.String(), errb.String())
	}
}

// TestRunRefusesUnregisteredExistingTarget proves an existing real
// directory reaches Execute — the identical-retry shape — but only the
// exact registered worktree is a no-op success.
func TestRunRefusesUnregisteredExistingTarget(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	if err := os.Mkdir(fx.cap.Target, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	var rc int
	withCapabilityFd(t, fx.mint(t), func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 1 || !strings.Contains(errb.String(), "not a registered worktree") {
		t.Fatalf("unregistered existing target: rc=%d err=%q", rc, errb.String())
	}
}

// TestRunRefusesSymlinkedTarget proves the no-follow rule survives the
// create-replay repair: a symlinked target never reaches Execute.
func TestRunRefusesSymlinkedTarget(t *testing.T) {
	fx := newFixture(t)
	injectKey(t, fx.key)
	if err := os.Symlink(fx.boundary, fx.cap.Target); err != nil {
		t.Skip("symlink unavailable")
	}
	var out, errb bytes.Buffer
	var rc int
	withCapabilityFd(t, fx.mint(t), func() {
		rc = run_([]string{broker.VerbCreate}, strings.NewReader(""), &out, &errb)
	})
	if rc != 1 || !strings.Contains(errb.String(), "not a real directory") {
		t.Fatalf("symlinked target: rc=%d err=%q", rc, errb.String())
	}
}

// run_ is the broker entry point under test (run is shadowed by helpers).
func run_(args []string, stdin *strings.Reader, stdout, stderr *bytes.Buffer) int {
	return run(args, stdin, stdout, stderr)
}

// TestRealProcessFixedKeyPath spawns the actual binary with the libexec
// root pinned by release-style ldflags into the test's own tree — the
// exact host spawn shape — and proves the capability key is read only
// from the fixed installed path beside it: no argv or environment can
// redirect key loading.
func TestRealProcessFixedKeyPath(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	fx := newFixture(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "libexec", "noise")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(filepath.Dir(root), "noise-worktree-broker.key")
	if err := os.WriteFile(keyPath, fx.key, 0o400); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "noise-worktree-broker")
	build := exec.Command("go", "build",
		"-ldflags", "-X github.com/noisefloor-oss/tasks/internal/libexec.Root="+root,
		"-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	docf, err := os.CreateTemp("", "cap")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(docf.Name())
	docf.Write(fx.mint(t))
	docf.Close()
	reopened, err := os.Open(docf.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, broker.VerbCreate)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = []string{"PATH=/usr/bin:/bin", "GIT_DIR=/nonexistent/attacker", "GIT_CONFIG_GLOBAL=/nonexistent"}
	cmd.ExtraFiles = []*os.File{reopened}
	err = cmd.Run()
	rc := 0
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		// Root owns the installed key exactly as deployed: the full
		// confined create runs at the current identity under the hostile
		// inherited environment.
		if rc != 0 || !strings.Contains(stdout.String(), `"changed":true`) {
			t.Fatalf("real-process create: rc=%d out=%q err=%q", rc, stdout.String(), stderr.String())
		}
		if _, err := os.Stat(filepath.Join(fx.boundary, ".worktrees", "main-42", "f.txt")); err != nil {
			t.Fatalf("worktree missing: %v", err)
		}
	} else {
		// Unprivileged: the attacker cannot install a root-owned key, so
		// even a perfectly minted document is refused at key loading from
		// the fixed path — never from any caller-chosen one.
		if rc != 1 || !strings.Contains(stderr.String(), "capability key") {
			t.Fatalf("unprivileged real-process create: rc=%d err=%q", rc, stderr.String())
		}
	}

	// The removed production override is a usage error on the real binary.
	stdout.Reset()
	stderr.Reset()
	cmd = exec.Command(bin, broker.VerbCreate, "--key", "/nonexistent/attacker.key")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	err = cmd.Run()
	rc = 0
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if rc != 2 {
		t.Fatalf("real-process --key argv exit %d, want usage error 2 (err=%q)", rc, stderr.String())
	}
}
