package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// The §8.2 immutable family identity recorded in the meta table. Once stored,
// no normal mutation, restore, or later schema upgrade changes either value.
const (
	metaFamilyID      = "familyId"
	metaOwnerBoundary = "ownerBoundary"
)

// EnsureIdentity validates the immutable familyId/ownerBoundary pair and
// records it when the store does not yet carry it (SPEC §8.2: setup, init,
// and the first identity-bearing upgrade of a released schema mint familyId
// from a cryptographically secure random UUID and record the owner boundary).
// ownerBoundary must be the canonical §8.1 owner boundary; it is required
// only when minting. A partial identity, an invalid familyId UUID, or a
// supplied boundary that disagrees with the stored one fails closed — half an
// identity is never repaired and a mismatch is never silently accepted.
// Existing values are never changed.
func (d *DB) EnsureIdentity(ownerBoundary string) error {
	if err := d.SetOwnerBoundary(ownerBoundary); err != nil {
		return err
	}
	return d.immediate(func(t *tx) error {
		return t.ensureIdentity(ownerBoundary)
	})
}

// ensureIdentity is the transactional pair check: validate when both rows
// exist, fail on any partial or corrupt state, mint only when wholly absent.
func (t *tx) ensureIdentity(ownerBoundary string) error {
	var familyID, storedBoundary string
	ferr := t.QueryRow(`SELECT value FROM meta WHERE key=?`, metaFamilyID).Scan(&familyID)
	if ferr != nil && !errors.Is(ferr, sql.ErrNoRows) {
		return fmt.Errorf("read family identity: %w", ferr)
	}
	oerr := t.QueryRow(`SELECT value FROM meta WHERE key=?`, metaOwnerBoundary).Scan(&storedBoundary)
	if oerr != nil && !errors.Is(oerr, sql.ErrNoRows) {
		return fmt.Errorf("read owner boundary: %w", oerr)
	}
	switch {
	case ferr == nil && oerr == nil:
		if _, err := uuid.Parse(familyID); err != nil {
			return fmt.Errorf("task store family id %q is not a valid UUID", familyID)
		}
		if ownerBoundary != "" && ownerBoundary != storedBoundary {
			return fmt.Errorf("task store owner boundary %q does not match this repository family %q", storedBoundary, ownerBoundary)
		}
		return nil
	case ferr == nil || oerr == nil:
		return errors.New("task store has a partial family identity; refusing to repair it")
	}
	if ownerBoundary == "" {
		return errors.New("task store has no family identity and no owner boundary was supplied to mint one")
	}
	if _, err := t.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, metaFamilyID, uuid.NewString()); err != nil {
		return fmt.Errorf("mint family id: %w", err)
	}
	if _, err := t.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, metaOwnerBoundary, ownerBoundary); err != nil {
		return fmt.Errorf("record owner boundary: %w", err)
	}
	return nil
}

// CheckIdentity validates the stored identity pair against the supplied
// canonical owner boundary without minting, repairing, or upgrading: backup
// create verifies the store already carries its §8.2 identity instead of
// becoming the fallback that repairs or mints one.
func (d *DB) CheckIdentity(ownerBoundary string) error {
	familyID, stored, err := d.Identity()
	if err != nil {
		return err
	}
	switch {
	case familyID == "" && stored == "":
		return errors.New("task store has no family identity; run any mutating noise-tasks command to upgrade it first")
	case familyID == "" || stored == "":
		return errors.New("task store has a partial family identity; refusing to snapshot it")
	}
	if _, err := uuid.Parse(familyID); err != nil {
		return fmt.Errorf("task store family id %q is not a valid UUID", familyID)
	}
	if ownerBoundary != "" && stored != ownerBoundary {
		return fmt.Errorf("task store owner boundary %q does not match this repository family %q", stored, ownerBoundary)
	}
	return nil
}

// PrepareIdentity is the normal-open identity gate (SPEC §8.2): it
// configures the canonical owner boundary this handle would upgrade and
// mint with and, when the store already carries the current schema,
// validates the stored familyId/ownerBoundary pair immediately — before
// any read or write is served. A wrong boundary, a partial or wholly
// missing identity, or a malformed familyId UUID fails closed. An older
// pre-identity schema (v1) is only configured: it stays readable and its
// first normal mutation upgrades and mints atomically. currentSchema
// distinguishes the two cases so a minting command (setup) can upgrade a
// v1 store deliberately while an ordinary open never silently repairs a
// current-schema store that lost its identity.
func (d *DB) PrepareIdentity(ownerBoundary string) (currentSchema bool, err error) {
	if err := d.SetOwnerBoundary(ownerBoundary); err != nil {
		return false, err
	}
	version, err := d.userVersion()
	if err != nil {
		return false, fmt.Errorf("read task store schema version: %w", err)
	}
	if version >= identitySchemaVersion {
		if err := d.CheckIdentity(ownerBoundary); err != nil {
			return false, err
		}
		return version >= SchemaVersion, nil
	}
	return false, nil
}

// Identity reads the immutable family identity. Empty strings report a store
// that predates identity recording (a v1 database no v2 mutation or backup
// has upgraded yet).
func (d *DB) Identity() (familyID, ownerBoundary string, err error) {
	err = d.deferred(func(t *tx) error {
		var ferr, oerr error
		ferr = t.QueryRow(`SELECT value FROM meta WHERE key=?`, metaFamilyID).Scan(&familyID)
		if ferr != nil && !errors.Is(ferr, sql.ErrNoRows) {
			return fmt.Errorf("read family id: %w", ferr)
		}
		oerr = t.QueryRow(`SELECT value FROM meta WHERE key=?`, metaOwnerBoundary).Scan(&ownerBoundary)
		if oerr != nil && !errors.Is(oerr, sql.ErrNoRows) {
			return fmt.Errorf("read owner boundary: %w", oerr)
		}
		return nil
	})
	return familyID, ownerBoundary, err
}

// ValidFamilyID reports whether v is a real UUID under the same strict
// identity rule the store enforces on its recorded familyId (SPEC §8.2):
// manifest and payload family ids are held to it verbatim.
func ValidFamilyID(v string) bool {
	_, err := uuid.Parse(v)
	return err == nil
}

// requireIdentity reads the identity inside a transaction and fails closed
// when it is absent: the §8.5 backup/restore digests cover the singleton
// family/owner record, so a store without identity cannot be snapshotted.
func (t *tx) requireIdentity() (familyID, ownerBoundary string, err error) {
	if err := t.QueryRow(`SELECT value FROM meta WHERE key=?`, metaFamilyID).Scan(&familyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", errors.New("task store has no family identity; run any mutating noise-tasks command to upgrade it first")
		}
		return "", "", fmt.Errorf("read family id: %w", err)
	}
	if err := t.QueryRow(`SELECT value FROM meta WHERE key=?`, metaOwnerBoundary).Scan(&ownerBoundary); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", errors.New("task store has no owner boundary; run any mutating noise-tasks command to upgrade it first")
		}
		return "", "", fmt.Errorf("read owner boundary: %w", err)
	}
	return familyID, ownerBoundary, nil
}
