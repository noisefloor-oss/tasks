//go:build unix

// Privileged real-process broker suite (§9.6). Opt-in only: it exercises
// the actual installed-form binary as real processes, including the
// setuid privilege drop, so it must run as root on an isolated runner:
//
//	NOISE_BROKER_PRIVILEGED_TESTS=1 sudo -E go test ./cmd/noise-worktree-broker/ -run Privileged -v
//
// The binary is built with the libexec root pinned by release-style
// ldflags into the fixture's own tree, and the capability key is installed
// root-owned mode 0400 at the fixed path beside that root — the deployed
// shape, with no argv or environment key selection anywhere. Without the
// environment variable or root it skips safely.
package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noisefloor-oss/tasks/internal/broker"
)

const nobodyUID, nobodyGID = 65534, 65534

// privilegedReady gates the suite and returns the fixture directory.
func privilegedReady(t *testing.T) string {
	t.Helper()
	if os.Getenv("NOISE_BROKER_PRIVILEGED_TESTS") != "1" {
		t.Skip("opt-in only: set NOISE_BROKER_PRIVILEGED_TESTS=1 and run as root")
	}
	if os.Geteuid() != 0 {
		t.Skip("requires euid 0")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	return t.TempDir()
}

type privFixture struct {
	binary   string
	keyPath  string
	key      broker.Key
	boundary string
	base     string
}

// newPrivFixture builds the broker exactly as a release does — the libexec
// root recorded at build time — and installs the 32-byte capability key
// root-owned mode 0400 at the fixed derived path inside its own tree.
func newPrivFixture(t *testing.T) *privFixture {
	t.Helper()
	dir := privilegedReady(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	key, err := broker.LoadKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "libexec", "noise")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(filepath.Dir(root), "noise-worktree-broker.key")
	if err := os.WriteFile(keyPath, raw, 0o400); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "noise-worktree-broker")
	out, err := exec.Command("go", "build",
		"-ldflags", "-X github.com/noisefloor-oss/tasks/internal/libexec.Root="+root,
		"-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build broker: %v: %s", err, out)
	}
	if err := os.Chown(bin, 0, 0); err != nil {
		t.Fatalf("root-own broker: %v", err)
	}
	if err := os.Chmod(bin, 0o6755); err != nil {
		t.Fatalf("install setuid broker: %v", err)
	}
	boundary := filepath.Join(dir, "repo")
	if err := os.Mkdir(boundary, 0o755); err != nil {
		t.Fatal(err)
	}
	boundary, err = filepath.EvalSymlinks(boundary)
	if err != nil {
		t.Fatal(err)
	}
	privGit(t, boundary, "init", "-b", "main", ".")
	privGit(t, boundary, "config", "user.email", "t@example.com")
	privGit(t, boundary, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(boundary, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	privGit(t, boundary, "add", "f.txt")
	privGit(t, boundary, "commit", "-m", "init")
	if err := os.Mkdir(filepath.Join(boundary, ".worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(boundary, ".git", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(privGit(t, boundary, "rev-parse", "HEAD"))
	return &privFixture{binary: bin, keyPath: keyPath, key: key, boundary: boundary, base: base}
}

func privGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	} else {
		return string(out)
	}
	return ""
}

func (fx *privFixture) capFor(verb string, uid, gid int) broker.Capability {
	return broker.Capability{
		Verb:             verb,
		FamilyID:         "family-1",
		SagaFence:        "saga-9",
		RegistrationRoot: filepath.Join(fx.boundary, ".git", "worktrees"),
		OwnerBoundary:    fx.boundary,
		Target:           filepath.Join(fx.boundary, ".worktrees", "main-42"),
		Branch:           "tasks/main/42",
		BaseCommit:       fx.base,
		UID:              uid,
		GID:              gid,
		Expires:          time.Now().Add(10 * time.Minute).Unix(),
	}
}

// chownFamily hands the family tree to the bound non-root identity so the
// dropped broker can act on it.
func (fx *privFixture) chownFamily(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("chown", "-R",
		strconv.Itoa(nobodyUID)+":"+strconv.Itoa(nobodyGID), fx.boundary).CombinedOutput(); err != nil {
		t.Fatalf("chown family: %v: %s", err, out)
	}
}

// spawn runs the real broker process with doc (if non-nil) inherited on
// fd 3 — exactly the host's spawn shape — and a hostile environment a leaf
// might have left behind.
func spawn(t *testing.T, binary string, args []string, doc []byte, env []string) (rc int, out, errOut string) {
	return spawnAs(t, binary, args, doc, env, -1, -1)
}

// spawnAs executes the root-owned mode-6755 broker with the selected real
// caller identity. On a setuid-capable isolated runner the kernel supplies
// euid 0 while Getuid/Getgid remain uid/gid, exactly matching deployment.
func spawnAs(t *testing.T, binary string, args []string, doc []byte, env []string, uid, gid int) (rc int, out, errOut string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(binary, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = env
	if uid >= 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: uint32(uid), Gid: uint32(gid), NoSetGroups: true,
		}}
	}
	if doc != nil {
		f, err := os.CreateTemp("", "cap")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())
		f.Write(doc)
		f.Close()
		reopened, err := os.Open(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		cmd.ExtraFiles = []*os.File{reopened}
	}
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), stdout.String(), stderr.String()
		}
		t.Fatalf("spawn broker: %v", err)
	}
	return 0, stdout.String(), stderr.String()
}

var hostileEnv = []string{
	"PATH=/usr/bin:/bin",
	"GIT_DIR=/nonexistent/attacker",
	"GIT_WORK_TREE=/nonexistent/attacker",
	"GIT_CONFIG_GLOBAL=/nonexistent/attacker",
	"GIT_INDEX_FILE=/nonexistent/attacker",
	"HOME=/nonexistent/attacker",
}

func TestPrivilegedLeafDeniedWithoutCapability(t *testing.T) {
	fx := newPrivFixture(t)
	rc, _, errOut := spawnAs(t, fx.binary, []string{broker.VerbCreate}, nil, hostileEnv, nobodyUID, nobodyGID)
	if rc != 1 || !strings.Contains(errOut, "no host capability on fd 3") {
		t.Fatalf("leaf invocation: rc=%d err=%q", rc, errOut)
	}
}

// TestPrivilegedUnknownKeyOptionIsUsageError proves the removed production
// key override is a usage error on the real binary: no argv selects the
// capability key.
func TestPrivilegedUnknownKeyOptionIsUsageError(t *testing.T) {
	fx := newPrivFixture(t)
	rc, _, errOut := spawn(t, fx.binary, []string{broker.VerbCreate, "--key", "/nonexistent/attacker.key"}, nil, hostileEnv)
	if rc != 2 || !strings.Contains(errOut, "unknown option") {
		t.Fatalf("key argv: rc=%d err=%q", rc, errOut)
	}
}

// TestPrivilegedAttackerMintedCapabilityRefused proves a document minted
// with an attacker-controlled key fails authentication against the
// installed root-owned key.
func TestPrivilegedAttackerMintedCapabilityRefused(t *testing.T) {
	fx := newPrivFixture(t)
	attackerRaw := make([]byte, 32)
	rand.Read(attackerRaw)
	attackerKey, err := broker.LoadKey(attackerRaw)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := broker.Mint(attackerKey, fx.capFor(broker.VerbCreate, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	rc, _, errOut := spawn(t, fx.binary, []string{broker.VerbCreate}, doc, hostileEnv)
	if rc != 1 || !strings.Contains(errOut, "authentication failed") {
		t.Fatalf("attacker-key capability: rc=%d err=%q", rc, errOut)
	}
}

func TestPrivilegedTamperedCapabilityRefused(t *testing.T) {
	fx := newPrivFixture(t)
	doc, _ := broker.Mint(fx.key, fx.capFor(broker.VerbCreate, 0, 0))
	s := string(doc)
	i := strings.LastIndexByte(s[:len(s)-1], '\n')
	doc = []byte(s[:i+1] + "1111111111111111111111111111111111111111111111111111111111111111\n")
	rc, _, errOut := spawn(t, fx.binary, []string{broker.VerbCreate}, doc, hostileEnv)
	if rc != 1 || !strings.Contains(errOut, "authentication failed") {
		t.Fatalf("tampered: rc=%d err=%q", rc, errOut)
	}
}

func TestPrivilegedExpiredCapabilityRefused(t *testing.T) {
	fx := newPrivFixture(t)
	cap := fx.capFor(broker.VerbCreate, 0, 0)
	cap.Expires = time.Now().Add(-time.Minute).Unix()
	doc, _ := broker.Mint(fx.key, cap)
	rc, _, errOut := spawn(t, fx.binary, []string{broker.VerbCreate}, doc, hostileEnv)
	if rc != 1 || !strings.Contains(errOut, "expired") {
		t.Fatalf("expired: rc=%d err=%q", rc, errOut)
	}
}

// TestPrivilegedSymlinkedKeyRefused proves the key open is O_NOFOLLOW
// against the real fixed path: a symlink in place of the installed key —
// even one pointing at the valid key — is refused.
func TestPrivilegedSymlinkedKeyRefused(t *testing.T) {
	fx := newPrivFixture(t)
	real := filepath.Join(t.TempDir(), "real.key")
	if err := os.Rename(fx.keyPath, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, fx.keyPath); err != nil {
		t.Skip("symlink unavailable")
	}
	doc, _ := broker.Mint(fx.key, fx.capFor(broker.VerbCreate, 0, 0))
	rc, _, errOut := spawn(t, fx.binary, []string{broker.VerbCreate}, doc, hostileEnv)
	if rc != 1 || !strings.Contains(errOut, "capability key") {
		t.Fatalf("symlinked key: rc=%d err=%q", rc, errOut)
	}
}

// TestPrivilegedBroadKeyPermissionsRefused proves any group or world bit
// on the installed key refuses.
func TestPrivilegedBroadKeyPermissionsRefused(t *testing.T) {
	fx := newPrivFixture(t)
	if err := os.Chmod(fx.keyPath, 0o640); err != nil {
		t.Fatal(err)
	}
	doc, _ := broker.Mint(fx.key, fx.capFor(broker.VerbCreate, 0, 0))
	rc, _, errOut := spawn(t, fx.binary, []string{broker.VerbCreate}, doc, hostileEnv)
	if rc != 1 || !strings.Contains(errOut, "too broad") {
		t.Fatalf("broad key permissions: rc=%d err=%q", rc, errOut)
	}
}

// TestPrivilegedNonRootKeyOwnerRefused proves a key file not owned by root
// refuses, so an attacker cannot pre-install their own key at the fixed
// path.
func TestPrivilegedNonRootKeyOwnerRefused(t *testing.T) {
	fx := newPrivFixture(t)
	if err := os.Chown(fx.keyPath, nobodyUID, nobodyGID); err != nil {
		t.Skipf("cannot re-own key fixture: %v", err)
	}
	doc, _ := broker.Mint(fx.key, fx.capFor(broker.VerbCreate, 0, 0))
	rc, _, errOut := spawn(t, fx.binary, []string{broker.VerbCreate}, doc, hostileEnv)
	if rc != 1 || !strings.Contains(errOut, "root-owned") {
		t.Fatalf("non-root key owner: rc=%d err=%q", rc, errOut)
	}
}

func TestPrivilegedCreateDropAndRetryIdempotency(t *testing.T) {
	fx := newPrivFixture(t)
	// The caller's real identity is nobody; the root-owned mode-6755 binary
	// enters with euid root, verifies the capability, then irreversibly drops
	// back to the capability-bound identity before touching the family tree.
	fx.chownFamily(t)
	doc, _ := broker.Mint(fx.key, fx.capFor(broker.VerbCreate, nobodyUID, nobodyGID))
	rc, out, errOut := spawnAs(t, fx.binary, []string{broker.VerbCreate}, doc, hostileEnv, nobodyUID, nobodyGID)
	if rc != 0 {
		t.Fatalf("create: rc=%d out=%q err=%q", rc, out, errOut)
	}
	if !strings.Contains(out, `"changed":true`) {
		t.Fatalf("create output: %q", out)
	}
	// Privilege drop: every path the broker created is owned by the bound
	// identity, not root.
	for _, p := range []string{
		filepath.Join(fx.boundary, ".worktrees", "main-42"),
		filepath.Join(fx.boundary, ".worktrees", "main-42", "f.txt"),
	} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("created path missing: %v", err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != nobodyUID || int(st.Gid) != nobodyGID {
			t.Fatalf("path %s not owned by the dropped identity: %+v", p, fi.Sys())
		}
	}
	// Identical retry through a fresh process is a no-op success.
	rc, out, errOut = spawnAs(t, fx.binary, []string{broker.VerbCreate}, doc, hostileEnv, nobodyUID, nobodyGID)
	if rc != 0 || !strings.Contains(out, `"changed":false`) {
		t.Fatalf("retry: rc=%d out=%q err=%q", rc, out, errOut)
	}
	// Hostile environment proves the Git child runs with the fixed argv and
	// stripped environment: the bogus GIT_* values above were ignored.
}

// TestPrivilegedUnregisteredExistingTargetRefused proves the create-replay
// repair through the real CLI: an existing real directory at the exact
// target is a no-op success only when registered, and a refusal otherwise.
func TestPrivilegedUnregisteredExistingTargetRefused(t *testing.T) {
	fx := newPrivFixture(t)
	fx.chownFamily(t)
	if err := os.Mkdir(fx.capFor(broker.VerbCreate, nobodyUID, nobodyGID).Target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(fx.capFor(broker.VerbCreate, nobodyUID, nobodyGID).Target, nobodyUID, nobodyGID); err != nil {
		t.Fatal(err)
	}
	doc, _ := broker.Mint(fx.key, fx.capFor(broker.VerbCreate, nobodyUID, nobodyGID))
	rc, _, errOut := spawnAs(t, fx.binary, []string{broker.VerbCreate}, doc, hostileEnv, nobodyUID, nobodyGID)
	if rc != 1 || !strings.Contains(errOut, "not a registered worktree") {
		t.Fatalf("unregistered existing target: rc=%d err=%q", rc, errOut)
	}
}

func TestPrivilegedRemoveDropAndRetry(t *testing.T) {
	fx := newPrivFixture(t)
	fx.chownFamily(t)
	doc, _ := broker.Mint(fx.key, fx.capFor(broker.VerbRemove, nobodyUID, nobodyGID))
	rc, out, _ := spawnAs(t, fx.binary, []string{broker.VerbRemove}, doc, hostileEnv, nobodyUID, nobodyGID)
	if rc != 0 || !strings.Contains(out, `"changed":false`) {
		t.Fatalf("remove of absent registration: rc=%d out=%q", rc, out)
	}
	create, _ := broker.Mint(fx.key, fx.capFor(broker.VerbCreate, nobodyUID, nobodyGID))
	if rc, _, errOut := spawnAs(t, fx.binary, []string{broker.VerbCreate}, create, hostileEnv, nobodyUID, nobodyGID); rc != 0 {
		t.Fatalf("create: %q", errOut)
	}
	rc, out, _ = spawnAs(t, fx.binary, []string{broker.VerbRemove}, doc, hostileEnv, nobodyUID, nobodyGID)
	if rc != 0 || !strings.Contains(out, `"changed":true`) {
		t.Fatalf("remove: rc=%d out=%q", rc, out)
	}
	rc, out, _ = spawnAs(t, fx.binary, []string{broker.VerbRemove}, doc, hostileEnv, nobodyUID, nobodyGID)
	if rc != 0 || !strings.Contains(out, `"changed":false`) {
		t.Fatalf("remove retry: rc=%d out=%q", rc, out)
	}
}

func TestPrivilegedRefusesForeignRegistrationRoot(t *testing.T) {
	fx := newPrivFixture(t)
	cap := fx.capFor(broker.VerbCreate, nobodyUID, nobodyGID)
	cap.RegistrationRoot = filepath.Join(fx.boundary, "elsewhere")
	if err := os.Mkdir(cap.RegistrationRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fx.chownFamily(t)
	doc, err := broker.Mint(fx.key, cap)
	if err != nil {
		t.Fatal(err)
	}
	rc, _, errOut := spawnAs(t, fx.binary, []string{broker.VerbCreate}, doc, hostileEnv, nobodyUID, nobodyGID)
	if rc != 1 || !strings.Contains(errOut, "registration root") {
		t.Fatalf("foreign registration root: rc=%d err=%q", rc, errOut)
	}
}

func TestPrivilegedInstalledBrokerShape(t *testing.T) {
	fx := newPrivFixture(t)
	info, err := os.Stat(fx.binary)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || st.Gid != 0 || info.Mode().Perm() != 0o755 || info.Mode()&os.ModeSetuid == 0 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("broker installation is not root:root mode 6755: mode=%v stat=%+v", info.Mode(), info.Sys())
	}
}

func TestPrivilegedPackagingInstallsTrustedBroker(t *testing.T) {
	_ = privilegedReady(t)
	stageBase := "/var/tmp"
	if runtime.GOOS == "darwin" {
		stageBase = "/private/var/tmp"
	}
	stage, err := os.MkdirTemp(stageBase, "noise-package-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stage) })
	if err := os.Chmod(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "scripts/package.sh")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"DESTDIR="+stage,
		"PREFIX=/usr",
		"VERSION=privileged-test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package trusted broker: %v: %s", err, out)
	}
	brokerPath := filepath.Join(stage, "usr", "libexec", "noise-worktree-broker")
	info, err := os.Stat(brokerPath)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || st.Gid != 0 || info.Mode().Perm() != 0o755 || info.Mode()&os.ModeSetuid == 0 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("packaged broker is not root:root mode 6755: mode=%v stat=%+v", info.Mode(), info.Sys())
	}
}

func TestPackagingRefusesUnprivilegedBrokerInstall(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged packaging refusal requires a non-root test process")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "scripts/package.sh")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "DESTDIR="+t.TempDir(), "PREFIX=/usr")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "root is required") {
		t.Fatalf("unprivileged package result: err=%v output=%q", err, out)
	}
}
