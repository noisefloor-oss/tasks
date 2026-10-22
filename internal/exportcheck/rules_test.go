package exportcheck

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func publicFixture() snapshot {
	files := snapshot{}
	for _, name := range requiredPublicFiles {
		files[name] = member{data: []byte("public text")}
	}
	return files
}

func TestContentRules(t *testing.T) {
	for _, text := range []string{
		"noise" + "floor-app", "/work" + "space/private", "brain" + "company",
		"doug" + "@example.com", "task" + " 12", "Task" + " 75", "Task" + " 41c",
		"review" + " round 2",
		"PR" + " #12", "(" + "#123)", "F" + "R-12", "NF" + "R-3",
		"TASK" + "-MASTER", "research" + "-agent", "agent" + "-console",
	} {
		files := publicFixture()
		files["notes.txt"] = member{data: []byte(text)}
		if got := inspectExport(files, nil); len(got) == 0 {
			t.Errorf("missed private text %q", text)
		}
	}
	files := publicFixture()
	files["internal.txt"] = member{data: []byte("private notes")}
	files["README.md"] = member{data: []byte("See internal.txt and notes.md")}
	files["hidden/child.txt"] = member{data: []byte("nested")}
	findings := strings.Join(inspectExport(files, []string{"internal.txt", "hidden", "docs/notes.md"}), "\n")
	for _, want := range []string{"excluded archive member: internal.txt", "excluded archive member: hidden/child.txt", `forbidden reference "internal.txt"`, `forbidden reference "notes.md"`} {
		if !strings.Contains(findings, want) {
			t.Errorf("missing %q in %s", want, findings)
		}
	}
}

func TestRequiredPublicFiles(t *testing.T) {
	files := publicFixture()
	if got := inspectExport(files, nil); len(got) != 0 {
		t.Fatalf("complete public files must pass: %v", got)
	}
	for _, name := range requiredPublicFiles {
		files = publicFixture()
		delete(files, name)
		if got := strings.Join(inspectExport(files, nil), "\n"); !strings.Contains(got, "missing required public file: "+name) {
			t.Fatalf("%s must be required: %s", name, got)
		}
	}
	files = publicFixture()
	files["SECURITY.md"] = member{data: []byte("Task" + " 75")}
	if got := inspectExport(files, nil); len(got) == 0 {
		t.Fatal("present required files must still be scanned")
	}
}

func TestPrivateModuleReferencesAreRejected(t *testing.T) {
	module := "github.com/" + "noisefloor" + "-app/tasks"
	for _, tc := range []struct {
		name, text string
	}{
		{"go.mod", "module " + module + "\n"},
		{"go.mod", "// module " + module + "\n"},
		{"sample.go", "package sample\nimport \"" + module + "/internal/store\"\n"},
		{"sample.go", "package sample\n// " + module + "\n"},
		{"sample.go", "package sample\nconst name = \"" + module + "\"\n"},
		{"sample.go", "package sample\nimport \"" + module + "-other\"\n"},
		{"notes.md", module},
	} {
		files := publicFixture()
		files[tc.name] = member{data: []byte(tc.text)}
		got := inspectExport(files, nil)
		if len(got) == 0 {
			t.Errorf("%s: private module reference accepted: %q", tc.name, tc.text)
		}
	}
}

func TestFixtureExceptionIsLimitedToPathAndPattern(t *testing.T) {
	files := publicFixture()
	files["internal/backlog/testdata/validation/bad_id.json"] = member{data: []byte("Task" + " 1")}
	if got := inspectExport(files, nil); len(got) != 0 {
		t.Fatalf("synthetic title rejected: %v", got)
	}
	files["internal/backlog/testdata/validation/bad_id.json"] = member{data: []byte("Task" + " 75")}
	if got := inspectExport(files, nil); len(got) == 0 {
		t.Fatal("exception must not permit other citations")
	}
	files = publicFixture()
	files["README.md"] = member{data: []byte("Task" + " 1")}
	if got := inspectExport(files, nil); len(got) == 0 {
		t.Fatal("fixture exception must not exempt prose elsewhere")
	}
}

func TestMarkdownLinks(t *testing.T) {
	files := publicFixture()
	files["docs"] = member{dir: true}
	files["docs/guide.md"] = member{data: []byte("# Guide")}
	files["docs/with space.md"] = member{}
	files["docs/with(parens).md"] = member{}
	for _, text := range []string{
		"[guide](guide.md#guide)", "![image](../LICENSE)", "[root](/README.md)",
		"[guide](<with space.md> \"Title\")", "[guide](with%20space.md)",
		"[guide](with(parens).md)", "[guide](with\\(parens\\).md)",
		"[directory](../docs/)", "[root](../)", "[heading](#guide)",
		"[remote](https://example.com/missing)", "[remote](//example.com/missing)",
		"[email](mailto:reader@example.com)", "[guide][ref]\n[ref]: guide.md",
		"[guide][]\n[guide]: guide.md", "[guide]\n[guide]: guide.md",
		"`[example](missing)`", "```markdown\n[example](missing)\n```",
		"~~~\n[example](missing)\n~~~", "    [example](missing)",
	} {
		if got := markdownFindings("docs/guide.md", text, files); len(got) != 0 {
			t.Errorf("valid link %q: %v", text, got)
		}
	}
	for _, text := range []string{
		"[missing](absent.md)", "![missing](absent.png)", "[escape](../../README.md)",
		"[missing][ref]\n[ref]: absent.md", "[missing][undefined]",
		"[missing](<absent file.md>)", "[missing](/absent.md)",
	} {
		if got := markdownFindings("docs/guide.md", text, files); len(got) == 0 {
			t.Errorf("missed broken link %q", text)
		}
	}
}

func TestArchiveRejectsUnsafeMembers(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
	}{
		{"../escape", tar.TypeReg}, {"/absolute", tar.TypeReg}, {"a/../escape", tar.TypeReg},
		{"symlink", tar.TypeSymlink}, {"hardlink", tar.TypeLink},
	} {
		var data bytes.Buffer
		writer := tar.NewWriter(&data)
		if err := writer.WriteHeader(&tar.Header{Name: tc.name, Typeflag: tc.kind, Mode: 0644}); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := readArchive(data.Bytes()); err == nil {
			t.Errorf("accepted unsafe member %q", tc.name)
		}
	}
}

func TestCheckerSourceHasNoPrivateReferences(t *testing.T) {
	files := publicFixture()
	names, err := filepath.Glob("*")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		files["internal/exportcheck/"+name] = member{data: data}
	}
	for _, finding := range inspectExport(files, nil) {
		t.Error(finding)
	}
}
