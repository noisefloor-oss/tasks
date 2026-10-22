package exportcheck

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// All required public files are available; no missing files are exempted.
var pendingPublicFiles = map[string]string{}

var requiredPublicFiles = []string{
	"LICENSE", "README.md", "SECURITY.md", "DEVELOPMENT.md", ".github/ISSUE_TEMPLATE/config.yml",
	"skills/install-noise-tasks/SKILL.md",
}

// Internal documents and tooling whose export-ignore rules must not be
// dropped. Literals are split so this source passes its own archive scan.
var requiredExcludedFiles = []string{
	"cmd/noise-tasks-" + "eval",
	"docs/GENERATION_" + "EVALS.md",
	"docs/SPEC" + ".md",
	"eva" + "ls",
}

// Split private vocabulary so the gate's own shipped source is scanned too.
var privateMarkers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)` + "noisefloor" + "-app"),
	regexp.MustCompile("/work" + "space/"),
	regexp.MustCompile(`(?i)` + "brain" + "company"),
	regexp.MustCompile(`(?i)` + "doug" + "@"),
	regexp.MustCompile(`(?i)\b` + "task" + `[ -]+\d+[a-z]?\b`),
	regexp.MustCompile(`(?i)\b` + "PR" + `\s*#\d+\b|\(#\d+\)`),
	regexp.MustCompile(`(?i)\b(?:` + "F" + "R|NF" + "R" + `)-\d+\b`),
	regexp.MustCompile(`(?i)` + "task" + "-master"),
	regexp.MustCompile(`(?i)` + "research" + "-agent|agent" + "-console"),
	regexp.MustCompile(`(?i)\b` + "review" + `[ -]+round[ -]*\d+\b`),
}

type exception struct {
	path, pattern, reason string
}

// Each entry permits only the matching text in the named path.
var allowedHits = []exception{
	{"scripts/package.sh", "^/work" + "space/$", "refused install prefix in the setuid broker safety rule, not a private path"},
	{"internal/backlog/testdata/roundtrip/basic.json", `(?i)^task [0-9]+$`, "synthetic task titles and subtask titles"},
	{"internal/backlog/testdata/roundtrip/legacy_master.json", `(?i)^task [0-9]+$`, "synthetic legacy task titles"},
	{"internal/backlog/testdata/roundtrip/legacy_meta.json", `(?i)^task [1-3]$`, "synthetic legacy task titles"},
	{"internal/backlog/testdata/roundtrip/two_tags.json", `(?i)^task [0-9]+$`, "synthetic task titles across tags"},
	{"internal/backlog/testdata/validation/bad_dep_id.json", `(?i)^` + "task" + ` 1$`, "synthetic validation task title"},
	{"internal/backlog/testdata/validation/bad_id.json", `(?i)^` + "task" + ` 1$`, "synthetic validation task title"},
	{"internal/backlog/testdata/validation/bad_status.json", `(?i)^` + "task" + ` 1$`, "synthetic validation task title"},
	{"internal/backlog/testdata/validation/dup_ids.json", `(?i)^` + "task" + ` 1$`, "synthetic duplicate task titles"},
	{"internal/backlog/testdata/gen_fixtures.go", `(?i)^` + "task" + ` 1$`, "generator for synthetic validation task titles"},
	{"internal/backlog/backlog_test.go", `(?i)^` + "task" + ` 1$`, "fixture assertions"},
	{"internal/store/backup_test.go", `(?i)^task [12]$`, "backup fixture diagnostics"},
	{"internal/store/legacy_test.go", `(?i)^` + "task" + ` 1$`, "restore fixture assertions"},
	{"internal/store/orchestrator_plan_test.go", `(?i)^task [23]$`, "plan fixture assertions"},
	{"internal/store/claim_fence_test.go", `(?i)^` + "task" + `-1$`, "synthetic branch name"},
	{"cmd/noise-tasks/write_test.go", `(?i)^task [12]$`, "CLI fixture assertions and fixture structure comments"},
	{"cmd/noise-tasks/worktree_remove_test.go", `(?i)^` + "task" + ` 2$`, "CLI fixture assertion"},
	{"cmd/noise-tasks/worktree_prune_test.go", `(?i)^` + "task" + ` 1$`, "documents the task provisioned by the fixture helper"},
	{"cmd/noise-tasks/hygiene_test.go", `(?i)^task [13]$`, "documents claim and run fixtures"},
	{"cmd/noise-tasks/claimnext_test.go", `(?i)^task [12]$`, "claim fixture assertions and dependency comments"},
	{"cmd/noise-tasks/verify_test.go", `(?i)^task [123]$`, "CLI fixture assertions and fixture edits"},
	{"cmd/noise-tasks/milestone_test.go", `(?i)^task [24]$`, "milestone fixture assertions and dependency comments"},
	{"cmd/noise-tasks/read_test.go", `(?i)^task [123]$`, "CLI fixture assertions and dependency comments"},
	{"cmd/noise-tasks/token_file_test.go", `(?i)^` + "task" + ` 1$`, "documents the task claimed by the fixture helper"},
}

type member struct {
	data []byte
	mode os.FileMode
	dir  bool
}

type snapshot map[string]member

func gitOutput(t *testing.T, root string, env []string, input []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); os.IsNotExist(err) {
		t.Skip("source export has no Git checkout")
	} else if err != nil {
		t.Fatal(err)
	}
	return root
}

func archiveHEAD(t *testing.T, root string) (snapshot, []string) {
	t.Helper()
	data := gitOutput(t, root, nil, nil, "archive", "HEAD")
	files, err := readArchive(data)
	if err != nil {
		t.Fatal(err)
	}

	// Ask Git to interpret the committed attributes, including directory and
	// wildcard rules. A temporary index avoids reading uncommitted attributes
	// or changing the user's index, and works with older Git releases too.
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index")}
	gitOutput(t, root, env, nil, "read-tree", "HEAD")
	names := gitOutput(t, root, nil, nil, "ls-tree", "-r", "-t", "--name-only", "-z", "HEAD")
	attrs := gitOutput(t, root, env, names, "check-attr", "--cached", "-z", "--stdin", "export-ignore")
	fields := strings.Split(strings.TrimSuffix(string(attrs), "\x00"), "\x00")
	var excluded []string
	for i := 0; i+2 < len(fields); i += 3 {
		if fields[i+2] == "set" {
			excluded = append(excluded, fields[i])
		}
	}
	return files, excluded
}

func readArchive(data []byte) (snapshot, error) {
	files := snapshot{}
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !filepath.IsLocal(name) || path.Clean(name) != name || strings.Contains(name, `\`) {
			return nil, fmt.Errorf("unsafe archive path %q", header.Name)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			return nil, fmt.Errorf("unsupported archive member %q (type %d)", name, header.Typeflag)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		files[name] = member{body, os.FileMode(header.Mode).Perm(), header.Typeflag == tar.TypeDir}
	}
}

func permitted(name string, data []byte, start, end int) bool {
	hit := string(data[start:end])
	for _, allow := range allowedHits {
		if !regexp.MustCompile(allow.pattern).MatchString(hit) {
			continue
		}
		if name == allow.path {
			return true
		}
	}
	return false
}

func inspectExport(files snapshot, excluded []string) []string {
	var findings []string
	for _, name := range requiredPublicFiles {
		if file, ok := files[name]; (!ok || file.dir) && pendingPublicFiles[name] == "" {
			findings = append(findings, "missing required public file: "+name)
		}
	}
	patterns := append([]*regexp.Regexp{}, privateMarkers...)
	refs := map[string]bool{}
	for _, ignored := range excluded {
		// Prose in the same directory often names only the basename.
		for _, ref := range []string{ignored, path.Base(ignored)} {
			if !refs[ref] {
				patterns = append(patterns, regexp.MustCompile(regexp.QuoteMeta(ref)))
				refs[ref] = true
			}
		}
	}
	for name, file := range files {
		for _, ignored := range excluded {
			if name == ignored || strings.HasPrefix(name, ignored+"/") {
				findings = append(findings, "excluded archive member: "+name)
			}
		}
		if file.dir || !utf8.Valid(file.data) || bytes.ContainsRune(file.data, 0) {
			continue
		}
		for _, pattern := range patterns {
			for _, loc := range pattern.FindAllIndex(file.data, -1) {
				if permitted(name, file.data, loc[0], loc[1]) {
					continue
				}
				line := bytes.Count(file.data[:loc[0]], []byte("\n")) + 1
				findings = append(findings, fmt.Sprintf("%s:%d: forbidden reference %q", name, line, file.data[loc[0]:loc[1]]))
			}
		}
		if strings.EqualFold(path.Ext(name), ".md") {
			findings = append(findings, markdownFindings(name, string(file.data), files)...)
		}
	}
	sort.Strings(findings)
	return findings
}

func checkExport(t *testing.T, files snapshot, excluded []string) {
	t.Helper()
	// A tree without export-ignore rules is a public checkout; the internal
	// exclusion rules live only in the private source tree.
	if len(excluded) > 0 {
		ignored := map[string]bool{}
		for _, name := range excluded {
			ignored[name] = true
		}
		for _, name := range requiredExcludedFiles {
			if !ignored[name] {
				t.Errorf("internal path lost its export-ignore rule: %s", name)
			}
		}
	}
	for _, allow := range allowedHits {
		t.Logf("allow: %s | %s | %s", allow.path, allow.pattern, allow.reason)
	}
	for name, reason := range pendingPublicFiles {
		if _, ok := files[name]; !ok {
			t.Logf("pending public file: %s (%s)", name, reason)
		}
	}
	for _, finding := range inspectExport(files, excluded) {
		t.Error(finding)
	}
}

func TestExportContent(t *testing.T) {
	files, excluded := archiveHEAD(t, repoRoot(t))
	checkExport(t, files, excluded)
}

// The public repository holds the exported tree under Git, without the
// private export-ignore rules; the checks must pass there too.
func TestExportContentPublicCheckout(t *testing.T) {
	files, _ := archiveHEAD(t, repoRoot(t))
	root := t.TempDir()
	writeSnapshot(t, root, files)
	gitOutput(t, root, nil, nil, "init", "-q")
	gitOutput(t, root, nil, nil, "add", "-A")
	gitOutput(t, root, nil, nil, "-c", "user.name=Export Test", "-c", "user.email=export-test@example.com", "commit", "-qm", "public snapshot")
	publicFiles, publicExcluded := archiveHEAD(t, root)
	if len(publicExcluded) != 0 {
		t.Fatalf("public checkout must have no export-ignore rules: %v", publicExcluded)
	}
	checkExport(t, publicFiles, publicExcluded)
}

func writeSnapshot(t *testing.T, root string, files snapshot) {
	t.Helper()
	for name, file := range files {
		dest := filepath.Join(root, filepath.FromSlash(name))
		if file.dir {
			if err := os.MkdirAll(dest, 0755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, file.data, file.mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportBuildAndVet(t *testing.T) {
	files, _ := archiveHEAD(t, repoRoot(t))
	root := t.TempDir()
	writeSnapshot(t, root, files)
	for _, verb := range []string{"build", "vet"} {
		t.Run(verb, func(t *testing.T) {
			cmd := exec.Command("go", verb, "./...")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOWORK=off")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("go %s ./... in extracted archive: %v\n%s", verb, err, out)
			}
		})
	}
}
