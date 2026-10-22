package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func storeUserVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func requireCompleteStore(t *testing.T, path, owner string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("published store missing: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("published store mode = %v", info.Mode())
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("published store has %s sidecar: %v", suffix, err)
		}
	}
	if v := storeUserVersion(t, path); v != SchemaVersion {
		t.Fatalf("published store user_version = %d", v)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open published store: %v", err)
	}
	defer d.Close()
	familyID, storedOwner, err := d.Identity()
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if familyID == "" || storedOwner != owner {
		t.Fatalf("identity = (%q, %q), want familyId minted for %q", familyID, storedOwner, owner)
	}
}

func TestCreateWithOwnerRequiresOwnerBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	if _, err := CreateWithOwner(path, ""); err == nil {
		t.Fatal("empty owner boundary accepted")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal created state: %v", err)
	}
}

func TestCreateWithOwnerPublishesCompleteStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.sqlite")
	d, err := CreateWithOwner(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	requireCompleteStore(t, path, dir)
}

func TestCreateWithOwnerLoserGetsErrExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.sqlite")
	winner, err := CreateWithOwner(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	familyID, _, _ := winner.Identity()
	winner.Close()
	loser, err := CreateWithOwner(path, dir)
	if loser != nil {
		loser.Close()
		t.Fatal("second create returned a handle")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("loser error = %v, want os.ErrExist", err)
	}
	requireCompleteStore(t, path, dir)
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if id, _, _ := reopened.Identity(); id != familyID {
		t.Fatalf("loser disturbed the winner: familyId %q then %q", familyID, id)
	}
}

func TestCreateWithOwnerCrashBeforePublishLeavesNoAuthority(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.sqlite")
	original := createFaultHook
	createFaultHook = func(point string) error {
		if point != "pre-publish" {
			t.Fatalf("fault point = %q", point)
		}
		return errors.New("simulated crash")
	}
	t.Cleanup(func() { createFaultHook = original })
	if _, err := CreateWithOwner(path, dir); err == nil {
		t.Fatal("crash injection ignored")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-publish crash left an authority: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Base(entry.Name()) != "maintenance.lock" {
			t.Fatalf("pre-publish crash left residue: %s", entry.Name())
		}
	}
	// The next attempt succeeds cleanly.
	createFaultHook = func(string) error { return nil }
	d, err := CreateWithOwner(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	requireCompleteStore(t, path, dir)
}

func TestCreateWithOwnerCrashAfterRenameLeavesCompleteStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.sqlite")
	original := createFaultHook
	createFaultHook = func(point string) error {
		if point == "post-rename" {
			return errors.New("simulated crash")
		}
		return nil
	}
	t.Cleanup(func() { createFaultHook = original })
	if _, err := CreateWithOwner(path, dir); err == nil {
		t.Fatal("crash injection ignored")
	}
	// The rename already published: the authority is complete and adoptable.
	requireCompleteStore(t, path, dir)
}

func TestCreateWithOwnerConcurrentCreatorsHaveOneWinner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.sqlite")
	const creators = 8
	errs := make([]error, creators)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range creators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := CreateWithOwner(path, dir)
			if d != nil {
				d.Close()
			}
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()
	winners := 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, os.ErrExist):
		default:
			t.Fatalf("concurrent create error = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want 1 (errs=%v)", winners, errs)
	}
	requireCompleteStore(t, path, dir)
}

// TestCreateWithOwnerExclusiveCannotEnterStagingGap proves the shared
// maintenance lock covers the whole staging-to-publication window: while
// creation sits between the complete staged database and the publishing
// rename, an exclusive offline holder (restore) cannot acquire the lock,
// and it can again once creation finished and its handle closed.
func TestCreateWithOwnerExclusiveCannotEnterStagingGap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.sqlite")
	inGap := make(chan struct{})
	leaveGap := make(chan struct{})
	original := createFaultHook
	createFaultHook = func(op string) error {
		if op == "pre-publish" {
			close(inGap)
			<-leaveGap
		}
		return nil
	}
	t.Cleanup(func() { createFaultHook = original })
	done := make(chan error, 1)
	go func() {
		d, err := CreateWithOwner(path, dir)
		if d != nil {
			err = d.Close()
		}
		done <- err
	}()
	<-inGap
	if rel, err := LockExclusive(dir); err == nil {
		rel()
		t.Fatal("exclusive maintenance lock acquired inside the staging-to-publication gap")
	}
	close(leaveGap)
	if err := <-done; err != nil {
		t.Fatalf("create in gap test: %v", err)
	}
	rel, err := LockExclusive(dir)
	if err != nil {
		t.Fatalf("exclusive lock after completed creation: %v", err)
	}
	rel()
	requireCompleteStore(t, path, dir)
}

func TestOpenRefusesUninitializedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE foreign_table (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if v := storeUserVersion(t, path); v != 0 {
		t.Fatalf("fixture user_version = %d, want 0", v)
	}
	_, err = Open(path)
	var uninit *UninitializedError
	if !errors.As(err, &uninit) {
		t.Fatalf("open of uninitialized store = %v, want UninitializedError", err)
	}
}
