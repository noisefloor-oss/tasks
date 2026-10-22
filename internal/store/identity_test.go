package store

import (
	"database/sql"

	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// setMetaRow rewrites one meta row directly, simulating a damaged identity.
func setMetaRow(t *testing.T, path, key, value string, drop bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if drop {
		if _, err := db.Exec(`DELETE FROM meta WHERE key=?`, key); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
		t.Fatal(err)
	}
}

// makeV2Store creates the exact preceding released schema with identity.
func makeV2Store(t *testing.T, dir, owner string) string {
	t.Helper()
	path := filepath.Join(dir, "noise-tasks", "tasks.sqlite")
	d, err := CreateWithOwner(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	rewindStoreToV2(t, d)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// metaSnapshot reads the whole meta table for before/after comparison.
func metaSnapshot(t *testing.T, path string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		m[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return m
}

// An identity-bearing v2 store with a wrong stored owner boundary is refused by
// the normal-open gate before any read or write is served.
func TestPrepareIdentityWrongBoundaryFailsClosed(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	path := makeV2Store(t, dir, owner)
	other := filepath.Join(dir, "other")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	current, err := db.PrepareIdentity(other)
	if err == nil || !strings.Contains(err.Error(), "does not match this repository family") {
		t.Fatalf("PrepareIdentity(wrong) = (%v, %v), want boundary mismatch refusal", current, err)
	}
	if current {
		t.Fatal("mismatch reported as current schema")
	}
	// The stored identity is untouched.
	familyID, stored, err := db.Identity()
	if err != nil || familyID == "" || stored != owner {
		t.Fatalf("identity after refusal = (%q, %q, %v)", familyID, stored, err)
	}
}

// An identity-bearing v2 store with a partial, wholly missing, or malformed
// identity never passes the normal-open gate and is never repaired.
func TestPrepareIdentityDamagedV2PairsFailClosed(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	for _, tc := range []struct {
		name   string
		mutate func(path string)
		want   string
	}{
		{"partial family id only", func(p string) { setMetaRow(t, p, metaOwnerBoundary, "", true) }, "partial family identity"},
		{"partial owner only", func(p string) { setMetaRow(t, p, metaFamilyID, "", true) }, "partial family identity"},
		{"missing identity", func(p string) {
			setMetaRow(t, p, metaFamilyID, "", true)
			setMetaRow(t, p, metaOwnerBoundary, "", true)
		}, "no family identity"},
		{"malformed family id", func(p string) { setMetaRow(t, p, metaFamilyID, "not-a-uuid", false) }, "not a valid UUID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := makeV2Store(t, dir, owner)
			tc.mutate(path)
			before := metaSnapshot(t, path)
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.PrepareIdentity(owner); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PrepareIdentity = %v, want %q", err, tc.want)
			}
			// No repair happened: the meta table is byte-identical to the
			// damaged fixture the refusal saw.
			db.Close()
			after := metaSnapshot(t, path)
			if len(after) != len(before) {
				t.Fatalf("meta rows changed across the refusal: %v -> %v", before, after)
			}
			for k, v := range before {
				if after[k] != v {
					t.Fatalf("meta key %q changed across the refusal: %q -> %q", k, v, after[k])
				}
			}
		})
	}
}

// A complete v2 identity passes the gate but reports that schema upgrade is
// still needed.
func TestPrepareIdentityCompletePairPasses(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	path := makeV2Store(t, dir, owner)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	current, err := db.PrepareIdentity(owner)
	if err != nil {
		t.Fatal(err)
	}
	if current {
		t.Fatal("v2 identity-bearing store reported as current v3 schema")
	}
}

// A released v1 store passes the gate as older-schema: configured but not
// validated, still readable, and upgraded with a minted identity by its
// first normal mutation.
func TestPrepareIdentityV1ConfiguresThenFirstMutationUpgrades(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	path := makeV1Store(t, dir)

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	current, err := db.PrepareIdentity(owner)
	if err != nil {
		t.Fatal(err)
	}
	if current {
		t.Fatal("v1 store reported as current schema")
	}
	// Still readable before any mutation.
	if _, _, err := db.Identity(); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTag("main"); err != nil {
		t.Fatalf("first mutation: %v", err)
	}
	if v, verr := db.userVersion(); verr != nil || v != SchemaVersion {
		t.Fatalf("user_version %d (%v), want %d", v, verr, SchemaVersion)
	}
	familyID, stored, err := db.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := uuid.Parse(familyID); perr != nil {
		t.Fatalf("family id %q invalid: %v", familyID, perr)
	}
	if stored != owner {
		t.Fatalf("owner boundary %q, want %q", stored, owner)
	}
	// After the upgrade the store is a current-schema one: a fresh open
	// with a different derived boundary now fails the gate closed.
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.PrepareIdentity(filepath.Join(dir, "other")); err == nil ||
		!strings.Contains(err.Error(), "does not match this repository family") {
		t.Fatalf("post-upgrade PrepareIdentity(other) = %v, want mismatch refusal", err)
	}
}
