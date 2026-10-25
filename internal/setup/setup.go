// Package setup validates and atomically publishes a fixed libexec tree.
package setup

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noisefloor-oss/tasks/internal/libexec"
)

// TestRoot is a build-time-only authority boundary for unprivileged process
// tests. Release builds leave it empty.
var TestRoot string

// These build-time-only values support process-level fault and trust-policy
// tests. They cannot be selected by a release process at runtime.
var TestPoint, TestRollbackPoint, TestMarker, TestFilesystem string

type IDs map[uint32]bool

type Config struct {
	Stage       string
	SetupUIDs   IDs
	SetupGIDs   IDs
	RuntimeUIDs IDs
	RuntimeGIDs IDs
	Homebrew    bool
}

func Publish(c Config) error {
	root := libexec.Root
	if err := validateRecordedRoot(root); err != nil {
		return err
	}
	if c.Stage == "" || !filepath.IsAbs(c.Stage) || filepath.Clean(c.Stage) != c.Stage {
		return trust(c.Stage, "staged root must be an absolute, lexically clean path")
	}
	if c.SetupUIDs == nil {
		c.SetupUIDs = IDs{}
	}
	if c.SetupGIDs == nil {
		c.SetupGIDs = IDs{}
	}
	if c.RuntimeUIDs == nil {
		c.RuntimeUIDs = IDs{}
	}
	if c.RuntimeGIDs == nil {
		c.RuntimeGIDs = IDs{}
	}
	c.SetupUIDs[0] = true
	euid := uint32(os.Geteuid())
	if !c.SetupUIDs[euid] {
		return trust(root, fmt.Sprintf("effective UID %d is not a declared setup principal", euid))
	}
	for id := range c.RuntimeUIDs {
		if c.SetupUIDs[id] {
			return trust(root, fmt.Sprintf("runtime UID %d is also a setup authority", id))
		}
	}
	for id := range c.RuntimeGIDs {
		if c.SetupGIDs[id] {
			return trust(root, fmt.Sprintf("runtime GID %d is also a writable setup group", id))
		}
	}
	if euid != 0 {
		if !c.Homebrew || (root != "/opt/homebrew/libexec/noise" && root != "/usr/local/libexec/noise") {
			if TestRoot == "" {
				return trust(root, "non-root publication requires a declared Homebrew root")
			}
		}
	}

	parent := filepath.Dir(root)
	if filepath.Dir(c.Stage) != parent || filepath.Base(c.Stage) == filepath.Base(root) {
		return trust(c.Stage, fmt.Sprintf("staged root must be a private direct child of %s", parent))
	}
	stageInfo, err := os.Lstat(c.Stage)
	if err != nil {
		return trust(c.Stage, "cannot inspect staged root: "+err.Error())
	}
	if !stageInfo.IsDir() || stageInfo.Mode()&os.ModeSymlink != 0 {
		return trust(c.Stage, "staged root must be a directory, not a symlink")
	}
	if err := validatePath(parent, c, true); err != nil {
		return err
	}
	if err := sameLocalFilesystem(parent, c.Stage); err != nil {
		return err
	}

	lockPath := filepath.Join(parent, ".noise.setup.lock")
	lock, err := openLock(lockPath)
	if err != nil {
		return trust(lockPath, "cannot open setup serialization lock: "+err.Error())
	}
	defer lock.Close()
	lockInfo, err := lock.Stat()
	if err != nil {
		return trust(lockPath, "cannot inspect setup serialization lock: "+err.Error())
	}
	if !lockInfo.Mode().IsRegular() {
		return trust(lockPath, "setup serialization lock is not a regular file")
	}
	if err := validateOwnerMode(lockPath, lockInfo, c, false); err != nil {
		return err
	}
	if err := flock(lock); err != nil {
		return trust(lockPath, "cannot serialize setup: "+err.Error())
	}
	defer funlock(lock)
	if err := testPoint("locked"); err != nil {
		return err
	}

	// Revalidate all mutable inputs after acquiring the publication lock.
	if err := validatePath(parent, c, true); err != nil {
		return err
	}
	if err := sameLocalFilesystem(parent, c.Stage); err != nil {
		return err
	}
	if err := validateStage(c); err != nil {
		return err
	}
	prior, err := validatePrior(root, c)
	if err != nil {
		return err
	}
	if err := testPoint("validated"); err != nil {
		return err
	}
	if err := freezeAndSync(c.Stage); err != nil {
		return trust(c.Stage, "cannot synchronize staged root: "+err.Error())
	}

	version, err := versionPath(parent, c.Stage)
	if err != nil {
		return trust(c.Stage, "cannot identify staged root: "+err.Error())
	}
	if err := os.Rename(c.Stage, version); err != nil {
		return trust(c.Stage, "cannot atomically place versioned root: "+err.Error())
	}
	published := false
	retainVersion := false
	defer func() {
		if !published && !retainVersion {
			discardVersion(version)
		}
	}()
	if err := testPoint("versioned"); err != nil {
		return err
	}

	temporary := filepath.Join(parent, ".noise-link-"+strconv.Itoa(os.Getpid()))
	_ = os.Remove(temporary)
	if err := os.Symlink(filepath.Base(version), temporary); err != nil {
		return trust(temporary, "cannot create publication link: "+err.Error())
	}
	defer os.Remove(temporary)
	if err := syncDir(parent); err != nil {
		return trust(parent, "cannot synchronize publication directory: "+err.Error())
	}
	if err := testPoint("linked"); err != nil {
		return err
	}
	if err := os.Rename(temporary, root); err != nil {
		return trust(root, "cannot atomically publish recorded root: "+err.Error())
	}
	if err := testPoint("swapped"); err != nil {
		if rollbackErr := rollback(root, prior, parent); rollbackErr != nil {
			retainVersion = true
			return trust(root, fmt.Sprintf("%v; rollback failed: %v", err, rollbackErr))
		}
		return err
	}
	if err := syncDir(parent); err != nil {
		if rollbackErr := rollback(root, prior, parent); rollbackErr != nil {
			retainVersion = true
			return trust(root, fmt.Sprintf("publication sync failed: %v; rollback failed: %v", err, rollbackErr))
		}
		return trust(root, "publication sync failed and prior installation was restored: "+err.Error())
	}
	published = true
	return nil
}

func validateRecordedRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return trust(root, "recorded root must be an absolute, lexically clean non-root path")
	}
	if TestRoot != "" {
		clean := filepath.Clean(TestRoot)
		if !filepath.IsAbs(clean) || !within(clean, root) {
			return trust(root, "test publication root is outside its build-selected authority boundary")
		}
		return nil
	}
	for _, unsafe := range []string{"/workspace", "/tmp", "/var/tmp"} {
		if root == unsafe || strings.HasPrefix(root, unsafe+string(os.PathSeparator)) {
			return trust(root, "recorded root is inside a workspace-writable or temporary tree")
		}
	}
	return nil
}

func validatePath(path string, c Config, directory bool) error {
	start := string(os.PathSeparator)
	if TestRoot != "" {
		start = filepath.Clean(TestRoot)
	}
	if !within(start, path) {
		return trust(path, "path is outside the setup authority boundary")
	}
	rel, _ := filepath.Rel(start, path)
	current := start
	if err := validateComponent(current, c, true); err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return trust(current, "cannot inspect path component: "+err.Error())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := validateOwnerMode(current, info, c, false); err != nil {
				return err
			}
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return trust(current, "cannot resolve symlink: "+err.Error())
			}
			if err := validatePath(resolved, c, true); err != nil {
				return err
			}
			continue
		}
		if err := validateComponent(current, c, true); err != nil {
			return err
		}
	}
	if directory {
		info, _ := os.Stat(path)
		if info == nil || !info.IsDir() {
			return trust(path, "path must be a directory")
		}
	}
	return nil
}

func validateComponent(path string, c Config, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return trust(path, "cannot inspect path component: "+err.Error())
	}
	if directory && !info.IsDir() {
		return trust(path, "path component is not a directory")
	}
	return validateOwnerMode(path, info, c, directory)
}

func validateOwnerMode(path string, info os.FileInfo, c Config, directory bool) error {
	uid, gid, ok := ownership(info)
	if !ok {
		return trust(path, "platform does not expose Unix ownership")
	}
	if !c.SetupUIDs[uid] {
		return trust(path, fmt.Sprintf("owner UID %d is not a declared setup principal", uid))
	}
	if c.RuntimeUIDs[uid] {
		return trust(path, fmt.Sprintf("runtime UID %d owns a replaceable component", uid))
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	perm := info.Mode().Perm()
	if perm&0o002 != 0 {
		return trust(path, "other-writable state is outside declared setup authority")
	}
	if perm&0o020 != 0 {
		if !c.SetupGIDs[gid] {
			return trust(path, fmt.Sprintf("writable GID %d is not a declared setup group", gid))
		}
		if c.RuntimeGIDs[gid] {
			return trust(path, fmt.Sprintf("runtime GID %d can modify installation state", gid))
		}
	}
	if directory && perm&0o111 == 0 {
		return trust(path, "directory is not searchable")
	}
	if extendedACL(path) {
		return trust(path, "extended ACL grants are not accepted by this setup surface")
	}
	return nil
}

func validateStage(c Config) error {
	stageInfo, err := os.Lstat(c.Stage)
	if err != nil {
		return trust(c.Stage, "cannot inspect staged root: "+err.Error())
	}
	if !stageInfo.IsDir() || stageInfo.Mode()&os.ModeSymlink != 0 {
		return trust(c.Stage, "staged root must be a directory, not a symlink")
	}
	if err := validatePath(c.Stage, c, true); err != nil {
		return err
	}
	info, _ := os.Lstat(c.Stage)
	if info.Mode().Perm()&0o077 != 0 {
		return trust(c.Stage, "staged root must be private (mode 0700 or stricter)")
	}
	entries, err := os.ReadDir(c.Stage)
	if err != nil {
		return trust(c.Stage, "cannot read staged root: "+err.Error())
	}
	seenTasks := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "noise-") || !libexec.ValidNoun(strings.TrimPrefix(name, "noise-")) {
			return trust(filepath.Join(c.Stage, name), "entry name is not noise-<valid non-reserved noun>")
		}
		if name == "noise-tasks" {
			seenTasks = true
		}
		path := filepath.Join(c.Stage, name)
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return trust(path, "cannot resolve plugin target: "+err.Error())
		}
		if !within(c.Stage, canonical) || canonical == c.Stage {
			return trust(path, "plugin target escapes the staged root")
		}
		target, err := os.Stat(canonical)
		if err != nil {
			return trust(path, "cannot inspect plugin target: "+err.Error())
		}
		if !target.Mode().IsRegular() {
			return trust(path, "plugin target is not a regular file")
		}
		if target.Mode().Perm()&0o111 == 0 {
			return trust(path, "plugin target is not executable")
		}
		if err := validateOwnerMode(canonical, target, c, false); err != nil {
			return err
		}
		if link, err := os.Lstat(path); err == nil && link.Mode()&os.ModeSymlink != 0 {
			if err := validateOwnerMode(path, link, c, false); err != nil {
				return err
			}
			target, err := os.Readlink(path)
			if err != nil {
				return trust(path, "cannot read plugin symlink: "+err.Error())
			}
			if filepath.IsAbs(target) {
				return trust(path, "absolute plugin symlink would not survive version publication")
			}
		}
	}
	if !seenTasks {
		return trust(c.Stage, "required plugin noise-tasks is absent")
	}
	return nil
}

func validatePrior(root string, c Config) (string, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", trust(root, "cannot inspect prior installation: "+err.Error())
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", trust(root, "prior recorded root is not an atomic version link")
	}
	if err := validateOwnerMode(root, info, c, false); err != nil {
		return "", err
	}
	target, err := os.Readlink(root)
	if err != nil {
		return "", trust(root, "cannot read prior version link: "+err.Error())
	}
	if filepath.IsAbs(target) || filepath.Dir(target) != "." || !strings.HasPrefix(target, ".noise-v-") {
		return "", trust(root, "prior version link does not name a sibling immutable tree")
	}
	resolved := filepath.Join(filepath.Dir(root), target)
	if err := validatePath(resolved, c, true); err != nil {
		return "", err
	}
	return target, nil
}

func freezeAndSync(stage string) error {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(stage, entry.Name())
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		if err := os.Chmod(canonical, 0o555); err != nil {
			return err
		}
		f, err := os.Open(canonical)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := os.Chmod(stage, 0o555); err != nil {
		return err
	}
	return syncDir(stage)
}

func versionPath(parent, stage string) (string, error) {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, entry := range entries {
		io.WriteString(h, entry.Name()+"\x00")
		path, err := filepath.EvalSymlinks(filepath.Join(stage, entry.Name()))
		if err != nil {
			return "", err
		}
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		if _, err = io.Copy(h, f); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
	}
	base := fmt.Sprintf(".noise-v-%x-%d", h.Sum(nil)[:8], time.Now().UnixNano())
	return filepath.Join(parent, base), nil
}

func rollback(root, prior, parent string) error {
	if err := testRollbackPoint("rollback-before-restore"); err != nil {
		return err
	}
	if prior == "" {
		if err := os.Remove(root); err != nil {
			return err
		}
	} else {
		tmp := filepath.Join(parent, ".noise-rollback-"+strconv.Itoa(os.Getpid()))
		_ = os.Remove(tmp)
		if err := os.Symlink(prior, tmp); err != nil {
			return err
		}
		defer os.Remove(tmp)
		if err := os.Rename(tmp, root); err != nil {
			return err
		}
	}
	if err := testRollbackPoint("rollback-after-restore"); err != nil {
		return err
	}
	return syncDir(parent)
}

func testRollbackPoint(point string) error {
	if TestRollbackPoint == "fail:"+point {
		return trust(libexec.Root, "build-selected publication failure at "+point)
	}
	return nil
}

func discardVersion(path string) {
	_ = os.Chmod(path, 0o700)
	_ = os.RemoveAll(path)
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func trust(path, condition string) error {
	if path == "" {
		path = "<empty>"
	}
	return fmt.Errorf("setup refused %s: %s", path, condition)
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func testPoint(point string) error {
	if TestPoint == "fail:"+point {
		return trust(libexec.Root, "build-selected publication failure at "+point)
	}
	if TestPoint != "pause:"+point {
		return nil
	}
	if TestMarker != "" {
		if err := os.WriteFile(TestMarker, []byte(point), 0o600); err != nil {
			return trust(TestMarker, "cannot write build-selected test marker: "+err.Error())
		}
	}
	for {
		time.Sleep(time.Second)
	}
}

func ParseIDList(values []string) (IDs, error) {
	out := IDs{}
	for _, value := range values {
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid numeric identity %q", value)
		}
		out[uint32(n)] = true
	}
	return out, nil
}

func ownership(info os.FileInfo) (uint32, uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return stat.Uid, stat.Gid, true
}
