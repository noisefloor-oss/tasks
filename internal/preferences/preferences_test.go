package preferences

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func validPreferences() Preferences {
	return Preferences{
		SchemaVersion: SchemaVersion,
		Generation: Generation{
			Adapter: AdapterOMP,
			Model:   "zai/glm-5.2",
		},
	}
}

func writeDocument(t *testing.T, boundary string, raw []byte) string {
	t.Helper()
	dir := filepath.Join(boundary, stateDirectory)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := Path(boundary)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPreservesRawAndMarshalCanonical(t *testing.T) {
	boundary := t.TempDir()
	raw := []byte("{\n  \"generation\": {\"model\": \"zai/glm-5.2\", \"adapter\": \"omp\"},\n  \"schemaVersion\": 1\n}\n")
	writeDocument(t, boundary, raw)

	snapshot, err := Load(boundary)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Exists || snapshot.Preferences != validPreferences() {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if !bytes.Equal(snapshot.Raw, raw) {
		t.Fatalf("raw snapshot changed:\n%s", snapshot.Raw)
	}

	canonical, err := MarshalCanonical(snapshot.Preferences)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"schemaVersion\": 1,\n  \"generation\": {\n    \"adapter\": \"omp\",\n    \"model\": \"zai/glm-5.2\"\n  }\n}\n"
	if string(canonical) != want {
		t.Fatalf("canonical = %q, want %q", canonical, want)
	}
}

func TestLoadMissingIsSuccessfulSnapshot(t *testing.T) {
	for _, makeTasks := range []bool{false, true} {
		boundary := t.TempDir()
		if makeTasks {
			if err := os.Mkdir(filepath.Join(boundary, stateDirectory), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		snapshot, err := Load(boundary)
		if err != nil || snapshot.Exists || snapshot.Raw != nil {
			t.Errorf("tasks=%v snapshot=%+v err=%v", makeTasks, snapshot, err)
		}
	}
}

func TestLoadRejectsClosedSchemaAndInvalidValues(t *testing.T) {
	tooLong := strings.Repeat("m", MaxModelBytes+1)
	tests := map[string]string{
		"top level array":        `[]`,
		"unknown top key":        `{"schemaVersion":1,"generation":{"adapter":"omp","model":"m"},"extra":true}`,
		"duplicate top key":      `{"schemaVersion":1,"schemaVersion":1,"generation":{"adapter":"omp","model":"m"}}`,
		"missing version":        `{"generation":{"adapter":"omp","model":"m"}}`,
		"missing generation":     `{"schemaVersion":1}`,
		"fractional version":     `{"schemaVersion":1.0,"generation":{"adapter":"omp","model":"m"}}`,
		"exponent version":       `{"schemaVersion":1e0,"generation":{"adapter":"omp","model":"m"}}`,
		"string version":         `{"schemaVersion":"1","generation":{"adapter":"omp","model":"m"}}`,
		"newer version":          `{"schemaVersion":2,"generation":{"adapter":"omp","model":"m"}}`,
		"generation array":       `{"schemaVersion":1,"generation":[]}`,
		"unknown generation key": `{"schemaVersion":1,"generation":{"adapter":"omp","model":"m","extra":"x"}}`,
		"duplicate adapter":      `{"schemaVersion":1,"generation":{"adapter":"omp","adapter":"omp","model":"m"}}`,
		"duplicate model":        `{"schemaVersion":1,"generation":{"adapter":"omp","model":"m","model":"m"}}`,
		"missing adapter":        `{"schemaVersion":1,"generation":{"model":"m"}}`,
		"missing model":          `{"schemaVersion":1,"generation":{"adapter":"omp"}}`,
		"nonstring adapter":      `{"schemaVersion":1,"generation":{"adapter":1,"model":"m"}}`,
		"nonstring model":        `{"schemaVersion":1,"generation":{"adapter":"omp","model":1}}`,
		"unknown adapter":        `{"schemaVersion":1,"generation":{"adapter":"shell","model":"m"}}`,
		"empty model":            `{"schemaVersion":1,"generation":{"adapter":"omp","model":""}}`,
		"padded model":           `{"schemaVersion":1,"generation":{"adapter":"omp","model":" m "}}`,
		"option model":           `{"schemaVersion":1,"generation":{"adapter":"omp","model":"--danger"}}`,
		"control model":          `{"schemaVersion":1,"generation":{"adapter":"omp","model":"m\u001f"}}`,
		"delete model":           `{"schemaVersion":1,"generation":{"adapter":"omp","model":"m\u007f"}}`,
		"long model":             `{"schemaVersion":1,"generation":{"adapter":"omp","model":"` + tooLong + `"}}`,
		"trailing object":        `{"schemaVersion":1,"generation":{"adapter":"omp","model":"m"}} {}`,
		"trailing garbage":       `{"schemaVersion":1,"generation":{"adapter":"omp","model":"m"}} x`,
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			boundary := t.TempDir()
			writeDocument(t, boundary, []byte(document))
			if _, err := Load(boundary); err == nil {
				t.Fatal("Load succeeded, want refusal")
			}
		})
	}
}

func TestLoadRejectsInvalidUTF8AndOversize(t *testing.T) {
	boundary := t.TempDir()
	writeDocument(t, boundary, []byte{'{', 0xff, '}'})
	if _, err := Load(boundary); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}

	canonical, err := MarshalCanonical(validPreferences())
	if err != nil {
		t.Fatal(err)
	}
	exact := append(bytes.Clone(canonical), bytes.Repeat([]byte{' '}, MaxDocumentBytes-len(canonical))...)
	writeDocument(t, boundary, exact)
	if snapshot, err := Load(boundary); err != nil || !snapshot.Exists {
		t.Fatalf("exact-size document: snapshot=%+v err=%v", snapshot, err)
	}
	writeDocument(t, boundary, append(exact, ' '))
	if _, err := Load(boundary); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestLoadAndPublishRefuseUnsafeStatePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated Windows privileges")
	}
	t.Run("state symlink", func(t *testing.T) {
		boundary := t.TempDir()
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(boundary, stateDirectory)); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(boundary); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("Load error = %v", err)
		}
		if err := Publish(boundary, validPreferences()); err == nil {
			t.Fatal("Publish followed state symlink")
		}
	})
	t.Run("state regular file", func(t *testing.T) {
		boundary := t.TempDir()
		if err := os.WriteFile(filepath.Join(boundary, stateDirectory), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(boundary); err == nil {
			t.Fatal("Load accepted regular .tasks path")
		}
		if err := Publish(boundary, validPreferences()); err == nil {
			t.Fatal("Publish accepted regular .tasks path")
		}
	})
	t.Run("preferences symlink", func(t *testing.T) {
		boundary := t.TempDir()
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("untouched"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(boundary, stateDirectory), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, Path(boundary)); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(boundary); err == nil {
			t.Fatal("Load accepted preferences symlink")
		}
		if err := Publish(boundary, validPreferences()); err == nil {
			t.Fatal("Publish replaced preferences symlink")
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "untouched" {
			t.Fatalf("symlink target = %q, %v", got, err)
		}
	})
}

func TestPublishCreatesCanonicalExactModeAndReplaces(t *testing.T) {
	boundary := t.TempDir()
	if err := Publish(boundary, validPreferences()); err != nil {
		t.Fatal(err)
	}
	want, _ := MarshalCanonical(validPreferences())
	got, err := os.ReadFile(Path(boundary))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("published = %q, err=%v", got, err)
	}
	info, err := os.Stat(Path(boundary))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("fresh mode = %v, err=%v", info.Mode().Perm(), err)
	}

	if err := os.Chmod(Path(boundary), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := validPreferences()
	replacement.Generation.Model = "zai/glm-5.2-highspeed"
	if err := Publish(boundary, replacement); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(Path(boundary))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("replacement mode = %v, err=%v", info.Mode().Perm(), err)
	}
	snapshot, err := Load(boundary)
	if err != nil || snapshot.Preferences != replacement {
		t.Fatalf("replacement snapshot = %+v, err=%v", snapshot, err)
	}
}

func TestAdapterAvailable(t *testing.T) {
	boundary := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "")
	available, err := AdapterAvailable(boundary, AdapterAnthropicAPI)
	if err != nil || !available {
		t.Fatalf("in-process adapter = %v, %v", available, err)
	}
	if _, err := AdapterAvailable(boundary, "unknown"); err == nil {
		t.Fatal("unknown adapter accepted")
	}

	for _, adapter := range []string{AdapterClaudeCLI, AdapterOMP, AdapterCodex, AdapterKimi} {
		t.Run(adapter, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			available, err := AdapterAvailable(boundary, adapter)
			if err != nil || available {
				t.Fatalf("missing CLI = %v, %v", available, err)
			}
			if _, err := ResolveAdapterExecutable(boundary, adapter); !errors.Is(err, ErrAdapterUnavailable) {
				t.Fatalf("missing resolved error = %v", err)
			}

			external := t.TempDir()
			name := adapter
			if adapter == AdapterClaudeCLI {
				name = "claude"
			}
			writeExecutable(t, filepath.Join(external, name))
			t.Setenv("PATH", external)
			available, err = AdapterAvailable(boundary, adapter)
			if err != nil || !available {
				t.Fatalf("external CLI = %v, %v", available, err)
			}
			resolved, err := ResolveAdapterExecutable(boundary, adapter)
			if err != nil || resolved != filepath.Join(external, name) {
				t.Fatalf("resolved external CLI = %q, %v", resolved, err)
			}

			inside := filepath.Join(boundary, adapter)
			if err := os.Mkdir(inside, 0o755); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(inside, name))
			t.Setenv("PATH", inside)
			available, err = AdapterAvailable(boundary, adapter)
			if err != nil || available {
				t.Fatalf("project-contained CLI = %v, %v", available, err)
			}
			if _, err := ResolveAdapterExecutable(boundary, adapter); !errors.Is(err, ErrAdapterContained) {
				t.Fatalf("contained resolved error = %v", err)
			}
		})
	}
}

func TestAdapterContainmentUsesPathComponents(t *testing.T) {
	parent := t.TempDir()
	boundary := filepath.Join(parent, "project")
	siblingBin := filepath.Join(parent, "project-sibling", "bin")
	if err := os.MkdirAll(boundary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(siblingBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(siblingBin, "claude"))
	t.Setenv("PATH", siblingBin)
	available, err := AdapterAvailable(boundary, AdapterClaudeCLI)
	if err != nil || !available {
		t.Fatalf("prefix sibling = %v, %v", available, err)
	}

	if runtime.GOOS != "windows" {
		inside := filepath.Join(boundary, "omp-real")
		writeExecutable(t, inside)
		outside := t.TempDir()
		if err := os.Symlink(inside, filepath.Join(outside, "omp")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", outside)
		available, err = AdapterAvailable(boundary, AdapterOMP)
		if err != nil || available {
			t.Fatalf("symlink into project = %v, %v", available, err)
		}
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDoesNotMutateInput(t *testing.T) {
	prefs := validPreferences()
	prefs.Generation.Model = " model "
	before := prefs
	if err := Validate(prefs); err == nil {
		t.Fatal("padded model accepted")
	}
	if prefs != before {
		t.Fatal("Validate normalized input instead of rejecting it")
	}
}

func TestLoadRejectsPreferencesDirectory(t *testing.T) {
	boundary := t.TempDir()
	if err := os.MkdirAll(Path(boundary), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Load(boundary)
	if err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("directory error = %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) && strings.Contains(err.Error(), "not exist") {
		t.Fatalf("directory was mistaken for absence: %v", err)
	}
}
