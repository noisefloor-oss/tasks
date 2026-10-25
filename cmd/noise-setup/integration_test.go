//go:build linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const module = "github.com/noisefloor-oss/tasks"

type fixture struct {
	t      *testing.T
	base   string
	parent string
	root   string
	bin    string
}

func newFixture(t *testing.T, point, filesystem string) *fixture {
	t.Helper()
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	parent := filepath.Join(base, "prefix", "libexec")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, base: base, parent: parent, root: filepath.Join(parent, "noise"), bin: filepath.Join(base, "noise-setup")}
	ld := []string{
		"-X", module + "/internal/libexec.Root=" + f.root,
		"-X", module + "/internal/setup.TestRoot=" + base,
	}
	if point != "" {
		ld = append(ld, "-X", module+"/internal/setup.TestPoint="+point, "-X", module+"/internal/setup.TestMarker="+filepath.Join(base, "marker"))
	}
	ld = append(ld, "-X", module+"/internal/setup.TestFilesystem="+fixtureFilesystem(filesystem))
	cmd := exec.Command("go", "build", "-ldflags", strings.Join(ld, " "), "-o", f.bin, ".")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "TMPDIR="+base)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build setup: %v\n%s", err, out)
	}
	return f
}

// Integration fixtures exercise setup policy independently of the host that
// runs the tests. An empty selection means a supported local filesystem;
// refusal cases pass the specific simulated policy they need.
func fixtureFilesystem(selected string) string {
	if selected == "" {
		return "ext4"
	}
	return selected
}

func (f *fixture) stage(name, body string) string {
	f.t.Helper()
	path := filepath.Join(f.parent, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "noise-tasks"), []byte(body), 0o700); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *fixture) command(stage string, extra ...string) *exec.Cmd {
	args := []string{"--stage", stage, "--setup-uid", strconv.Itoa(os.Geteuid()), "--setup-gid", strconv.Itoa(os.Getegid())}
	args = append(args, extra...)
	return exec.Command(f.bin, args...)
}

func (f *fixture) run(stage string, extra ...string) ([]byte, error) {
	return f.command(stage, extra...).CombinedOutput()
}

func (f *fixture) seed() (string, []byte) {
	f.t.Helper()
	if out, err := f.run(f.stage(".noise-stage-old", "old")); err != nil {
		f.t.Fatalf("seed: %v\n%s", err, out)
	}
	link, err := os.Readlink(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(f.parent, link, "noise-tasks"))
	if err != nil {
		f.t.Fatal(err)
	}
	return link, body
}

func (f *fixture) assertPrior(link string, body []byte) {
	f.t.Helper()
	got, err := os.Readlink(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	if got != link {
		f.t.Fatalf("recorded root changed: %q -> %q", link, got)
	}
	gotBody, err := os.ReadFile(filepath.Join(f.parent, link, "noise-tasks"))
	if err != nil {
		f.t.Fatal(err)
	}
	if !bytes.Equal(gotBody, body) {
		f.t.Fatalf("prior version changed: %q", gotBody)
	}
}

func TestSetupPublishesCompleteVersionAndPreservesOldTree(t *testing.T) {
	f := newFixture(t, "", "")
	old, oldBody := f.seed()
	if out, err := f.run(f.stage(".noise-stage-new", "new")); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	newLink, err := os.Readlink(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if newLink == old {
		t.Fatal("upgrade did not replace root link")
	}
	newBody, err := os.ReadFile(filepath.Join(f.parent, newLink, "noise-tasks"))
	if err != nil {
		t.Fatal(err)
	}
	if string(newBody) != "new" {
		t.Fatalf("published %q", newBody)
	}
	kept, err := os.ReadFile(filepath.Join(f.parent, old, "noise-tasks"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept, oldBody) {
		t.Fatal("old version was not retained intact")
	}
}

func TestSetupRefusalsPreservePriorInstallation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fixture, string)
		args   []string
		bare   bool
	}{
		{"undeclared effective principal", func(*fixture, string) {}, []string{"--setup-uid", "4294967294"}, true},
		{"invalid identity declaration", func(*fixture, string) {}, []string{"--runtime-uid", "not-a-uid"}, false},
		{"runtime uid is setup authority", func(*fixture, string) {}, []string{"--runtime-uid", strconv.Itoa(os.Geteuid())}, false},
		{"runtime root cannot be isolated", func(*fixture, string) {}, []string{"--runtime-uid", "0"}, false},
		{"runtime gid is writable setup group", func(*fixture, string) {}, []string{"--runtime-gid", strconv.Itoa(os.Getegid())}, false},
		{"undeclared writable group", func(f *fixture, _ string) { must(os.Chmod(f.parent, 0o720)) }, []string{"--setup-uid", strconv.Itoa(os.Geteuid())}, true},
		{"replaceable parent", func(f *fixture, _ string) { must(os.Chmod(f.parent, 0o707)) }, nil, false},
		{"public stage", func(_ *fixture, s string) { must(os.Chmod(s, 0o707)) }, nil, false},
		{"invalid entry", func(_ *fixture, s string) { must(os.WriteFile(filepath.Join(s, "README"), []byte("x"), 0o700)) }, nil, false},
		{"reserved noun", func(_ *fixture, s string) { must(os.WriteFile(filepath.Join(s, "noise-help"), []byte("x"), 0o700)) }, nil, false},
		{"missing tasks", func(_ *fixture, s string) {
			must(os.Rename(filepath.Join(s, "noise-tasks"), filepath.Join(s, "noise-other")))
		}, nil, false},
		{"non executable", func(_ *fixture, s string) { must(os.Chmod(filepath.Join(s, "noise-tasks"), 0o600)) }, nil, false},
		{"non regular", func(_ *fixture, s string) {
			must(os.Remove(filepath.Join(s, "noise-tasks")))
			must(os.Mkdir(filepath.Join(s, "noise-tasks"), 0o700))
		}, nil, false},
		{"escaping symlink", func(f *fixture, s string) {
			must(os.Remove(filepath.Join(s, "noise-tasks")))
			outside := filepath.Join(f.base, "outside")
			must(os.WriteFile(outside, []byte("x"), 0o700))
			must(os.Symlink(outside, filepath.Join(s, "noise-tasks")))
		}, nil, false},
		{"absolute internal symlink", func(_ *fixture, s string) {
			target := filepath.Join(s, "noise-other")
			must(os.Rename(filepath.Join(s, "noise-tasks"), target))
			must(os.Symlink(target, filepath.Join(s, "noise-tasks")))
		}, nil, false},
		{"staged root symlink", func(f *fixture, s string) {
			must(os.Remove(filepath.Join(s, "noise-tasks")))
			must(os.Remove(s))
			target := f.stage(".noise-stage-target", "target")
			must(os.Symlink(target, s))
		}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "", "")
			prior, body := f.seed()
			stage := f.stage(".noise-stage-next", "next")
			tt.mutate(f, stage)
			var out []byte
			var err error
			if tt.bare {
				out, err = exec.Command(f.bin, append([]string{"--stage", stage}, tt.args...)...).CombinedOutput()
			} else {
				out, err = f.run(stage, tt.args...)
			}
			if err == nil {
				t.Fatalf("setup accepted refusal case\n%s", out)
			}
			if !bytes.Contains(out, []byte("setup refused")) || !bytes.Contains(out, []byte(f.base)) {
				t.Fatalf("diagnostic lacks path and condition: %s", out)
			}
			f.assertPrior(prior, body)
		})
	}
}

func TestSetupAcceptsSupportedInjectedMountPolicies(t *testing.T) {
	for _, policy := range []string{"ext4", "xfs", "overlayfs", "aufs"} {
		t.Run(policy, func(t *testing.T) {
			f := newFixture(t, "", policy)
			out, err := f.run(f.stage(".noise-stage-next", "next"))
			if err != nil {
				t.Fatalf("refused supported %s mount: %v\n%s", policy, err, out)
			}
			link, err := os.Readlink(f.root)
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(f.parent, link, "noise-tasks"))
			if err != nil || string(body) != "next" {
				t.Fatalf("published body = %q, %v", body, err)
			}
		})
	}
}

func TestSetupRefusesUnsupportedInjectedMountPoliciesAndPreservesPrior(t *testing.T) {
	for _, policy := range []string{"nfs", "smb2", "ceph", "unknown", "read-only", "noexec", "acl"} {
		t.Run(policy, func(t *testing.T) {
			f := newFixture(t, "", policy)
			prior, body := seedWithNormalBinary(t, f)
			out, err := f.run(f.stage(".noise-stage-next", "next"))
			if err == nil {
				t.Fatalf("accepted refused %s mount policy", policy)
			}
			if !bytes.Contains(out, []byte(f.base)) || !bytes.Contains(out, []byte("setup refused")) {
				t.Fatalf("diagnostic lacks path and trust condition: %s", out)
			}
			f.assertPrior(prior, body)
		})
	}
}

func TestSetupRejectsInvalidRecordedRoots(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, root, authority string
		homebrew              bool
	}{
		{"relative", "relative/libexec/noise", base, false},
		{"outside test authority", filepath.Join(base, "outside", "noise"), filepath.Join(base, "authority"), false},
		{"workspace", filepath.Join("/workspace", "untrusted", "noise"), "", false},
		{"unblessed homebrew root", "/usr/libexec/noise", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.authority != "" {
				must(os.MkdirAll(tt.authority, 0o700))
			}
			bin := buildRaw(t, base, tt.root, tt.authority, "", "")
			args := []string{"--stage", filepath.Join(base, "stage"), "--setup-uid", strconv.Itoa(os.Geteuid())}
			if tt.homebrew {
				args = append(args, "--homebrew")
			}
			out, err := exec.Command(bin, args...).CombinedOutput()
			if err == nil {
				t.Fatalf("accepted invalid root\n%s", out)
			}
			if !bytes.Contains(out, []byte(tt.root)) {
				t.Fatalf("diagnostic lacks root path: %s", out)
			}
		})
	}
}

func TestSetupStagePathRefusalsPreservePriorInstallation(t *testing.T) {
	f := newFixture(t, "", "")
	prior, body := f.seed()
	for _, stage := range []string{"relative-stage", filepath.Join(f.base, "not-adjacent")} {
		out, err := f.run(stage)
		if err == nil {
			t.Fatalf("accepted invalid stage %q\n%s", stage, out)
		}
		f.assertPrior(prior, body)
	}
}

func TestSetupRefusesNonAtomicPriorRootWithoutChangingIt(t *testing.T) {
	f := newFixture(t, "", "")
	if err := os.Mkdir(f.root, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(f.root, "sentinel")
	if err := os.WriteFile(sentinel, []byte("prior"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(f.stage(".noise-stage-next", "next"))
	if err == nil {
		t.Fatalf("accepted non-link prior root\n%s", out)
	}
	got, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(got) != "prior" {
		t.Fatalf("prior root changed: %q %v", got, readErr)
	}
}

func TestSetupFailureBoundariesRestorePriorInstallation(t *testing.T) {
	for _, point := range []string{"validated", "versioned", "linked", "swapped"} {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t, "fail:"+point, "")
			prior, body := seedWithNormalBinary(t, f)
			out, err := f.run(f.stage(".noise-stage-next", "next"))
			if err == nil {
				t.Fatalf("failure point accepted\n%s", out)
			}
			f.assertPrior(prior, body)
		})
	}
}

func TestSetupRollbackFailuresPreserveCompleteRootAndAllowRetry(t *testing.T) {
	for _, withPrior := range []bool{false, true} {
		name := "without-prior"
		if withPrior {
			name = "with-prior"
		}
		for _, point := range []string{"rollback-before-restore", "rollback-after-restore"} {
			t.Run(name+"/"+point, func(t *testing.T) {
				f := newFixture(t, "", "")
				var prior string
				var body []byte
				if withPrior {
					prior, body = seedWithNormalBinary(t, f)
				}
				f.bin = buildFaultVariant(t, f, "fail:swapped", "", "fail:"+point)
				out, err := f.run(f.stage(".noise-stage-next", "next"))
				if err == nil {
					t.Fatalf("rollback failure point accepted\n%s", out)
				}
				if !bytes.Contains(out, []byte("rollback failed")) {
					t.Fatalf("rollback failure not reported: %s", out)
				}

				link, readErr := os.Readlink(f.root)
				switch {
				case point == "rollback-before-restore":
					if readErr != nil {
						t.Fatalf("new root is not intact: %v", readErr)
					}
					got, err := os.ReadFile(filepath.Join(f.parent, link, "noise-tasks"))
					if err != nil || string(got) != "next" {
						t.Fatalf("new root body = %q, %v", got, err)
					}
				case withPrior:
					if readErr != nil || link != prior {
						t.Fatalf("prior root was not restored: %q, %v", link, readErr)
					}
					f.assertPrior(prior, body)
				case !os.IsNotExist(readErr):
					t.Fatalf("root remains after no-prior restoration: %q, %v", link, readErr)
				}

				f.bin = buildVariant(t, f, "", "")
				out, err = f.run(f.stage(".noise-stage-retry", "retry"))
				if err != nil {
					t.Fatalf("setup retry failed: %v\n%s", err, out)
				}
				got, err := os.ReadFile(filepath.Join(f.root, "noise-tasks"))
				if err != nil || string(got) != "retry" {
					t.Fatalf("retry root body = %q, %v", got, err)
				}
			})
		}
	}
}

func TestSetupCrashBoundariesAreAtomic(t *testing.T) {
	for _, tc := range []struct{ point, want string }{{"versioned", "old"}, {"swapped", "next"}} {
		t.Run(tc.point, func(t *testing.T) {
			f := newFixture(t, "pause:"+tc.point, "")
			prior, _ := seedWithNormalBinary(t, f)
			cmd := f.command(f.stage(".noise-stage-next", "next"))
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitMarker(t, filepath.Join(f.base, "marker"))
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			link, err := os.Readlink(f.root)
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(f.parent, link, "noise-tasks"))
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != tc.want {
				t.Fatalf("observed partial publication %q", body)
			}
			if tc.point == "versioned" && link != prior {
				t.Fatal("pre-swap crash changed root")
			}
		})
	}
}

func TestConcurrentSetupIsSerialized(t *testing.T) {
	f := newFixture(t, "pause:locked", "")
	normal := buildVariant(t, f, "", "")
	first := f.command(f.stage(".noise-stage-one", "one"))
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	waitMarker(t, filepath.Join(f.base, "marker"))
	stage2 := f.stage(".noise-stage-two", "two")
	second := exec.Command(normal, "--stage", stage2, "--setup-uid", strconv.Itoa(os.Geteuid()), "--setup-gid", strconv.Itoa(os.Getegid()))
	done := make(chan error, 1)
	go func() { done <- second.Run() }()
	select {
	case err := <-done:
		t.Fatalf("concurrent setup bypassed lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second setup did not acquire released lock")
	}
}

func seedWithNormalBinary(t *testing.T, f *fixture) (string, []byte) {
	t.Helper()
	original := f.bin
	f.bin = buildVariant(t, f, "", "")
	link, body := f.seed()
	f.bin = original
	return link, body
}
func buildVariant(t *testing.T, f *fixture, point, filesystem string) string {
	t.Helper()
	return buildFaultVariant(t, f, point, filesystem, "")
}

func buildFaultVariant(t *testing.T, f *fixture, point, filesystem, rollbackPoint string) string {
	t.Helper()
	out := filepath.Join(f.base, "setup-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	ld := []string{"-X", module + "/internal/libexec.Root=" + f.root, "-X", module + "/internal/setup.TestRoot=" + f.base}
	if point != "" {
		ld = append(ld, "-X", module+"/internal/setup.TestPoint="+point)
	}
	if rollbackPoint != "" {
		ld = append(ld, "-X", module+"/internal/setup.TestRollbackPoint="+rollbackPoint)
	}
	ld = append(ld, "-X", module+"/internal/setup.TestFilesystem="+fixtureFilesystem(filesystem))
	cmd := exec.Command("go", "build", "-ldflags", strings.Join(ld, " "), "-o", out, ".")
	cmd.Env = append(os.Environ(), "TMPDIR="+f.base)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build variant: %v\n%s", err, data)
	}
	return out
}

func buildRaw(t *testing.T, outputDir, root, authority, point, filesystem string) string {
	t.Helper()
	out := filepath.Join(outputDir, "setup-raw-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	ld := []string{"-X", module + "/internal/libexec.Root=" + root}
	if authority != "" {
		ld = append(ld, "-X", module+"/internal/setup.TestRoot="+authority)
	}
	if point != "" {
		ld = append(ld, "-X", module+"/internal/setup.TestPoint="+point)
	}
	ld = append(ld, "-X", module+"/internal/setup.TestFilesystem="+fixtureFilesystem(filesystem))
	cmd := exec.Command("go", "build", "-ldflags", strings.Join(ld, " "), "-o", out, ".")
	cmd.Env = append(os.Environ(), "TMPDIR="+outputDir)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build raw setup: %v\n%s", err, data)
	}
	return out
}
func waitMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for publication point")
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
