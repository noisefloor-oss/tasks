package store

import (
	"fmt"
	"os"
	"strings"
)

// BackupTo writes a database-consistent copy to destPath using VACUUM INTO
// (SPEC §8.5: a backup goes through SQLite's backup interface or VACUUM INTO,
// never a byte copy of the main file while a writer may hold the WAL). The
// destination must not exist. The copy carries mode 0600.
func (d *DB) BackupTo(destPath string) error {
	if _, err := os.Lstat(destPath); err == nil {
		return fmt.Errorf("backup destination %s already exists", destPath)
	} else if !os.IsNotExist(err) {
		return err
	}
	// VACUUM INTO takes a filename expression; inline it as a quoted literal.
	if _, err := d.db.Exec(`VACUUM INTO '` + strings.ReplaceAll(destPath, `'`, `''`) + `'`); err != nil {
		return fmt.Errorf("backup task store to %s: %w", destPath, err)
	}
	if err := os.Chmod(destPath, 0o600); err != nil {
		return fmt.Errorf("set backup mode on %s: %w", destPath, err)
	}
	return nil
}

// IntegrityCheck runs PRAGMA integrity_check, used by the §8.5 restore
// validation and tests.
func (d *DB) IntegrityCheck() error {
	var result string
	if err := d.db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("integrity check on %s: %w", d.path, err)
	}
	if result != "ok" {
		return fmt.Errorf("integrity check on %s: %s", d.path, result)
	}
	return nil
}
