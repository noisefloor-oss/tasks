package broker

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) Key {
	t.Helper()
	k, err := LoadKey(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func validCap() Capability {
	return Capability{
		Verb:             VerbCreate,
		FamilyID:         "family-1",
		SagaFence:        "saga-7",
		RegistrationRoot: "/repo/.git/worktrees",
		OwnerBoundary:    "/repo",
		Target:           "/repo/.worktrees/main-42",
		Branch:           "tasks/main/42",
		BaseCommit:       strings.Repeat("a", 40),
		UID:              1200,
		GID:              1200,
		Expires:          time.Now().Add(time.Minute).Unix(),
	}
}

func TestMintParseRoundTrip(t *testing.T) {
	key := testKey(t)
	doc, err := Mint(key, validCap())
	if err != nil {
		t.Fatal(err)
	}
	cap, err := Parse(key, doc, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	want := validCap()
	if *cap != want {
		t.Fatalf("round trip mismatch: %+v != %+v", *cap, want)
	}
}

func TestParseTampering(t *testing.T) {
	key := testKey(t)
	doc, err := Mint(key, validCap())
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	i := strings.Index(s, `"family_id":"family-1"`)
	tampered := s[:i] + `"family_id":"family-9"` + s[i+len(`"family_id":"family-1"`):]
	if _, err := Parse(key, []byte(tampered), time.Now().Unix()); err == nil {
		t.Fatal("tampered capability accepted")
	}
	other, _ := LoadKey(bytes.Repeat([]byte{0x99}, 32))
	if _, err := Parse(other, doc, time.Now().Unix()); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestParseExpiry(t *testing.T) {
	key := testKey(t)
	cap := validCap()
	cap.Expires = time.Now().Add(-time.Second).Unix()
	doc, _ := Mint(key, cap)
	if _, err := Parse(key, doc, time.Now().Unix()); err == nil {
		t.Fatal("expired capability accepted")
	}
}

func TestParseStrictEncoding(t *testing.T) {
	key := testKey(t)
	doc, _ := Mint(key, validCap())
	s := string(doc)
	body := s[:strings.LastIndexByte(s[:len(s)-1], '\n')]
	for _, mangle := range []string{
		// Reordered fields must not decode.
		strings.Replace(body, `"verb":"create","family_id"`, `"family_id":"family-1","verb":"create","family_id"`, 1),
		body + " ",
		"{}",
		"",
	} {
		if _, err := Parse(key, []byte(mangle+"\n00\n"), time.Now().Unix()); err == nil {
			t.Fatalf("non-canonical capability accepted: %q", mangle)
		}
	}
	// These documents carry a valid authenticator, so rejection must come
	// from the canonical decoder rather than the MAC layer.
	for _, mangle := range []string{
		strings.Replace(body, `"target":"/repo/`, `"target":"\u002frepo/`, 1),
		strings.Replace(body, `"uid":1200`, `"uid":01200`, 1),
	} {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(capabilityDomain))
		mac.Write([]byte(mangle))
		signed := mangle + "\n" + hex.EncodeToString(mac.Sum(nil)) + "\n"
		if _, err := Parse(key, []byte(signed), time.Now().Unix()); err == nil {
			t.Fatalf("authenticated non-canonical capability accepted: %q", mangle)
		}
	}
}

func TestValidateBindingsRefusals(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Capability)
	}{
		{"bad verb", func(c *Capability) { c.Verb = "destroy" }},
		{"family charset", func(c *Capability) { c.FamilyID = "fam ily" }},
		{"empty saga", func(c *Capability) { c.SagaFence = "" }},
		{"relative registration", func(c *Capability) { c.RegistrationRoot = "repo/.git/worktrees" }},
		{"control byte in boundary", func(c *Capability) { c.OwnerBoundary = "/repo\nname"; c.Target = "/repo\nname/.worktrees/main-42" }},
		{"unclean boundary", func(c *Capability) { c.OwnerBoundary = "/repo/../repo"; c.Target = "/repo/.worktrees/main-42" }},
		{"target mismatch", func(c *Capability) { c.Target = "/elsewhere/.worktrees/main-42" }},
		{"slash tag", func(c *Capability) { c.Branch = "tasks/ma/in/42"; c.Target = "/repo/.worktrees/ma-42" }},
		{"leading zero id", func(c *Capability) { c.Branch = "tasks/main/042"; c.Target = "/repo/.worktrees/main-042" }},
		{"abbreviated base", func(c *Capability) { c.BaseCommit = "abcdef" }},
		{"empty base on create", func(c *Capability) { c.BaseCommit = "" }},
		{"empty base on remove", func(c *Capability) { c.Verb = VerbRemove; c.BaseCommit = "" }},
		{"abbreviated base on prune-registration", func(c *Capability) { c.Verb = VerbPruneRegistration; c.BaseCommit = "abcdef" }},
		{"force on create", func(c *Capability) { c.Force = true }},
		{"negative uid", func(c *Capability) { c.UID = -1 }},
	}
	for _, tc := range cases {
		cap := validCap()
		tc.mut(&cap)
		if err := cap.ValidateBindings(); err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
	}
	vc := validCap()
	if err := vc.ValidateBindings(); err != nil {
		t.Fatalf("valid capability refused: %v", err)
	}
}

func TestValidateBindingsHistoricalTagCompatibility(t *testing.T) {
	for _, tag := range []string{
		"feature+1",
		"café",
		strings.Repeat("a", 64),
		strings.Repeat("a", 65),
		strings.Repeat("a", 191),
	} {
		cap := validCap()
		cap.Branch = "tasks/" + tag + "/42"
		cap.Target = "/repo/.worktrees/" + tag + "-42"
		if len(cap.Branch) > maxBranchByte {
			t.Fatalf("test branch exceeds boundary: %d", len(cap.Branch))
		}
		if err := cap.ValidateBindings(); err != nil {
			t.Errorf("tag %q refused: %v", tag, err)
			continue
		}
		if _, err := Mint(testKey(t), cap); err != nil {
			t.Errorf("tag %q could not mint: %v", tag, err)
		}
	}

	cap := validCap()
	cap.Branch = "tasks/" + strings.Repeat("a", 192) + "/42"
	cap.Target = "/repo/.worktrees/" + strings.Repeat("a", 192) + "-42"
	if len(cap.Branch) != maxBranchByte+1 {
		t.Fatalf("test branch length = %d", len(cap.Branch))
	}
	if err := cap.ValidateBindings(); err == nil {
		t.Fatal("overlong historical branch accepted")
	}
}

// TestValidateBindingsPruneRegistrationNullBase covers §9.5's sole null
// base commit: exact legacy cleanup, represented as the empty string on
// the wire and recovered only through trusted prune.
func TestValidateBindingsPruneRegistrationNullBase(t *testing.T) {
	cap := validCap()
	cap.Verb = VerbPruneRegistration
	cap.BaseCommit = ""
	if err := cap.ValidateBindings(); err != nil {
		t.Fatalf("legacy-cleanup null base refused: %v", err)
	}
	key := testKey(t)
	doc, err := Mint(key, cap)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(key, doc, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.BaseCommit != "" {
		t.Fatalf("round trip changed the null base to %q", parsed.BaseCommit)
	}
	if err := parsed.ValidateBindings(); err != nil {
		t.Fatalf("parsed legacy-cleanup capability refused: %v", err)
	}
}

func TestSanitizeBounds(t *testing.T) {
	in := strings.Repeat("x", 2000) + "\x00\x01\xff"
	out := Sanitize(in)
	if len(out) > MaxReason {
		t.Fatalf("sanitized reason exceeds bound: %d", len(out))
	}
	if strings.ContainsAny(out, "\x00\x01") {
		t.Fatal("control bytes survived sanitization")
	}
	if Sanitize("multi\nline\t text") != "multi line text" {
		t.Fatalf("unexpected sanitization: %q", Sanitize("multi\nline\t text"))
	}
}

// --- real Git fixtures ---

func gitEnv() []string {
	return gitEnvironment()
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func newRepo(t *testing.T) (boundary, registrationRoot string, cap *Capability) {
	t.Helper()
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	boundary = real
	runGit(t, boundary, "init", "-b", "main", ".")
	runGit(t, boundary, "config", "user.email", "t@example.com")
	runGit(t, boundary, "config", "user.name", "t")
	os.WriteFile(filepath.Join(boundary, "f.txt"), []byte("x"), 0o644)
	runGit(t, boundary, "add", "f.txt")
	runGit(t, boundary, "commit", "-m", "init")
	os.Mkdir(filepath.Join(boundary, ".worktrees"), 0o755)
	os.Mkdir(filepath.Join(boundary, ".git", "worktrees"), 0o755)
	runGit(t, boundary, "config", "core.excludesFile", "/dev/null")
	registrationRoot = filepath.Join(boundary, ".git", "worktrees")
	cap = &Capability{
		Verb:             VerbCreate,
		FamilyID:         "family-1",
		SagaFence:        "saga-7",
		RegistrationRoot: registrationRoot,
		OwnerBoundary:    boundary,
		Target:           filepath.Join(boundary, ".worktrees", "main-42"),
		Branch:           "tasks/main/42",
		BaseCommit:       strings.TrimSpace(runGit(t, boundary, "rev-parse", "HEAD")),
		UID:              os.Getuid(),
		GID:              os.Getgid(),
		Expires:          time.Now().Add(10 * time.Minute).Unix(),
	}
	return boundary, registrationRoot, cap
}

func TestExecuteCreateIdempotent(t *testing.T) {
	_, _, cap := newRepo(t)
	res, err := Execute(cap)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("first create reported no change")
	}
	if _, err := os.Stat(filepath.Join(cap.Target, "f.txt")); err != nil {
		t.Fatalf("worktree content missing: %v", err)
	}
	// Identical retry: the registration exists, changed=false, still exit 0 —
	// and the existing real-directory target passes path validation so the
	// retry actually reaches Execute.
	if err := cap.ValidatePaths(); err != nil {
		t.Fatalf("retry path validation refused: %v", err)
	}
	res, err = Execute(cap)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatal("identical retry reported a change")
	}
}

func TestExecuteCreateUsesBoundBaseCommit(t *testing.T) {
	boundary, _, cap := newRepo(t)
	want := cap.BaseCommit
	if err := os.WriteFile(filepath.Join(boundary, "later.txt"), []byte("later"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, boundary, "add", "later.txt")
	runGit(t, boundary, "commit", "-m", "move main")
	if got := strings.TrimSpace(runGit(t, boundary, "rev-parse", "HEAD")); got == want {
		t.Fatal("fixture main did not move")
	}
	if _, err := Execute(cap); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGit(t, cap.Target, "rev-parse", "HEAD")); got != want {
		t.Fatalf("worktree base = %s, want bound %s", got, want)
	}
}

func TestExecuteIgnoresCallerPATH(t *testing.T) {
	_, _, cap := newRepo(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if _, err := Execute(cap); err != nil {
		t.Fatalf("caller PATH selected a different Git: %v", err)
	}
}

// TestExecuteCreateUnregisteredDirectoryRefuses proves the create-replay
// repair stops at registration: an existing real directory reaches
// Execute but only the exact registered worktree is a no-op success.
func TestExecuteCreateUnregisteredDirectoryRefuses(t *testing.T) {
	_, _, cap := newRepo(t)
	if err := os.Mkdir(cap.Target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cap.ValidatePaths(); err != nil {
		t.Fatalf("existing real-directory target refused by validation: %v", err)
	}
	if _, err := Execute(cap); err == nil {
		t.Fatal("create over unregistered existing directory accepted")
	}
}

// TestExecuteCreateWrongBranchRefuses proves path equality alone is not an
// idempotent replay: the registered branch must match the capability too.
func TestExecuteCreateWrongBranchRefuses(t *testing.T) {
	boundary, _, cap := newRepo(t)
	wrongTarget := cap.Target
	runGit(t, boundary, "worktree", "add", "-b", "tasks/main/99", "--", wrongTarget, "main")
	if err := cap.ValidatePaths(); err != nil {
		t.Fatalf("registered target validation: %v", err)
	}
	if _, err := Execute(cap); err == nil || !strings.Contains(err.Error(), "another branch") {
		t.Fatalf("wrong-branch replay = %v, want branch-binding refusal", err)
	}
	cap.Verb = VerbRemove
	if _, err := Execute(cap); err == nil || !strings.Contains(err.Error(), "another branch") {
		t.Fatalf("wrong-branch removal = %v, want branch-binding refusal", err)
	}
}

func TestExecuteRemoveIdempotent(t *testing.T) {
	_, _, cap := newRepo(t)
	if _, err := Execute(cap); err != nil {
		t.Fatal(err)
	}
	cap.Verb = VerbRemove
	res, err := Execute(cap)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("remove reported no change")
	}
	res, err = Execute(cap)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatal("remove retry reported a change")
	}
}

func TestExecuteRemoveDirtyRefusesUnforced(t *testing.T) {
	_, _, cap := newRepo(t)
	if _, err := Execute(cap); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cap.Target, "dirty.txt"), []byte("d"), 0o644)
	cap.Verb = VerbRemove
	if _, err := Execute(cap); err == nil {
		t.Fatal("dirty remove accepted without force")
	}
	cap.Force = true
	if err := cap.ValidateBindings(); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(cap); err != nil {
		t.Fatalf("forced dirty remove refused: %v", err)
	}
}

func TestExecuteRemoveUnregisteredDirectoryRefuses(t *testing.T) {
	_, _, cap := newRepo(t)
	os.Mkdir(cap.Target, 0o755)
	cap.Verb = VerbRemove
	if _, err := Execute(cap); err == nil {
		t.Fatal("remove of unregistered directory accepted")
	}
}

func TestExecutePruneRegistration(t *testing.T) {
	boundary, _, cap := newRepo(t)
	if _, err := Execute(cap); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(boundary, ".worktrees", "main-99")
	runGit(t, boundary, "worktree", "add", "-b", "tasks/main/99", "--", other, cap.BaseCommit)
	if err := os.RemoveAll(cap.Target); err != nil {
		t.Fatal(err)
	}
	cap.Verb = VerbPruneRegistration
	res, err := Execute(cap)
	if err != nil || !res.Changed {
		t.Fatal(err)
	}
	res, err = Execute(cap)
	if err != nil || res.Changed {
		t.Fatalf("prune retry = %+v, %v", res, err)
	}
	registered, err := registeredWorktrees(boundary)
	if err != nil {
		t.Fatal(err)
	}
	if registered[other] != "refs/heads/tasks/main/99" {
		t.Fatal("scoped prune removed an unrelated registration")
	}
}

func TestBoundedBufferDrainsWithoutGrowing(t *testing.T) {
	b := &boundedBuffer{limit: 16}
	p := bytes.Repeat([]byte("x"), 1024)
	if n, err := b.Write(p); err != nil || n != len(p) {
		t.Fatalf("write = %d, %v", n, err)
	}
	if !b.overflow || b.buf.Len() != 16 {
		t.Fatalf("bounded buffer = len %d overflow %t", b.buf.Len(), b.overflow)
	}
}

func TestExecuteForeignRegistrationRootRefuses(t *testing.T) {
	_, _, cap := newRepo(t)
	cap.RegistrationRoot = filepath.Join(cap.OwnerBoundary, "elsewhere")
	if err := cap.ValidateBindings(); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(cap); err == nil {
		t.Fatal("foreign registration root accepted")
	}
}

func TestValidatePathsNoFollow(t *testing.T) {
	_, _, cap := newRepo(t)
	if err := cap.ValidatePaths(); err != nil {
		t.Fatalf("valid paths refused: %v", err)
	}
	// A symlinked owner boundary component refuses.
	alt := filepath.Join(filepath.Dir(cap.OwnerBoundary), "link-boundary")
	if err := os.Symlink(cap.OwnerBoundary, alt); err != nil {
		t.Skip("symlink unavailable")
	}
	bad := *cap
	bad.OwnerBoundary = alt
	bad.Target = filepath.Join(alt, ".worktrees", "main-42")
	if err := bad.ValidateBindings(); err != nil {
		t.Fatal(err)
	}
	if err := bad.ValidatePaths(); err == nil {
		t.Fatal("symlinked owner boundary accepted")
	}
	// An existing real-directory create target passes validation — the
	// identical-retry shape that Execute then decides; a symlinked or
	// non-directory target refuses; a missing target is the fresh case.
	os.MkdirAll(cap.Target, 0o755)
	if err := cap.ValidatePaths(); err != nil {
		t.Fatalf("existing real-directory create target refused: %v", err)
	}
	if err := os.Remove(cap.Target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cap.OwnerBoundary, cap.Target); err != nil {
		t.Skip("symlink unavailable")
	}
	if err := cap.ValidatePaths(); err == nil {
		t.Fatal("symlinked create target accepted")
	}
	if err := os.Remove(cap.Target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cap.Target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cap.ValidatePaths(); err == nil {
		t.Fatal("non-directory create target accepted")
	}
	if err := os.Remove(cap.Target); err != nil {
		t.Fatal(err)
	}
	if err := cap.ValidatePaths(); err != nil {
		t.Fatalf("absent create target refused: %v", err)
	}
}
