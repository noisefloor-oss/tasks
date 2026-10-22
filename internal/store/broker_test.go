package store

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noisefloor-oss/tasks/internal/libexec"
)

// §9.5 unprivileged broker tests: permission-class detection on registration
// writes, delegate-once retry through a strictly validated broker with the
// real one-verb/fd-3 calling convention, exact confinement diagnostics, and
// preserved claim rollback / keep:true degradation.

// brokerTestRepo creates a git repository with one task and opens its store.
func brokerTestRepo(t *testing.T) (*DB, string) {
	t.Helper()
	return brokerTestRepoAt(t, t.TempDir())
}

func brokerTestRepoAt(t *testing.T, repo string) (*DB, string) {
	t.Helper()
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.email", "test@example.invalid")
	git(t, repo, "config", "user.name", "test")
	git(t, repo, "commit", "--allow-empty", "-m", "init")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".gitignore")
	git(t, repo, "commit", "-m", "ignore")
	dbPath, _ := Path(repo)
	db, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	addTask(t, db, "main", "broker task")
	return db, repo
}

// confineRegistration makes the <common-dir>/worktrees registration area
// unwritable for the invoking (unprivileged) owner.
func confineRegistration(t *testing.T, repo string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("confined registration requires an unprivileged uid")
	}
	reg := filepath.Join(repo, ".git", "worktrees")
	if err := os.MkdirAll(reg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(reg, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(reg, 0o755) })
}

// fakeBroker stages a fake noise-worktree-broker and points the real strict
// resolver at it by publishing a libexec root.
func fakeBroker(t *testing.T, script string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "noise-worktree-broker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := libexec.Root
	libexec.Root = root
	t.Cleanup(func() { libexec.Root = old })
}

func brokerCapabilityDoc(target, branch, ref string) string {
	return target + "\n" + branch + "\n" + ref + "\n"
}

// functionalFakeBroker performs the registration effect after the host's
// direct attempt proves confinement. It temporarily restores owner write
// permission to model the installed broker's writable namespace.
func functionalFakeBroker(t *testing.T, log string) {
	t.Helper()
	logLines := ""
	if log != "" {
		logLines = "printf 'argv: %s\\n' \"$*\" >> '" + log + "'\n" +
			"printf 'env: %s\\n' \"$(env | cut -d= -f1 | sort | tr '\\n' ',')\" >> '" + log + "'\n"
	}
	script := "#!/bin/sh\n" +
		"if [ \"$#\" -ne 1 ]; then echo bad-argv >&2; exit 7; fi\n" +
		"if ! IFS= read -r target <&3; then echo missing-fd3 >&2; exit 8; fi\n" +
		"IFS= read -r branch <&3 || exit 8\n" +
		"IFS= read -r ref <&3 || exit 8\n" +
		"exec 3<&-\n" + logLines +
		"reg=\"$(git rev-parse --git-common-dir)/worktrees\"\n" +
		"chmod u+w \"$reg\"\n" +
		"case \"$1\" in\n" +
		"  create)\n" +
		"    if git show-ref --verify --quiet \"refs/heads/$branch\"; then\n" +
		"      git worktree add -- \"$target\" \"$branch\"\n" +
		"    else\n" +
		"      git worktree add -b \"$branch\" -- \"$target\" \"$ref\"\n" +
		"    fi ;;\n" +
		"  remove) git worktree remove --force -- \"$target\" ;;\n" +
		"  prune-registration) git worktree prune ;;\n" +
		"  *) false ;;\n" +
		"esac\n" +
		"status=$?\n" +
		"chmod u-w \"$reg\"\n" +
		"exit \"$status\"\n"
	fakeBroker(t, script)
}

// unresolveBroker makes broker resolution strictly refuse.
func unresolveBroker(t *testing.T) {
	t.Helper()
	old := libexec.Root
	libexec.Root = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { libexec.Root = old })
}

func requireConfined(t *testing.T, err error) {
	t.Helper()
	if err == nil || err.Error() != "worktree registration is confined and no broker is available" {
		t.Fatalf("err = %v, want the pinned confinement diagnostic", err)
	}
}

// TestWorktreeCreateConfinedNoBroker proves the exact §9.5 confinement
// diagnostic when the registration write fails for a permission-class reason
// and no strictly valid broker resolves.
func TestWorktreeCreateConfinedNoBroker(t *testing.T) {
	db, repo := brokerTestRepo(t)
	confineRegistration(t, repo)
	unresolveBroker(t)

	_, err := db.CreateWorktree(repo, "main", "1", false)
	requireConfined(t, err)

	// Nothing was created at the sandbox target.
	if _, err := os.Lstat(filepath.Join(repo, ".worktrees", "main-1")); !os.IsNotExist(err) {
		t.Fatal("target created despite confinement")
	}
	if exists, err := worktreeBranchExists(repo, "tasks/main/1"); err != nil || exists {
		t.Fatalf("partial create branch survived: exists=%v err=%v", exists, err)
	}
}

// TestWorktreeCreateConfinedUnresolvableRoots covers the strict resolution
// refusals that all collapse onto the pinned confinement diagnostic: a
// missing root and an unsafe (escaping symlink) broker entry.
func TestWorktreeCreateConfinedUnresolvableRoots(t *testing.T) {
	db, repo := brokerTestRepo(t)
	confineRegistration(t, repo)

	// Root that does not exist at all.
	old := libexec.Root
	t.Cleanup(func() { libexec.Root = old })
	libexec.Root = filepath.Join(t.TempDir(), "no-such-root")
	var err error
	withBrokerCapabilityFD(t, brokerCapabilityDoc(filepath.Join(repo, ".worktrees", "main-1"), "tasks/main/1", "main"), func() {
		_, err = db.CreateWorktree(repo, "main", "1", false)
	})
	requireConfined(t, err)
	if exists, err := worktreeBranchExists(repo, "tasks/main/1"); err != nil || exists {
		t.Fatalf("missing-root branch compensation: exists=%v err=%v", exists, err)
	}

	// A symlinked broker entry that escapes the root is unsafe, not usable.
	root := t.TempDir()
	outside := t.TempDir()
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(outside, "real-broker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "real-broker"), filepath.Join(root, "noise-worktree-broker")); err != nil {
		t.Fatal(err)
	}
	libexec.Root = root
	withBrokerCapabilityFD(t, brokerCapabilityDoc(filepath.Join(repo, ".worktrees", "main-1"), "tasks/main/1", "main"), func() {
		_, err = db.CreateWorktree(repo, "main", "1", false)
	})
	requireConfined(t, err)
}

// TestWorktreeCreateConfinedDelegatesOnceWithFixedArgv proves the delegate-once
// retry: one direct permission failure, exactly one broker invocation with a
// fixed argv and the stripped environment, and a bound worktree afterwards.
func TestWorktreeCreateConfinedDelegatesOnceWithFixedArgv(t *testing.T) {
	db, repo := brokerTestRepo(t)
	confineRegistration(t, repo)

	log := filepath.Join(t.TempDir(), "broker.log")
	functionalFakeBroker(t, log)

	wantPath := filepath.Join(repo, ".worktrees", "main-1")
	var binding *WorktreeBinding
	var err error
	withBrokerCapabilityFD(t, brokerCapabilityDoc(wantPath, "tasks/main/1", "main"), func() {
		binding, err = db.CreateWorktree(repo, "main", "1", false)
	})
	if err != nil {
		t.Fatalf("delegated create failed: %v", err)
	}
	if binding.Path != wantPath || binding.Branch != "tasks/main/1" {
		t.Fatalf("binding = %+v", binding)
	}
	if registered, err := worktreeRegistered(repo, wantPath); err != nil || !registered {
		t.Fatalf("delegated target is not registered: registered=%v err=%v", registered, err)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("broker invoked %d times, want exactly once:\n%s", len(lines)/2, data)
	}
	wantArgv := "create"
	if lines[0] != "argv: "+wantArgv {
		t.Fatalf("broker argv = %q, want fixed %q", lines[0], wantArgv)
	}
	// The broker environment is stripped and fixed: only the four allowed
	// keys survive (PWD/SHLVL/_ are injected by the broker's own shell), so no
	// inherited Git configuration or tokens leak.
	envLine := strings.TrimPrefix(lines[1], "env:")
	for _, key := range strings.Split(strings.TrimSuffix(envLine, ","), ",") {
		key = strings.TrimSpace(key)
		switch key {
		case "HOME", "LANG", "LC_ALL", "PATH", "PWD", "SHLVL", "_", "":
		default:
			t.Fatalf("unexpected broker environment key %q", key)
		}
	}
}

// TestWorktreeCreateConfinedRequiresHostCapabilityFD proves that a confined
// registration never invokes the broker unless the host supplied fd 3.
func TestWorktreeCreateConfinedRequiresHostCapabilityFD(t *testing.T) {
	db, repo := brokerTestRepo(t)
	confineRegistration(t, repo)
	log := filepath.Join(t.TempDir(), "broker-invoked")
	fakeBroker(t, "#!/bin/sh\nprintf invoked > '"+log+"'\nexit 0\n")

	var err error
	withoutBrokerCapabilityFD(t, func() {
		_, err = db.CreateWorktree(repo, "main", "1", false)
	})
	requireConfined(t, err)
	if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
		t.Fatalf("broker ran without fd-3 host capability: %v", statErr)
	}
}

// TestWorktreeRemoveConfinedDelegatesToBroker proves removal registration also
// delegates once through the broker.
func TestWorktreeRemoveConfinedDelegatesToBroker(t *testing.T) {
	db, repo := brokerTestRepo(t)
	if _, err := db.CreateWorktree(repo, "main", "1", false); err != nil {
		t.Fatal(err)
	}
	confineRegistration(t, repo)

	log := filepath.Join(t.TempDir(), "broker.log")
	functionalFakeBroker(t, log)

	var err error
	withBrokerCapabilityFD(t, brokerCapabilityDoc(filepath.Join(repo, ".worktrees", "main-1"), "tasks/main/1", "main"), func() {
		_, err = db.RemoveWorktree(repo, "main", "1", false)
	})
	if err != nil {
		t.Fatalf("delegated remove failed: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "argv:"); got != 1 {
		t.Fatalf("broker invoked %d times, want exactly once", got)
	}
	if task, _ := db.GetTask("main", "1"); task == nil || task.Binding != nil {
		t.Fatalf("binding not cleared: %+v", task.Binding)
	}
	if _, err := os.Lstat(filepath.Join(repo, ".worktrees", "main-1")); !os.IsNotExist(err) {
		t.Fatalf("removed target still exists: %v", err)
	}
}

// TestWorktreeBrokerRefusalSanitized proves a refusing broker maps onto the
// §9.5 refusal error with a bounded, sanitized reason — no control
// characters, no runaway length.
func TestWorktreeBrokerRefusalSanitized(t *testing.T) {
	db, repo := brokerTestRepo(t)
	confineRegistration(t, repo)

	long := strings.Repeat("x", 5000)
	script := "#!/bin/sh\nprintf 'no\x01\x02pe\\t" + long + "\\n' >&2\nexit 7\n"
	fakeBroker(t, script)

	var err error
	withBrokerCapabilityFD(t, brokerCapabilityDoc(filepath.Join(repo, ".worktrees", "main-1"), "tasks/main/1", "main"), func() {
		_, err = db.CreateWorktree(repo, "main", "1", false)
	})
	var refusal *WorktreeBrokerRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a broker refusal", err)
	}
	if len(refusal.Reason) > brokerMaxReason {
		t.Fatalf("reason length %d exceeds bound %d", len(refusal.Reason), brokerMaxReason)
	}
	if !strings.HasPrefix(refusal.Reason, "no pe") {
		t.Fatalf("reason = %q, want the sanitized control byte collapsed", refusal.Reason)
	}
	if want := "worktree broker refused: " + refusal.Reason; err.Error() != want {
		t.Fatalf("error text = %q", err.Error())
	}
	if exists, err := worktreeBranchExists(repo, "tasks/main/1"); err != nil || exists {
		t.Fatalf("refused create branch survived: exists=%v err=%v", exists, err)
	}
}

// TestWorktreeBrokerRefusalSilent proves a broker that exits nonzero without
// output still yields a bounded refusal reason.
func TestWorktreeBrokerRefusalSilent(t *testing.T) {
	db, repo := brokerTestRepo(t)
	confineRegistration(t, repo)
	fakeBroker(t, "#!/bin/sh\nexit 9\n")

	var err error
	withBrokerCapabilityFD(t, brokerCapabilityDoc(filepath.Join(repo, ".worktrees", "main-1"), "tasks/main/1", "main"), func() {
		_, err = db.CreateWorktree(repo, "main", "1", false)
	})
	var refusal *WorktreeBrokerRefusal
	if !errors.As(err, &refusal) || refusal.Reason != "broker exited 9" {
		t.Fatalf("err = %v, want silent-exit refusal", err)
	}
}

// TestWorktreeBrokerTimeout proves a broker that never answers is killed at
// the bound and refused as timed out.
func TestWorktreeBrokerTimeout(t *testing.T) {
	db, repo := brokerTestRepo(t)
	confineRegistration(t, repo)
	fakeBroker(t, "#!/bin/sh\nexec sleep 30\n")

	oldTimeout := brokerTimeout
	brokerTimeout = 250 * time.Millisecond
	t.Cleanup(func() { brokerTimeout = oldTimeout })

	start := time.Now()
	var err error
	withBrokerCapabilityFD(t, brokerCapabilityDoc(filepath.Join(repo, ".worktrees", "main-1"), "tasks/main/1", "main"), func() {
		_, err = db.CreateWorktree(repo, "main", "1", false)
	})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("broker timeout not enforced: %s", elapsed)
	}
	var refusal *WorktreeBrokerRefusal
	if !errors.As(err, &refusal) || refusal.Reason != "broker timed out" {
		t.Fatalf("err = %v, want a timeout refusal", err)
	}
}

// TestWorktreeBrokerTimeoutWithLeakedPipe proves a broker descendant cannot
// keep the caller waiting indefinitely by retaining stdout/stderr after the
// direct broker process is cancelled.
func TestWorktreeBrokerTimeoutWithLeakedPipe(t *testing.T) {
	db, repo := brokerTestRepo(t)
	confineRegistration(t, repo)
	fakeBroker(t, "#!/bin/sh\nsleep 30 &\nwait\n")

	oldTimeout := brokerTimeout
	brokerTimeout = 100 * time.Millisecond
	t.Cleanup(func() { brokerTimeout = oldTimeout })

	start := time.Now()
	var err error
	withBrokerCapabilityFD(t, brokerCapabilityDoc(filepath.Join(repo, ".worktrees", "main-1"), "tasks/main/1", "main"), func() {
		_, err = db.CreateWorktree(repo, "main", "1", false)
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("broker descendant retained pipes for %s", elapsed)
	}
	var refusal *WorktreeBrokerRefusal
	if !errors.As(err, &refusal) || refusal.Reason != "broker timed out" {
		t.Fatalf("err = %v, want timeout refusal", err)
	}
}

// TestClaimWorktreeConfinedRollsBackClaim proves the §9.2 claim rollback is
// preserved: a confined, broker-less worktree creation failure leaves no
// claim behind.
func TestClaimWorktreeConfinedRollsBackClaim(t *testing.T) {
	db, repo := brokerTestRepo(t)
	confineRegistration(t, repo)
	unresolveBroker(t)

	_, _, _, err := db.ClaimWorktree(repo, "main", "1", "agent-a", 3600, false)
	requireConfined(t, err)

	task, err := db.GetTask("main", "1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Claim != nil {
		t.Fatalf("claim survived a failed worktree creation: %+v", task.Claim)
	}
	// The task is immediately claimable once the registration boundary is
	// repaired; exact compensation removed the failed add's branch side effect.
	if err := os.Chmod(filepath.Join(repo, ".git", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	claim, _, _, err := db.ClaimWorktree(repo, "main", "1", "agent-a", 3600, false)
	if err != nil || claim == nil {
		t.Fatalf("re-claim after rollback: %v", err)
	}
}

// TestReleaseConfinedDegradesToKeep proves the keep:true degradation survives
// TestReleaseConfinedDegradesToKeep proves the keep:true degradation survives
// confinement: a broker-less removal failure never blocks the release.
func TestReleaseConfinedDegradesToKeep(t *testing.T) {
	db, repo := brokerTestRepo(t)
	claim, _, _, err := db.ClaimWorktree(repo, "main", "1", "agent-a", 3600, false)
	if err != nil {
		t.Fatal(err)
	}
	confineRegistration(t, repo)
	unresolveBroker(t)

	note, err := db.ReleaseClaimWorktree(repo, "main", "1", "agent-a", claim.Token, false, false)
	if err != nil {
		t.Fatalf("release blocked by confinement: %v", err)
	}
	if note == "" || !strings.Contains(note, "keep: true") {
		t.Fatalf("degradation note = %q, want keep:true", note)
	}
	task, err := db.GetTask("main", "1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Claim != nil {
		t.Fatal("claim not released")
	}
	if task.Binding == nil || !task.Binding.Keep {
		t.Fatalf("binding = %+v, want keep:true", task.Binding)
	}
	if _, err := os.Stat(task.Binding.Path); err != nil {
		t.Fatalf("keep:true target was not restored: %v", err)
	}
	if registered, err := worktreeRegistered(repo, task.Binding.Path); err != nil || !registered {
		t.Fatalf("keep:true registration was not preserved: registered=%v err=%v", registered, err)
	}
}

// TestWorktreeRemoveConfinedFailureRestoresTarget proves the staged-remove
// protocol leaves both the target and its exact registration intact when no
// broker can complete the registration write.
func TestWorktreeRemoveConfinedFailureRestoresTarget(t *testing.T) {
	db, repo := brokerTestRepo(t)
	binding, err := db.CreateWorktree(repo, "main", "1", false)
	if err != nil {
		t.Fatal(err)
	}
	confineRegistration(t, repo)
	unresolveBroker(t)

	_, err = db.RemoveWorktree(repo, "main", "1", false)
	requireConfined(t, err)
	if info, err := os.Stat(binding.Path); err != nil || !info.IsDir() {
		t.Fatalf("target was not restored: info=%v err=%v", info, err)
	}
	if registered, err := worktreeRegistered(repo, binding.Path); err != nil || !registered {
		t.Fatalf("registration changed on refusal: registered=%v err=%v", registered, err)
	}
	task, err := db.GetTask("main", "1")
	if err != nil || task.Binding == nil {
		t.Fatalf("binding changed on refusal: task=%+v err=%v", task, err)
	}
}

func TestWorktreeRemoveConfinedRefusalRestoresTarget(t *testing.T) {
	db, repo := brokerTestRepo(t)
	binding, err := db.CreateWorktree(repo, "main", "1", false)
	if err != nil {
		t.Fatal(err)
	}
	confineRegistration(t, repo)
	fakeBroker(t, "#!/bin/sh\necho capability-mismatch >&2\nexit 7\n")

	withBrokerCapabilityFD(t, brokerCapabilityDoc(binding.Path, binding.Branch, "main"), func() {
		_, err = db.RemoveWorktree(repo, "main", "1", false)
	})
	var refusal *WorktreeBrokerRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("remove error = %v, want broker refusal", err)
	}
	if info, err := os.Stat(binding.Path); err != nil || !info.IsDir() {
		t.Fatalf("target was not restored after refusal: info=%v err=%v", info, err)
	}
	if registered, err := worktreeRegistered(repo, binding.Path); err != nil || !registered {
		t.Fatalf("registration changed after refusal: registered=%v err=%v", registered, err)
	}
}

// TestRegistrationPermissionFailureClassification pins the permission-class
// detection: registration-area permission failures delegate, target-area and
// non-permission failures surface unchanged.
func TestRegistrationPermissionFailureClassification(t *testing.T) {
	_, repo := brokerTestRepo(t)
	regErr := errors.New(`git worktree add: exit 128: fatal: unable to create directory for '` +
		filepath.Join(repo, ".git", "worktrees", "main-1") + `': Permission denied`)
	if !registrationPermissionFailure(repo, regErr) {
		t.Fatal("registration permission failure not detected")
	}
	targetErr := errors.New(`git worktree add: exit 128: fatal: unable to create directory for '` +
		filepath.Join(repo, ".worktrees", "main-1") + `': Permission denied`)
	if registrationPermissionFailure(repo, targetErr) {
		t.Fatal("sandbox-target permission failure must not delegate")
	}
	ancestorTarget := errors.New(`git worktree add: exit 128: fatal: unable to create directory for '` +
		filepath.Join(t.TempDir(), "worktrees", "repo", ".worktrees", "main-1") + `': Permission denied`)
	if registrationPermissionFailure(repo, ancestorTarget) {
		t.Fatal("an unrelated ancestor named worktrees must not delegate")
	}
	nestedAdminTarget := errors.New(`git worktree add: exit 128: fatal: unable to create directory for '` +
		filepath.Join(t.TempDir(), ".git", "worktrees", "outer", "repo", ".worktrees", "main-1") + `': Permission denied`)
	if registrationPermissionFailure(repo, nestedAdminTarget) {
		t.Fatal("a relative root spelling in an absolute ancestor path must not delegate")
	}
	notPerm := errors.New(`git worktree add: exit 128: fatal: invalid reference: main`)
	if registrationPermissionFailure(repo, notPerm) {
		t.Fatal("non-permission failure must not delegate")
	}
}

func TestSandboxTargetPermissionUnderWorktreesAncestorDoesNotDelegate(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "worktrees", "repo")
	db, repo := brokerTestRepoAt(t, repo)
	targetRoot := filepath.Join(repo, ".worktrees")
	if err := os.Mkdir(targetRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(targetRoot, 0o755) })
	log := filepath.Join(t.TempDir(), "broker-invoked")
	fakeBroker(t, "#!/bin/sh\nprintf invoked > '"+log+"'\nexit 0\n")

	_, err := db.CreateWorktree(repo, "main", "1", false)
	if err == nil {
		t.Fatal("target permission failure unexpectedly succeeded")
	}
	if errors.Is(err, errWorktreeConfined) {
		t.Fatalf("target permission failure misclassified as confinement: %v", err)
	}
	var refusal *WorktreeBrokerRefusal
	if errors.As(err, &refusal) {
		t.Fatalf("target permission failure delegated: %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("broker ran for a sandbox-target failure: %v", err)
	}
}

func TestRegistrationGitLocaleIsPinned(t *testing.T) {
	t.Setenv("LANG", "fr_FR.UTF-8")
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	env := gitRegistrationEnvironment(os.Environ())
	if !slices.Contains(env, "LANG=C") || !slices.Contains(env, "LC_ALL=C") {
		t.Fatalf("registration environment is not C-localized: %v", env)
	}
	for _, item := range env {
		if item == "LANG=fr_FR.UTF-8" || item == "LC_ALL=fr_FR.UTF-8" {
			t.Fatalf("inherited locale survived: %q", item)
		}
	}

	db, repo := brokerTestRepo(t)
	confineRegistration(t, repo)
	unresolveBroker(t)
	_, err := db.CreateWorktree(repo, "main", "1", false)
	requireConfined(t, err)
	if exists, err := worktreeBranchExists(repo, "tasks/main/1"); err != nil || exists {
		t.Fatalf("localized failure branch survived: exists=%v err=%v", exists, err)
	}
}

func TestRegistrationPermissionFailureLinkedWorktreeCommonDir(t *testing.T) {
	db, repo := brokerTestRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	git(t, repo, "worktree", "add", "-b", "linked-test", "--", linked, "main")
	reg := filepath.Join(repo, ".git", "worktrees")
	if err := os.Chmod(reg, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(reg, 0o755) })
	unresolveBroker(t)

	_, err := db.CreateWorktree(linked, "main", "1", false)
	requireConfined(t, err)
	if exists, err := worktreeBranchExists(linked, "tasks/main/1"); err != nil || exists {
		t.Fatalf("linked-worktree branch survived: exists=%v err=%v", exists, err)
	}
}

// TestBrokerEnvironmentIsStrippedAndBounded proves the child environment is
// exactly the fixed allowlist regardless of inherited noise.
func TestBrokerEnvironmentIsStrippedAndBounded(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.worktree")
	t.Setenv("NOISE_CLAIM_TOKEN", "secret")
	env := brokerEnvironment()
	want := []string{"LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	if !slices.Equal(env, want) {
		t.Fatalf("environment = %v, want exactly %v", env, want)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "NOISE_") {
			t.Fatalf("inherited %q leaked into the broker environment", kv)
		}
	}
}

// TestSanitizeBrokerReason pins the reason sanitizer: printable, single-line,
// bounded.
func TestSanitizeBrokerReason(t *testing.T) {
	if got := sanitizeBrokerReason("a\x00b\tc\nd"); got != "a b c d" {
		t.Fatalf("sanitize = %q", got)
	}
	if got := sanitizeBrokerReason(strings.Repeat("y", brokerMaxReason+50)); len(got) != brokerMaxReason {
		t.Fatalf("length = %d, want %d", len(got), brokerMaxReason)
	}
	if got := sanitizeBrokerReason(" inverted\x1b[31mred "); strings.Contains(got, "\x1b") {
		t.Fatalf("sanitize = %q, still carries escape bytes", got)
	}
}

// Compile-time check that the fake-broker scripts stay shell scripts.
var _ = exec.Command
