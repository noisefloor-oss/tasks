package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// createFaultHook is a deterministic crash-injection point for the creation
// publication tests: "pre-publish" fires after the staged database is
// complete but before the publishing rename, "post-rename" right after it.
var createFaultHook = func(string) error { return nil }

// CreateWithOwner is the production creation entry point (SPEC §8.1/§8.2):
// it requires the canonical owner boundary and bootstraps the complete
// schema-version-2 store — DDL, fencing counter, and the minted
// familyId/ownerBoundary identity in one BEGIN IMMEDIATE transaction — on a
// same-directory temporary file, then checkpoints, fsyncs, and atomically
// publishes it with a no-replace rename. The authority path is therefore
// never visible at user_version 0 or without its family identity, and
// concurrent creators have exactly one winner: every loser gets an error
// wrapping os.ErrExist so it can adopt the winner's store.
func CreateWithOwner(path, ownerBoundary string) (*DB, error) {
	if ownerBoundary == "" {
		return nil, errors.New("create task store: canonical owner boundary is required")
	}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("create task store %s: not a regular file", path)
		}
		return nil, &storeExistsError{path}
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create task store directory: %w", err)
	}
	// One shared maintenance lock spans the whole creation: staging, the
	// no-replace publication, the directory fsync, and the returned handle's
	// lifetime (Close releases it). An exclusive offline holder can never
	// enter the staging-to-publication gap, and every failure below releases
	// the lock. The staged database and the final handle both open through
	// openUnlocked so this outer lock is the only one — there is no
	// double-lock gap between closing the staged handle and opening the
	// published authority.
	unlock, err := LockShared(dir)
	if err != nil {
		return nil, err
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	staged, err := os.CreateTemp(dir, ".tasks-create-")
	if err != nil {
		return nil, fmt.Errorf("stage task store creation: %w", err)
	}
	stagedPath := staged.Name()
	staged.Close()
	// open() re-creates the name exclusively with mode 0600; the reservation
	// only existed to allocate a unique same-directory name.
	os.Remove(stagedPath)
	cleanup := true
	defer func() {
		if cleanup {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				os.Remove(stagedPath + suffix)
			}
		}
	}()
	d, err := openUnlocked(stagedPath, true, ownerBoundary)
	if err != nil {
		return nil, err
	}
	if err := checkpointTruncate(d); err != nil {
		d.Close()
		return nil, err
	}
	if err := d.Close(); err != nil {
		return nil, err
	}
	if err := createFaultHook("pre-publish"); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(stagedPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	// The final private mode is set BEFORE the final fsync, so the durable
	// bytes are the final-mode bytes.
	if err := os.Chmod(stagedPath, 0o600); err != nil {
		return nil, err
	}
	if err := fsyncFile(stagedPath); err != nil {
		return nil, err
	}
	if err := renameNoReplace(stagedPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, &storeExistsError{path}
		}
		return nil, fmt.Errorf("publish task store %s: %w", path, err)
	}
	cleanup = false
	if err := createFaultHook("post-rename"); err != nil {
		return nil, err
	}
	if err := fsyncDir(dir); err != nil {
		return nil, err
	}
	// Open the published authority under the same outer lock: this handle
	// owns the lock from before staging through its Close, and it is
	// returned owner-configured and identity-validated.
	target, err := openUnlocked(path, false, "")
	if err != nil {
		return nil, err
	}
	target.unlock = unlock
	locked = false
	if err := target.SetOwnerBoundary(ownerBoundary); err != nil {
		target.Close()
		return nil, err
	}
	if err := target.CheckIdentity(ownerBoundary); err != nil {
		target.Close()
		return nil, err
	}
	return target, nil
}
