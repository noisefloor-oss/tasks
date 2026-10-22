package store

import (
	"fmt"
	"os"
	"path/filepath"
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

// Sync fsyncs the database file and its containing directory. In WAL mode
// recent commits live in the -wal sidecar, so callers must checkpoint
// (PRAGMA wal_checkpoint) first when they need the main file alone to be
// durable; the §8.6 publication path does exactly that.
func (d *DB) Sync() error {
	if err := d.db.Ping(); err != nil {
		return err
	}
	f, err := os.Open(d.path)
	if err != nil {
		return fmt.Errorf("open task store %s for fsync: %w", d.path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync task store %s: %w", d.path, err)
	}
	f.Close()
	dir, err := os.Open(filepath.Dir(d.path))
	if err != nil {
		return fmt.Errorf("open task store directory for fsync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return fmt.Errorf("fsync task store directory: %w", err)
	}
	return dir.Close()
}
