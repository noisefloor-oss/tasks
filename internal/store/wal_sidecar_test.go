package store

import (
	"os"
	"path/filepath"
	"testing"
)

func orphanCommittedWAL(t *testing.T, path, owner, title string, withSHM bool) string {
	t.Helper()
	db, err := CreateWithOwner(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	familyID, _, err := db.Identity()
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	addTask(t, db, "main", title)
	tasks, err := db.ListTasks("main")
	if err != nil || len(tasks) != 1 || tasks[0].Title != title {
		db.Close()
		t.Fatalf("committed WAL fixture tasks = %+v, %v", tasks, err)
	}
	wal, err := os.ReadFile(path + "-wal")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if len(wal) <= 32 {
		db.Close()
		t.Fatalf("committed WAL size = %d, want frames after the header", len(wal))
	}
	var shm []byte
	if withSHM {
		shm, err = os.ReadFile(path + "-shm")
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", wal, 0o600); err != nil {
		t.Fatal(err)
	}
	if withSHM {
		if err := os.WriteFile(path+"-shm", shm, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return familyID
}

func requireTaskTitles(t *testing.T, db *DB, tag string, want ...string) {
	t.Helper()
	tasks, err := db.ListTasks(tag)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != len(want) {
		t.Fatalf("tasks = %+v, want titles %v", tasks, want)
	}
	for i, title := range want {
		if tasks[i].Title != title {
			t.Fatalf("tasks = %+v, want titles %v", tasks, want)
		}
	}
}

func TestCreateWithOwnerDiscardsOrphanedCommittedWAL(t *testing.T) {
	for _, withSHM := range []bool{false, true} {
		name := "wal-only"
		if withSHM {
			name = "wal-and-shm"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, StoreFile)
			oldFamily := orphanCommittedWAL(t, path, dir, "stale creation row", withSHM)

			db, err := CreateWithOwner(path, dir)
			if err != nil {
				t.Fatal(err)
			}
			familyID, storedOwner, err := db.Identity()
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			if familyID == "" || familyID == oldFamily || storedOwner != dir {
				db.Close()
				t.Fatalf("fresh identity = (%q, %q), old family %q", familyID, storedOwner, oldFamily)
			}
			requireTaskTitles(t, db, "main")
			addTask(t, db, "main", "fresh creation row")
			requireTaskTitles(t, db, "main", "fresh creation row")
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestoreAbsentTargetDiscardsOrphanedCommittedWAL(t *testing.T) {
	for _, withSHM := range []bool{false, true} {
		name := "wal-only"
		if withSHM {
			name = "wal-and-shm"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			_, bundle := restoreFixture(t, dir)
			manifest := verifyOrDie(t, bundle)
			target := filepath.Join(dir, "recovery", "noise-tasks", StoreFile)
			oldFamily := orphanCommittedWAL(t, target, dir, "stale restore row", withSHM)

			result, err := Restore(bundle, target, RestoreOptions{RequestID: "wal-1", OwnerBoundary: dir})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Adopted {
				t.Fatal("absent-target restore did not report adoption")
			}
			db, err := Open(target)
			if err != nil {
				t.Fatal(err)
			}
			familyID, storedOwner, err := db.Identity()
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			if familyID != manifest.FamilyID || familyID == oldFamily || storedOwner != dir {
				db.Close()
				t.Fatalf("restored identity = (%q, %q), want bundle %q, old family %q", familyID, storedOwner, manifest.FamilyID, oldFamily)
			}
			requireTaskTitles(t, db, "main", "first", "second")
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestApplyMigrationDiscardsOrphanedCommittedWAL(t *testing.T) {
	for _, withSHM := range []bool{false, true} {
		name := "wal-only"
		if withSHM {
			name = "wal-and-shm"
		}
		t.Run(name, func(t *testing.T) {
			storeDir, _, commonDir, owner, bundle, plan := applyFamily(t)
			target := filepath.Join(storeDir, StoreFile)
			oldFamily := orphanCommittedWAL(t, target, owner, "stale migration row", withSHM)

			if _, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan)); err != nil {
				t.Fatal(err)
			}
			db, err := Open(target)
			if err != nil {
				t.Fatal(err)
			}
			familyID, storedOwner, err := db.Identity()
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			wantFamily := LegacyFamilyUUID(commonDir, owner)
			if familyID != wantFamily || familyID == oldFamily || storedOwner != owner {
				db.Close()
				t.Fatalf("migrated identity = (%q, %q), want %q, old family %q", familyID, storedOwner, wantFamily, oldFamily)
			}
			requireTaskTitles(t, db, "main", "One", "Two")
			requireTaskTitles(t, db, "aux", "Aux")
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
